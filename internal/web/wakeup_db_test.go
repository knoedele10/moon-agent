package web

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"blog-my/internal/model"
)

// 真库用例：验证扫描/领取/记账这几条 SQL 与租约语义。
// 默认跳过，设 AGENT_WAKEUP_IT=1 并用专用库才跑：
//
//	$env:AGENT_WAKEUP_IT='1'; $env:DB_NAME='blog_my_content_it'; go test ./internal/web -run TestWakeupStore -v

func openWakeupTestDB(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENT_WAKEUP_IT") != "1" {
		t.Skip("需要真 MySQL：设 AGENT_WAKEUP_IT=1 才跑")
	}
	if err := model.InitDB(); err != nil {
		t.Skipf("初始化数据库失败，跳过：%v", err)
	}
}

func wakeupTestUser(t *testing.T) string {
	t.Helper()
	return "wakeup-it-" + time.Now().Format("150405.000000000")
}

func wakeupTestConversation(t *testing.T, userID string) model.Conversation {
	t.Helper()
	conv := model.Conversation{UserID: userID, Title: "唤醒测试", Model: "deepseek-chat", Kind: "work"}
	if err := model.DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.DB.Where("user_id = ?", userID).Delete(&model.AgentWakeup{})
		model.DB.Where("id = ?", conv.Id).Delete(&model.Conversation{})
	})
	return conv
}

