package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 定时唤醒的用例分三层：
//   - 纯函数：时间解析、文案清洗、提示词、退避；
//   - 执行器：用内存假数据面驱动「成功 / 会话忙 / 限流 / 失败重试 / 次数用尽 / 被别人领走」；
//   - 工具：元数据与「不在会话里」的拒绝。
// 真库上的扫描与领取见 wakeup_db_test.go（AGENT_WAKEUP_IT=1 才跑）。

// ---------- 纯函数 ----------

func TestParseWakeupRunAt(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	ok, err := parseWakeupRunAt("2026-10-26T09:00:00+08:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC); !ok.Equal(want) {
		t.Fatalf("解析结果 = %v，期望 %v", ok.UTC(), want)
	}
	if _, err := parseWakeupRunAt("2026-10-25 09:00", now); err != nil {
		t.Fatalf("本地时间写法应可用：%v", err)
	}
	for _, bad := range []string{"", "明天早上", "2026-10-20T09:00:00+08:00", "2030-01-01T00:00:00+08:00"} {
		if _, err := parseWakeupRunAt(bad, now); err == nil {
			t.Fatalf("%q 应被拒", bad)
		}
	}
}

func TestCleanWakeupText(t *testing.T) {
	prompt, title, err := cleanWakeupText("  提醒我喝水  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "提醒我喝水" || title != "提醒我喝水" {
		t.Fatalf("归一结果不对：%q / %q", prompt, title)
	}
	if _, _, err := cleanWakeupText("   ", "标题"); err == nil {
		t.Fatal("空说明应被拒")
	}
	long := strings.Repeat("字", wakeupMaxPromptRunes+1)
	if _, _, err := cleanWakeupText(long, ""); err == nil {
		t.Fatal("超长说明应被拒")
	}
	_, title, _ = cleanWakeupText("正文", strings.Repeat("标", wakeupMaxTitleRunes+5))
	if n := len([]rune(title)); n > wakeupMaxTitleRunes {
		t.Fatalf("标题应截断到 %d 字，实际 %d", wakeupMaxTitleRunes, n)
	}
}

func TestWakeupPrompt(t *testing.T) {
	runAt := time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local)
	w := model.AgentWakeup{Title: "检查构建", Prompt: "看 CI 结果", RunAt: runAt}
	out := wakeupPrompt(w, time.Date(2026, 10, 25, 9, 0, 30, 0, time.Local))
	for _, want := range []string{"定时唤醒", "2026-10-25 09:00", "检查构建", "看 CI 结果", "不要编造"} {
		if !strings.Contains(out, want) {
			t.Fatalf("提示词缺少 %q：\n%s", want, out)
		}
	}
}

func TestWakeupBackoff(t *testing.T) {
	const base, max = time.Minute, 20 * time.Minute
	if got := wakeupBackoff(1, base, max); got != time.Minute {
		t.Fatalf("第 1 次 = %v", got)
	}
	if got := wakeupBackoff(3, base, max); got != 4*time.Minute {
		t.Fatalf("第 3 次 = %v", got)
	}
	if got := wakeupBackoff(9, base, max); got != max {
		t.Fatalf("应压到上限，实际 %v", got)
	}
	if got := wakeupBackoff(0, 0, 0); got != time.Minute {
		t.Fatalf("非法参数应回落 1 分钟，实际 %v", got)
	}
}

// ---------- 执行器 ----------

type failCall struct {
	id      uint
	cause   string
	retryAt *time.Time
}

type fakeWakeupSource struct {
	due      []model.AgentWakeup
	lost     map[uint]bool // 模拟被别人先领走
	done     []uint
	deferred []time.Time
	failed   []failCall
	lease    time.Duration
}

func (f *fakeWakeupSource) Due(now time.Time, limit int) ([]model.AgentWakeup, error) {
	return append([]model.AgentWakeup(nil), f.due...), nil
}

