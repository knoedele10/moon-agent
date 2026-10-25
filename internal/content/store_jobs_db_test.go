package content

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"blog-my/internal/model"
)

// 这些用例走真 MySQL（openTestStore 在没设 CONTENT_IT=1 时跳过），
// 覆盖纯逻辑测不到的部分：到期扫描、租约条件更新、导出台账推进、失败退避与跨用户隔离。

func mustDraft(t *testing.T, s *Store, userID, title string) model.ContentDraft {
	t.Helper()
	d, err := s.CreateDraft(userID, title, "测试选题", "正文", []string{"测试"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustSchedule(t *testing.T, s *Store, userID string, draftID uint, planned time.Time) model.ContentPublication {
	t.Helper()
	p, err := s.SavePublication(userID, model.ContentPublication{
		DraftID: draftID, Channel: "wechat", PlannedAt: &planned, Status: PubPlanned,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreDueAndClaim(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	d := mustDraft(t, s, u, "到点导出")
	p := mustSchedule(t, s, u, d.Id, time.Now().Add(-time.Minute))

	due, err := s.DuePublications(u, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Id != p.Id {
		t.Fatalf("应扫到 1 条到期排期，实际 %+v", due)
	}
	if n, err := s.CountDuePublications(u, time.Now()); err != nil || n != 1 {
		t.Fatalf("待导出计数 = %d, err=%v", n, err)
	}

	lease := time.Now().Add(5 * time.Minute)
	claimed, ok, err := s.ClaimPublication(p.Id, time.Now(), lease)
	if err != nil || !ok {
		t.Fatalf("第一次领取应成功：ok=%v err=%v", ok, err)
	}
	if claimed.NextAttemptAt == nil || claimed.NextAttemptAt.Before(time.Now()) {
		t.Fatalf("领取后应写入未来租约，实际 %v", claimed.NextAttemptAt)
	}
	if _, ok, _ := s.ClaimPublication(p.Id, time.Now(), lease); ok {
		t.Fatal("同一租约期内第二次领取应失败（防重复导出）")
	}
	if due, _ := s.DuePublications(u, time.Now(), 10); len(due) != 0 {
		t.Fatalf("租约期内不该再次到期，实际 %+v", due)
	}

	exported, err := s.MarkExported(claimed, `D:\exports\pub-1.md`)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Status != PubExported || exported.Url != `D:\exports\pub-1.md` {
		t.Fatalf("台账未推进到 exported：%+v", exported)
	}
	if exported.NextAttemptAt != nil {
		t.Fatal("导出成功后应清掉租约")
	}
	if err := CanTransitionPub(exported.Status, PubPublished); err != nil {
		t.Fatalf("导出后应能人工标记已发布：%v", err)
	}
	if due, _ := s.DuePublications(u, time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("已导出的排期不该再被扫到：%+v", due)
	}
}

func TestStoreExportFailureBackoff(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	d := mustDraft(t, s, u, "失败退避")
	p := mustSchedule(t, s, u, d.Id, time.Now().Add(-time.Minute))

	now := time.Now()
	retryAt := now.Add(2 * time.Minute)
	failed, err := s.RecordExportFailure(p, "磁盘满了", &retryAt, 3)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != PubPlanned || failed.Retries != 1 {
		t.Fatalf("第一次失败应保持 planned 并累计次数：%+v", failed)
	}
	if !strings.Contains(failed.Error, "磁盘满了") {
		t.Fatalf("失败原因应写进台账：%q", failed.Error)
	}
	if failed.NextAttemptAt == nil || failed.NextAttemptAt.Before(retryAt.Add(-time.Second)) {
		t.Fatalf("应记下退避时间，实际 %v", failed.NextAttemptAt)
	}
	if due, _ := s.DuePublications(u, now, 10); len(due) != 0 {
		t.Fatal("退避期内不该被扫到")
	}
	if due, _ := s.DuePublications(u, retryAt.Add(time.Second), 10); len(due) != 1 {
		t.Fatalf("退避到期后应重新被扫到，实际 %+v", due)
	}

	// 第 2、3 次失败：第 3 次达到上限，落到 failed 且不再排重试。
	retry2 := now.Add(4 * time.Minute)
	if _, err := s.RecordExportFailure(p, "还是失败", &retry2, 3); err != nil {
		t.Fatal(err)
	}
	last, err := s.RecordExportFailure(p, "第三次仍失败", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if last.Status != PubFailed || last.Retries != 3 {
		t.Fatalf("次数用尽应落到 failed：%+v", last)
	}
	if last.NextAttemptAt != nil {
		t.Fatal("已 failed 的记录不该再带退避时间")
	}
}

func TestStorePublicationCrossUserIsolation(t *testing.T) {
	s := openTestStore(t)
	u1, u2 := testUser(t)+"-a", testUser(t)+"-b"
	defer cleanupUser(t, s, u1)
	defer cleanupUser(t, s, u2)

	d := mustDraft(t, s, u1, "别人看不到")
	p := mustSchedule(t, s, u1, d.Id, time.Now().Add(-time.Minute))

	if _, err := s.GetPublication(u2, p.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户取台账应是 ErrNotFound，实际 %v", err)
	}
	if n, err := s.CountDuePublications(u2, time.Now()); err != nil || n != 0 {
		t.Fatalf("u2 的待导出数应为 0，实际 %d err=%v", n, err)
	}
	if due, _ := s.DuePublications(u2, time.Now(), 10); len(due) != 0 {
		t.Fatalf("u2 不该扫到 u1 的排期：%+v", due)
	}
	if _, err := s.MarkExported(model.ContentPublication{Id: p.Id, UserID: u2}, "x.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户推进台账应被拒，实际 %v", err)
	}
	// 全局口径（后台调度）能扫到它，但那是 worker 专用，不面向用户。
	if due, _ := s.DuePublications("", time.Now(), 100); len(due) == 0 {
		t.Fatal("后台调度的全局扫描应能看到到期排期")
	}
}

// 真库上跑一整轮：到期 → 领取 → 渲染落盘 → 台账推进 exported，再跑一轮不再重复处理。
func TestRunnerEndToEndWithRealStore(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	d := mustDraft(t, s, u, "端到端导出")
	p := mustSchedule(t, s, u, d.Id, time.Now().Add(-time.Minute))

	dir := t.TempDir()
	r := NewRunner(s, PackagePublisher{Dir: dir})
	rep := r.RunOnce(context.Background(), u)
	if rep.Exported != 1 || rep.Failed != 0 || rep.Due != 1 {
		t.Fatalf("整轮结果不对：%+v", rep)
	}

	got, err := s.GetPublication(u, p.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != PubExported {
		t.Fatalf("台账应推进到 exported，实际 %q", got.Status)
	}
	if !strings.HasPrefix(got.Url, dir) {
		t.Fatalf("台账应记下成品包路径，实际 %q", got.Url)
	}
	body, err := os.ReadFile(got.Url)
	if err != nil {
		t.Fatalf("成品包没落盘：%v", err)
	}
	for _, want := range []string{"端到端导出", "正文", "渠道：`wechat`"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("成品包缺少 %q：\n%s", want, body)
		}
	}

	if again := r.RunOnce(context.Background(), u); again.Due != 0 || again.Exported != 0 {
		t.Fatalf("已导出的排期不该被重复处理：%+v", again)
	}
	if items := r.Recent(u, 10); len(items) != 1 || items[0].PublicationID != p.Id {
		t.Fatalf("运行记录不对：%+v", items)
	}
}