func mustCreateWakeup(t *testing.T, userID string, convID uint, runAt time.Time) model.AgentWakeup {
	t.Helper()
	w, err := createWakeup(userID, convID, "测试唤醒", "检查一下", runAt, "user")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func findWakeup(rows []model.AgentWakeup, id uint) (model.AgentWakeup, bool) {
	for _, w := range rows {
		if w.Id == id {
			return w, true
		}
	}
	return model.AgentWakeup{}, false
}

func TestWakeupStoreClaimAndFinish(t *testing.T) {
	openWakeupTestDB(t)
	userID := wakeupTestUser(t)
	conv := wakeupTestConversation(t, userID)
	w := mustCreateWakeup(t, userID, conv.Id, time.Now().Add(-time.Minute))

	now := time.Now()
	due, err := dueWakeups(now, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findWakeup(due, w.Id); !ok {
		t.Fatal("到点的唤醒应出现在扫描结果里")
	}

	lease := now.Add(10 * time.Minute)
	claimed, ok, err := claimWakeup(w.Id, now, lease)
	if err != nil || !ok {
		t.Fatalf("第一次领取应成功：ok=%v err=%v", ok, err)
	}
	if claimed.NextAttemptAt == nil || claimed.NextAttemptAt.Before(now) {
		t.Fatalf("领取后应写入未来租约，实际 %v", claimed.NextAttemptAt)
	}
	if _, ok, _ := claimWakeup(w.Id, now, lease); ok {
		t.Fatal("租约期内不应被再次领取")
	}
	if err := finishWakeup(w.Id, now); err != nil {
		t.Fatal(err)
	}
	got, err := findWakeupByID(userID, w.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.WakeupDone || got.FiredAt == nil || got.NextAttemptAt != nil {
		t.Fatalf("完成后的台账不对：%+v", got)
	}
	// 已完成的不该再被扫到。
	due, _ = dueWakeups(now.Add(time.Hour), 50)
	if _, ok := findWakeup(due, w.Id); ok {
		t.Fatal("已完成的唤醒不该再出现")
	}
}

func TestWakeupStoreFailureAndCancel(t *testing.T) {
	openWakeupTestDB(t)
	userID := wakeupTestUser(t)
	conv := wakeupTestConversation(t, userID)
	w := mustCreateWakeup(t, userID, conv.Id, time.Now().Add(-time.Minute))

	now := time.Now()
	retryAt := now.Add(2 * time.Minute)
	if err := failWakeup(w, "模型不可用", &retryAt, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := findWakeupByID(userID, w.Id)
	if got.Status != model.WakeupPending || got.Attempts != 1 || got.NextAttemptAt == nil {
		t.Fatalf("第一次失败应保持 pending 并退避：%+v", got)
	}
	if _, ok := findWakeup(mustDue(t, now), w.Id); ok {
		t.Fatal("退避期内不该被扫到")
	}
	if _, ok := findWakeup(mustDue(t, retryAt.Add(time.Second)), w.Id); !ok {
		t.Fatal("退避到期后应重新出现")
	}

	// 次数用尽。
	got.Attempts = 2
	if err := failWakeup(got, "第三次仍失败", nil, 3); err != nil {
		t.Fatal(err)
	}
	got, _ = findWakeupByID(userID, w.Id)
	if got.Status != model.WakeupFailed || got.NextAttemptAt != nil {
		t.Fatalf("次数用尽应落 failed：%+v", got)
	}

	// 取消：只对 pending 生效。
	w2 := mustCreateWakeup(t, userID, conv.Id, now.Add(time.Hour))
	canceled, err := cancelUserWakeup(userID, w2.Id)
	if err != nil || canceled.Status != model.WakeupCanceled {
		t.Fatalf("取消失败：%+v err=%v", canceled, err)
	}
	if _, err := cancelUserWakeup(userID, w2.Id); err == nil {
		t.Fatal("重复取消应报错")
	}
	if _, err := cancelUserWakeup("someone-else", w2.Id); !errors.Is(err, errWakeupNotFound) {
		t.Fatalf("跨用户取消应是 not found，实际 %v", err)
	}
}

func TestWakeupStoreIsolationAndList(t *testing.T) {
	openWakeupTestDB(t)
	u1, u2 := wakeupTestUser(t)+"-a", wakeupTestUser(t)+"-b"
	conv := wakeupTestConversation(t, u1)
	w := mustCreateWakeup(t, u1, conv.Id, time.Now().Add(time.Hour))

	list, err := listUserWakeups(u1, conv.Id, model.WakeupPending, 10)
	if err != nil || len(list) != 1 || list[0].Id != w.Id {
		t.Fatalf("u1 应看到自己的唤醒：%+v err=%v", list, err)
	}
	if other, _ := listUserWakeups(u2, 0, "", 10); len(other) != 0 {
		t.Fatalf("u2 不该看到 u1 的唤醒：%+v", other)
	}
	if n, err := countPendingWakeups(conv.Id); err != nil || n != 1 {
		t.Fatalf("待唤醒计数 = %d, err=%v", n, err)
	}
}

// 真库 + 真执行器：会话没选模型时唤醒会失败并记一次尝试，不会真的去调模型。
func TestWakeupRunnerWithRealStore(t *testing.T) {
	openWakeupTestDB(t)
	userID := wakeupTestUser(t)
	conv := wakeupTestConversation(t, userID) // Model 故意留空
	w := mustCreateWakeup(t, userID, conv.Id, time.Now().Add(-time.Minute))

	r := newWakeupRunner(fireWakeup)
	r.now = time.Now
	rep := r.RunOnce(context.Background())
	if rep.Failed < 1 {
		t.Fatalf("没有模型的会话应唤醒失败：%+v", rep)
	}
	got, err := findWakeupByID(userID, w.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 || got.Status != model.WakeupPending {
		t.Fatalf("失败一次后应保持 pending 并记尝试次数：%+v", got)
	}
	if got.Error == "" {
		t.Fatal("失败原因应写进台账")
	}
}

func findWakeupByID(userID string, id uint) (model.AgentWakeup, error) {
	var w model.AgentWakeup
	err := model.DB.Where("id = ? AND user_id = ?", id, userID).First(&w).Error
	return w, err
}

func mustDue(t *testing.T, now time.Time) []model.AgentWakeup {
	t.Helper()
	rows, err := dueWakeups(now, 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