func (f *fakeWakeupSource) Claim(id uint, now, leaseUntil time.Time) (model.AgentWakeup, bool, error) {
	if f.lost[id] {
		return model.AgentWakeup{}, false, nil
	}
	f.lease = leaseUntil.Sub(now)
	for _, w := range f.due {
		if w.Id == id {
			return w, true, nil
		}
	}
	return model.AgentWakeup{}, false, nil
}

func (f *fakeWakeupSource) Done(id uint, at time.Time) error {
	f.done = append(f.done, id)
	return nil
}

func (f *fakeWakeupSource) Fail(w model.AgentWakeup, cause string, retryAt *time.Time, maxAttempts int) error {
	f.failed = append(f.failed, failCall{id: w.Id, cause: cause, retryAt: retryAt})
	return nil
}

func (f *fakeWakeupSource) Defer(id uint, until time.Time) error {
	f.deferred = append(f.deferred, until)
	return nil
}

func testWakeup(id uint) model.AgentWakeup {
	return model.AgentWakeup{
		Id: id, UserID: "u1", ConversationID: id * 10, Title: "标题", Prompt: "做事",
		RunAt: time.Date(2026, 10, 24, 20, 0, 0, 0, time.Local), Status: model.WakeupPending,
	}
}

func TestWakeupRunnerFiresDue(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	src := &fakeWakeupSource{due: []model.AgentWakeup{testWakeup(1), testWakeup(2)}}
	var fired []uint
	r := newWakeupRunner(func(_ context.Context, w model.AgentWakeup) error {
		fired = append(fired, w.Id)
		return nil
	})
	r.src = src
	r.now = func() time.Time { return now }

	rep := r.RunOnce(context.Background())
	if rep.Scanned != 2 || rep.Fired != 2 || rep.Failed != 0 || rep.Deferred != 0 {
		t.Fatalf("报告不对：%+v", rep)
	}
	if len(fired) != 2 || len(src.done) != 2 {
		t.Fatalf("应执行并标记 2 条：fired=%v done=%v", fired, src.done)
	}
	if src.lease != wakeupDefaultLease {
		t.Fatalf("租约 = %v", src.lease)
	}
}

func TestWakeupRunnerBusyDefersWithoutCountingFailure(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	src := &fakeWakeupSource{due: []model.AgentWakeup{testWakeup(3)}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error { return errWakeupBusy })
	r.src = src
	r.now = func() time.Time { return now }

	rep := r.RunOnce(context.Background())
	if rep.Deferred != 1 || rep.Failed != 0 {
		t.Fatalf("忙时应推迟而不是计失败：%+v", rep)
	}
	if len(src.failed) != 0 {
		t.Fatalf("不该记失败：%+v", src.failed)
	}
	if want := now.Add(wakeupBusyDefer); len(src.deferred) != 1 || !src.deferred[0].Equal(want) {
		t.Fatalf("推迟时间 = %v，期望 %v", src.deferred, want)
	}
}

func TestWakeupRunnerRateLimitedDefers(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	src := &fakeWakeupSource{due: []model.AgentWakeup{testWakeup(4)}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error { return errWakeupRateLimited })
	r.src = src
	r.now = func() time.Time { return now }

	rep := r.RunOnce(context.Background())
	if rep.Deferred != 1 || rep.Failed != 0 {
		t.Fatalf("限流应推迟：%+v", rep)
	}
	if want := now.Add(wakeupRateDefer); !src.deferred[0].Equal(want) {
		t.Fatalf("推迟时间 = %v，期望 %v", src.deferred[0], want)
	}
}

func TestWakeupRunnerFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	w := testWakeup(5)
	src := &fakeWakeupSource{due: []model.AgentWakeup{w}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error { return errors.New("模型不可用") })
	r.src = src
	r.now = func() time.Time { return now }
	r.backoffBase = 2 * time.Minute
	r.maxAttempts = 3

	rep := r.RunOnce(context.Background())
	if rep.Failed != 1 {
		t.Fatalf("应计 1 次失败：%+v", rep)
	}
	call := src.failed[0]
	if call.retryAt == nil || !call.retryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("退避时间不对：%v", call.retryAt)
	}
	if !strings.Contains(call.cause, "模型不可用") {
		t.Fatalf("失败原因没留下：%q", call.cause)
	}
}

func TestWakeupRunnerFailureExhausted(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	w := testWakeup(6)
	w.Attempts = 2 // 这次是第 3 次
	src := &fakeWakeupSource{due: []model.AgentWakeup{w}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error { return errors.New("还是不行") })
	r.src = src
	r.now = func() time.Time { return now }
	r.maxAttempts = 3

	r.RunOnce(context.Background())
	if src.failed[0].retryAt != nil {
		t.Fatalf("次数用尽不该再排重试：%v", src.failed[0].retryAt)
	}
}

func TestWakeupRunnerSkipsLostClaim(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	src := &fakeWakeupSource{due: []model.AgentWakeup{testWakeup(7)}, lost: map[uint]bool{7: true}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error {
		t.Fatal("没领到就不该执行")
		return nil
	})
	r.src = src
	r.now = func() time.Time { return now }

	rep := r.RunOnce(context.Background())
	if rep.Scanned != 1 || rep.Fired+rep.Failed+rep.Deferred != 0 {
		t.Fatalf("被别人领走应静默跳过：%+v", rep)
	}
}

func TestWakeupRunnerSkipsWhenPreviousRoundStillRunning(t *testing.T) {
	src := &fakeWakeupSource{due: []model.AgentWakeup{testWakeup(8)}}
	r := newWakeupRunner(func(context.Context, model.AgentWakeup) error { return nil })
	r.src = src
	r.runMu.Lock()
	rep := r.RunOnce(context.Background())
	r.runMu.Unlock()
	if rep.Scanned != 0 || len(src.done) != 0 {
		t.Fatalf("上一轮没跑完时应直接跳过：%+v", rep)
	}
}

// ---------- 工具 ----------

func TestWakeupToolMetadata(t *testing.T) {
	for _, name := range []string{"schedule_wakeup", "cancel_wakeup", "list_wakeups"} {
		meta := toolsys.LookupMeta(name)
		if !meta.Known {
			t.Fatalf("%s 没登记元数据", name)
		}
	}
	if !toolsys.LookupMeta("schedule_wakeup").Confirm {
		t.Fatal("schedule_wakeup 在 confirm 档应先问用户")
	}
	if toolsys.LookupMeta("cancel_wakeup").Confirm || toolsys.LookupMeta("list_wakeups").Confirm {
		t.Fatal("取消/列表不该要确认")
	}
	if !toolsys.LookupMeta("list_wakeups").ParallelOK {
		t.Fatal("列表是只读，应允许并发")
	}
}

func TestScheduleWakeupToolRejectsOutsideConversation(t *testing.T) {
	tool := scheduleWakeupTool{userID: "u1"}
	raw, _ := json.Marshal(map[string]any{"run_at": "2026-10-25T09:00:00+08:00", "prompt": "提醒我"})
	res, err := tool.Execute(context.Background(), raw, &agent.Exec{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "不在一个会话") {
		t.Fatalf("没有会话 id 时应拒绝：%+v", res)
	}
}

func TestCancelWakeupToolRequiresID(t *testing.T) {
	tool := cancelWakeupTool{userID: "u1"}
	res, err := tool.Execute(context.Background(), []byte(`{}`), &agent.Exec{SessionID: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "id") {
		t.Fatalf("缺 id 应报错：%+v", res)
	}
}

// ---------- HTTP 校验 ----------

func TestCreateWakeupRejectsBadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/wakeups", func(c *gin.Context) { c.Set(ctxUserIDKey, "u1"); createWakeupAPI(c) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/wakeups", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，实际 %d：%s", w.Code, w.Body.String())
	}
}
