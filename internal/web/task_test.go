package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	sqlite "github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 长任务运行时的端到端测试：临时 SQLite + 确定性脚本模型 + 真实调度器。
// 不访问任何真实模型供应商。

type taskCall struct {
	Kind   string // exec / judge / compact
	Req    llm.ChatRequest
	System string
	Last   llm.Message
	N      int // 本类请求的序号（从 1 开始）
}

type taskHarness struct {
	t        *testing.T
	db       *gorm.DB
	user     string
	main     model.Conversation
	mu       sync.Mutex
	calls    map[string]int
	execReqs []llm.ChatRequest
	inflight int32
	maxIn    int32
}

func (h *taskHarness) count(kind string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[kind]
}

var taskToolSeq int64

func tcall(name string, args any) []llm.Event {
	raw, _ := json.Marshal(args)
	id := fmt.Sprintf("call-%d", atomic.AddInt64(&taskToolSeq, 1))
	return []llm.Event{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: id, Name: name, Arguments: string(raw)}}, {Type: llm.EventDone}}
}

func ttext(s string) []llm.Event {
	return []llm.Event{{Type: llm.EventDelta, Content: s}, {Type: llm.EventDone}}
}

func submitAll(task model.AgentTask, summary string) []llm.Event {
	var items []map[string]string
	for _, c := range taskCriteria(task) {
		items = append(items, map[string]string{"criterion": c.ID, "evidence": "已核实：" + c.Text})
	}
	return tcall("task_submit", map[string]any{"summary": summary, "evidence": items})
}

func judgePass(taskCall) []llm.Event { return ttext(`{"verdict":"pass","reason":"证据与记录一致"}`) }

func newTaskHarness(t *testing.T, script func(h *taskHarness, c taskCall) []llm.Event) *taskHarness {
	t.Helper()
	t.Setenv("AGENT_EVOLVE", "0")
	t.Setenv("AGENT_ENGINE", "legacy")
	t.Setenv("AGENT_MODE", "confirm")
	t.Setenv("AGENT_COMPACT", "0")
	t.Setenv("AGENT_FS_ROOT", "")
	t.Setenv("AGENT_TASK_TICK_MS", "20")
	t.Setenv("AGENT_TASK_CONTINUE_DELAY_MS", "1")
	t.Setenv("AGENT_TASK_BACKOFF_MS", "5")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "task.db")+"?_pragma=busy_timeout(5000)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Conversation{}, &model.Message{}, &model.AgentSubagentRun{}, &model.AgentSetting{}, &model.User{}, &model.AgentUsage{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(model.TaskModels()...); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	h := &taskHarness{t: t, db: db, user: "task-owner", calls: map[string]int{}}
	h.main = model.Conversation{UserID: h.user, Model: "task-test", Kind: KindWork}
	db.Create(&h.main)

	oldStreamer := agent.Streamer()
	agent.SetStreamer(func(ctx context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
		c := taskCall{Req: req}
		if len(req.Messages) > 0 {
			c.System = req.Messages[0].Content
			c.Last = req.Messages[len(req.Messages)-1]
		}
		switch {
		case strings.Contains(c.System, "严格的验收员"):
			c.Kind = "judge"
		case strings.Contains(c.System, "你在压缩"):
			c.Kind = "compact"
		default:
			c.Kind = "exec"
		}
		h.mu.Lock()
		h.calls[c.Kind]++
		c.N = h.calls[c.Kind]
		if c.Kind == "exec" {
			h.execReqs = append(h.execReqs, req)
		}
		h.mu.Unlock()
		if c.Kind == "compact" {
			return evChan(ttext("摘要：已完成若干步骤。")), nil
		}
		if c.Kind == "exec" {
			n := atomic.AddInt32(&h.inflight, 1)
			defer atomic.AddInt32(&h.inflight, -1)
			for {
				m := atomic.LoadInt32(&h.maxIn)
				if n <= m || atomic.CompareAndSwapInt32(&h.maxIn, m, n) {
					break
				}
			}
		}
		evs := script(h, c)
		if evs == nil {
			<-ctx.Done() // 模拟一个长时间运行的模型调用，直到被取消
			return nil, ctx.Err()
		}
		return evChan(evs), nil
	})
	StartTaskScheduler(context.Background())
	t.Cleanup(func() {
		StopTaskScheduler()
		agent.SetStreamer(oldStreamer)
		model.DB = oldDB
		sqlDB.Close()
	})
	return h
}

func evChan(evs []llm.Event) <-chan llm.Event {
	ch := make(chan llm.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch
}

func (h *taskHarness) create(spec taskSpec) model.AgentTask {
	h.t.Helper()
	task, err := createTask(spec, taskCreateOptions{UserID: h.user, MainConversationID: h.main.Id, Model: "task-test", Kind: KindWork, Mode: ModeConfirm, AllowCommandChecks: true})
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *taskHarness) waitStatus(id uint, want ...string) model.AgentTask {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var task model.AgentTask
	for time.Now().Before(deadline) {
		task, _ = loadTask(id)
		for _, w := range want {
			if task.Status == w {
				return task
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var events []model.AgentTaskEvent
	h.db.Where("task_id = ?", id).Find(&events)
	h.t.Fatalf("任务状态 %s（stop=%s err=%s），等待 %v 超时；事件：%+v", task.Status, task.StopReason, task.LastError, want, events)
	return task
}

// mainNotices 等主对话通知落库（状态迁移先于通知写入，短暂轮询避免竞态）。
func (h *taskHarness) mainNotices() string {
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := h.mainNoticesNow()
		if s != "" || time.Now().After(deadline) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *taskHarness) mainNoticesNow() string {
	var rows []model.Message
	h.db.Where("conversation_id = ? AND role = ?", h.main.Id, subagentMessageRole).Find(&rows)
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Content + "\n")
	}
	return b.String()
}

func simpleSpec() taskSpec {
	return taskSpec{Title: "生成报告", Goal: "整理数据并生成报告", Constraints: "只用中文",
		Criteria: []taskCriterion{{Text: "报告包含三个章节"}, {Text: "数据来源已注明"}}}
}

// 1. 正常完成：执行、验证、交付，全程无须催促。
func TestTaskNormalCompletion(t *testing.T) {
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		switch c.N {
		case 1:
			return tcall("task_update_progress", map[string]any{"progress": "报告已写好", "done_steps": []string{"写报告"}})
		default:
			return submitAll(task, "报告已生成")
		}
	})
	task = h.create(simpleSpec())
	done := h.waitStatus(task.Id, model.TaskDone)
	if done.RoundsUsed != 1 || done.StopReason != "verified" {
		t.Fatalf("应一轮完成并经验收：rounds=%d stop=%s", done.RoundsUsed, done.StopReason)
	}
	if h.count("judge") != 2 {
		t.Fatalf("每条验收条件都应单独核对，judge=%d", h.count("judge"))
	}
	if v := taskLastVerification(done); v == nil || v.Overall != verdictPass || len(v.Items) != 2 {
		t.Fatalf("验收结果缺失：%+v", v)
	}
	if !strings.Contains(h.mainNotices(), "已通过全部验收条件") {
		t.Fatal("主对话没有收到交付报告")
	}
	if !strings.Contains(done.DoneSteps, "写报告") {
		t.Fatal("进度没有持久化")
	}
}

// 2. 提前收尾：模型停下来写计划，系统发现未完成并续跑（不需要用户说「继续」）。
func TestTaskEarlyStopAutoContinues(t *testing.T) {
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		if strings.Contains(c.Last.Content, "自动续跑") {
			return submitAll(task, "已完成")
		}
		return ttext("下一步：我将整理数据，然后生成报告。")
	})
	task = h.create(simpleSpec())
	done := h.waitStatus(task.Id, model.TaskDone)
	if done.RoundsUsed != 2 {
		t.Fatalf("应在第 2 轮自动续跑后完成，rounds=%d", done.RoundsUsed)
	}
	var userMsgs []model.Message
	h.db.Where("conversation_id = ? AND role = ?", done.ExecConversationID, "user").Order("id asc").Find(&userMsgs)
	if len(userMsgs) != 2 || !strings.Contains(userMsgs[1].Content, "只写了计划") {
		t.Fatalf("续跑消息应由系统投递且指出只写了计划：%+v", userMsgs)
	}
	var ev int64
	h.db.Model(&model.AgentTaskEvent{}).Where("task_id = ? AND kind = ?", task.Id, "auto_continue").Count(&ev)
	if ev != 1 {
		t.Fatalf("应记录一次自动续跑，实际 %d", ev)
	}
}

// 3. 验证失败：检查不通过，失败项交回原执行对话自动修复并重新验证。
func TestTaskVerifyFailureRepairs(t *testing.T) {
	var fixed atomic.Bool
	oldRunner := taskCommandRunner
	taskCommandRunner = func(ctx context.Context, _ model.AgentTask, c taskCheck) (bool, string) {
		if fixed.Load() {
			return true, "PASS"
		}
		return false, "FAIL: TestReport 断言失败"
	}
	t.Cleanup(func() { taskCommandRunner = oldRunner })
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		if strings.Contains(c.Last.Content, "验收未通过") {
			if !strings.Contains(c.Last.Content, "TestReport") {
				t.Errorf("失败原因没有交回执行对话：%s", c.Last.Content)
			}
			fixed.Store(true)
		}
		return submitAll(task, "完成")
	})
	spec := simpleSpec()
	spec.Criteria = []taskCriterion{{Text: "测试通过", Method: criterionCommand, Check: &taskCheck{Program: "go", Args: []string{"test", "./..."}}}, {Text: "报告已更新"}}
	task = h.create(spec)
	done := h.waitStatus(task.Id, model.TaskDone)
	if done.VerifyAttempts != 1 || done.RoundsUsed != 2 {
		t.Fatalf("应验收失败一次后修复：attempts=%d rounds=%d", done.VerifyAttempts, done.RoundsUsed)
	}
	// 同一执行对话（原对话修正，不新开对话）。
	var convs int64
	h.db.Model(&model.Conversation{}).Where("user_id = ?", h.user).Count(&convs)
	if convs != 2 {
		t.Fatalf("修复应发生在原执行对话里，会话数 %d", convs)
	}
}

// 3b. 「模型说完成了」不算完成：没有证据的提交被宿主驳回。
func TestTaskModelClaimIsNotCompletion(t *testing.T) {
	t.Setenv("AGENT_TASK_MAX_VERIFY_ATTEMPTS", "2")
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return ttext(`{"verdict":"fail","reason":"只有声明，没有可核对的事实"}`)
		}
		return tcall("task_submit", map[string]any{"summary": "完成了", "evidence": []map[string]string{
			{"criterion": "c1", "evidence": "完成了"}, {"criterion": "c2", "evidence": "完成了"}}})
	})
	task := h.create(simpleSpec())
	failed := h.waitStatus(task.Id, model.TaskFailed)
	if failed.StopReason != "verify_exhausted" || failed.VerifyAttempts != 2 {
		t.Fatalf("空口完成应被驳回直至失败：%s attempts=%d", failed.StopReason, failed.VerifyAttempts)
	}
}

// 4. 上下文压缩：多次压缩后仍遵守原始约束，已完成步骤不丢。
func TestTaskCompactionKeepsConstraints(t *testing.T) {
	var task model.AgentTask
	big := strings.Repeat("执行记录与工具输出。", 120)
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		if c.N >= 9 {
			return submitAll(task, "完成")
		}
		if c.Last.Role == llm.RoleUser {
			return tcall("task_update_progress", map[string]any{"progress": big, "done_steps": []string{fmt.Sprintf("步骤%d", c.N)}})
		}
		return ttext(big)
	})
	t.Setenv("AGENT_COMPACT", "1")
	t.Setenv("AGENT_MODEL_CONTEXT_TOKENS", "task-test=3000")
	spec := simpleSpec()
	spec.Constraints = "必须使用 Go 语言；禁止修改 README.md"
	spec.MaxRounds = 10
	task = h.create(spec)
	done := h.waitStatus(task.Id, model.TaskDone)
	if h.count("compact") < 2 {
		t.Fatalf("长任务应多次压缩，实际 %d 次", h.count("compact"))
	}
	h.mu.Lock()
	reqs := append([]llm.ChatRequest(nil), h.execReqs...)
	h.mu.Unlock()
	for i, r := range reqs {
		sys := r.Messages[0].Content
		if !strings.Contains(sys, "禁止修改 README.md") || !strings.Contains(sys, "整理数据并生成报告") {
			t.Fatalf("第 %d 次请求丢失原始目标或约束", i+1)
		}
	}
	last := reqs[len(reqs)-1].Messages[0].Content
	if !strings.Contains(last, "已完成步骤（不要重复执行）") || !strings.Contains(last, "步骤1") {
		t.Fatal("压缩后已完成步骤未注入")
	}
	var compactedRounds int64
	h.db.Model(&model.AgentTaskRound{}).Where("task_id = ? AND compacted > 0", done.Id).Count(&compactedRounds)
	if compactedRounds == 0 {
		t.Fatal("没有任何一轮发生入口压缩")
	}
}

// 4b. 循环内压缩阈值跟随模型窗口。
func TestTaskContextWindowLookup(t *testing.T) {
	t.Setenv("AGENT_MODEL_CONTEXT_TOKENS", "my-model=5000")
	t.Setenv("AGENT_CONTEXT_TOKENS", "")
	if modelContextTokens("my-model-v2") != 5000 || modelContextTokens("provider/claude-x") != 200000 || modelContextTokens("unknown") != 32000 {
		t.Fatal("模型窗口查找不正确")
	}
	if compactTokenTrigger("my-model") != 2500 {
		t.Fatal("入口压缩应在窗口一半触发")
	}
}

// 5. 中途重启：恢复原任务，先核对现场，再继续执行；不重复投递、不重复外部操作。
func TestTaskRestartReconciles(t *testing.T) {
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		if !strings.Contains(c.System, "恢复说明") || !strings.Contains(c.System, "结果未知") {
			t.Errorf("重启后的轮次没有先核对现场的说明")
		}
		return submitAll(task, "核对后完成")
	})
	StopTaskScheduler() // 模拟进程停止：先建任务、伪造「跑到一半」的现场
	task = h.create(simpleSpec())
	if _, err := deliverTaskOutbox(task); err != nil {
		t.Fatal(err)
	}
	journal, _ := json.Marshal([]taskJournalEntry{
		{Name: "write", Args: `{"path":"report.md"}`, Status: "ok"},
		{Name: "run_command", Args: `{"commands":[{"program":"go"}]}`, Status: "started"},
	})
	h.db.Create(&model.AgentTaskRound{TaskID: task.Id, No: 1, Status: model.TaskRoundRunning, Journal: string(journal), StartedAt: time.Now()})
	h.db.Model(&model.AgentTask{}).Where("id = ?", task.Id).Updates(map[string]any{"rounds_used": 1, "lease_owner": "dead-process", "lease_until": time.Now().Add(time.Hour)})

	StartTaskScheduler(context.Background()) // 「重启」：启动对账 + 续跑
	done := h.waitStatus(task.Id, model.TaskDone)
	var r1 model.AgentTaskRound
	h.db.Where("task_id = ? AND no = 1", task.Id).First(&r1)
	if r1.Status != model.TaskRoundInterrupted {
		t.Fatalf("中断轮次应标为 interrupted：%s", r1.Status)
	}
	if done.RoundsUsed != 2 {
		t.Fatalf("应从第 2 轮继续：%d", done.RoundsUsed)
	}
	var goals int64
	h.db.Model(&model.Message{}).Where("conversation_id = ? AND content LIKE ?", task.ExecConversationID, "【任务】%").Count(&goals)
	if goals != 1 {
		t.Fatalf("任务说明被重复投递：%d", goals)
	}
}

// 5b. 核对轮里，重启前已成功的风险操作不会被原样再执行一次。
type countingTool struct{ n *int32 }

func (countingTool) Name() string               { return "write" }
func (countingTool) Description() string        { return "" }
func (countingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (c countingTool) Execute(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error) {
	atomic.AddInt32(c.n, 1)
	return agent.Result{Content: "ok"}, nil
}

func TestTaskReconcileGuardBlocksDuplicateSideEffect(t *testing.T) {
	if !riskyTool("write") {
		t.Skip("write 不是风险工具")
	}
	var n int32
	args := `{"path":"report.md"}`
	st := &taskRoundState{priorOK: map[string]bool{taskCallKey("write", args): true}}
	db, _ := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "g.db")), &gorm.Config{})
	db.AutoMigrate(model.TaskModels()...)
	old := model.DB
	model.DB = db
	defer func() {
		model.DB = old
		if s, err := db.DB(); err == nil {
			s.Close()
		}
	}()
	tool := taskJournalTool{inner: countingTool{n: &n}, st: st}
	res, _ := tool.Execute(context.Background(), json.RawMessage(args), &agent.Exec{})
	if !res.IsError || n != 0 {
		t.Fatal("重复的外部操作没有被拦截")
	}
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"path":"other.md"}`), &agent.Exec{})
	if res.IsError || n != 1 {
		t.Fatal("不同参数的操作被误拦")
	}
}

// 6. 人工接管：缺少登录时暂停等待人工，补充后继续。
func TestTaskHumanTakeover(t *testing.T) {
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		if strings.Contains(c.Last.Content, "【用户补充】") {
			return submitAll(task, "登录后完成")
		}
		return tcall("task_block", map[string]any{"reason": "需要登录后台账号才能导出数据", "kind": "login"})
	})
	task = h.create(simpleSpec())
	waiting := h.waitStatus(task.Id, model.TaskWaitingHuman)
	if !strings.Contains(waiting.Blocker, "登录") || !strings.Contains(h.mainNotices(), "需要人工处理") {
		t.Fatal("等待人工的原因没有展示给用户")
	}
	time.Sleep(100 * time.Millisecond)
	if h.count("exec") != 1 {
		t.Fatalf("等待人工期间不应继续执行：%d", h.count("exec"))
	}
	if _, err := applyTaskAction(waiting, "input", taskActionInput{Message: "已登录，账号 ops"}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(task.Id, model.TaskDone)
	if done.RoundsUsed != 2 {
		t.Fatalf("补充后应继续一轮完成：%d", done.RoundsUsed)
	}
}

// 7a. 取消：取消后不再启动新步骤。
func TestTaskCancelStopsNewSteps(t *testing.T) {
	started := make(chan struct{}, 1)
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "exec" {
			select {
			case started <- struct{}{}:
			default:
			}
			return nil // 阻塞直到被取消
		}
		return judgePass(c)
	})
	task := h.create(simpleSpec())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("任务没有开始执行")
	}
	cur, _ := loadTask(task.Id)
	if _, err := applyTaskAction(cur, "cancel", taskActionInput{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(task.Id, model.TaskCanceled)
	time.Sleep(200 * time.Millisecond)
	if h.count("exec") != 1 {
		t.Fatalf("取消后又启动了新步骤：%d", h.count("exec"))
	}
	var r model.AgentTaskRound
	h.db.Where("task_id = ?", task.Id).First(&r)
	if r.Status != model.TaskRoundInterrupted {
		t.Fatalf("被取消的轮次应记为 interrupted：%s", r.Status)
	}
}

// 7b. 限额：预算耗尽明确标记未完成。
func TestTaskBudgetExhausted(t *testing.T) {
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		return ttext("接下来我将继续分析。")
	})
	spec := simpleSpec()
	spec.MaxRounds = 2
	task := h.create(spec)
	out := h.waitStatus(task.Id, model.TaskBudgetExhausted)
	if out.RoundsUsed != 2 || out.StopReason != "budget_rounds" || !strings.Contains(out.Blocker, "未完成") {
		t.Fatalf("预算耗尽标记不明确：%+v", out)
	}
	if !strings.Contains(h.mainNotices(), "未完成") {
		t.Fatal("主对话没有被告知未完成")
	}
	// 追加预算后可重试。
	if _, err := applyTaskAction(out, "retry", taskActionInput{ExtraRounds: 1}); err != nil {
		t.Fatal(err)
	}
	again := h.waitStatus(task.Id, model.TaskBudgetExhausted)
	if again.RoundsUsed != 3 {
		t.Fatalf("追加 1 轮预算后应再跑 1 轮：%d", again.RoundsUsed)
	}
}

// 7c. 临时故障退避重试；重复失败停止且原因可查。
func TestTaskTransientRetryThenFail(t *testing.T) {
	t.Setenv("AGENT_TASK_MAX_FAILURES", "3")
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		// 引擎内部会对同一次模型调用重试一次；前两次都失败才会落到任务级退避。
		if c.N <= 2 {
			return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("upstream 503 service unavailable")}}
		}
		return submitAll(task, "完成")
	})
	task = h.create(simpleSpec())
	done := h.waitStatus(task.Id, model.TaskDone)
	if done.ConsecutiveFailures != 0 {
		t.Fatal("成功后连续失败计数应清零")
	}
	var retries int64
	h.db.Model(&model.AgentTaskEvent{}).Where("task_id = ? AND kind = ? AND message LIKE ?", task.Id, "retry", "%临时故障%").Count(&retries)
	if retries != 1 {
		t.Fatalf("临时故障应退避重试一次：%d", retries)
	}
}

func TestTaskRepeatedFailureStops(t *testing.T) {
	t.Setenv("AGENT_TASK_MAX_FAILURES", "3")
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("upstream 503 service unavailable")}}
	})
	task := h.create(simpleSpec())
	out := h.waitStatus(task.Id, model.TaskFailed)
	if out.StopReason != "repeated_failure" || !strings.Contains(out.LastError, "503") || out.ConsecutiveFailures != 3 {
		t.Fatalf("重复失败应停止且可查原因：%+v", out)
	}
}

// 8. 并发与隔离：同一执行对话不会同时跑两轮；不能跨用户访问。
func TestTaskConcurrencyAndIsolation(t *testing.T) {
	var task model.AgentTask
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event {
		if c.Kind == "judge" {
			return judgePass(c)
		}
		time.Sleep(20 * time.Millisecond)
		if c.N < 4 {
			return ttext("下一步继续。")
		}
		return submitAll(task, "完成")
	})
	StopTaskScheduler()
	task = h.create(simpleSpec())
	// 多个调度方同时抢同一个任务：最多一个拿到租约。
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if claimTaskLease(task.Id, fmt.Sprintf("owner-%d", i), time.Minute) {
				atomic.AddInt32(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("租约被 %d 个调度方同时拿到", wins)
	}
	h.db.Model(&model.AgentTask{}).Where("id = ?", task.Id).Updates(map[string]any{"lease_owner": "", "lease_until": nil})
	// 多个 goroutine 同时推进同一任务。
	for i := 0; i < 4; i++ {
		go runTaskStep(context.Background(), task.Id)
	}
	StartTaskScheduler(context.Background())
	h.waitStatus(task.Id, model.TaskDone)
	if atomic.LoadInt32(&h.maxIn) != 1 {
		t.Fatalf("同一执行对话同时跑了 %d 轮", h.maxIn)
	}

	// 跨用户隔离：工具、存储与 HTTP 接口都拒绝。
	if _, err := loadUserTask("intruder", task.Id); err == nil {
		t.Fatal("其他用户读到了任务")
	}
	foreign := taskManageTool{policy: agentToolPolicy{UserID: "intruder", Kind: KindWork}}
	res, _ := foreign.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"action":"get","id":%d}`, task.Id)),
		&agent.Exec{SessionID: fmt.Sprint(h.main.Id)})
	if !res.IsError {
		t.Fatal("其他用户通过工具访问了任务")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxUserIDKey, "intruder"); c.Next() })
	r.GET("/api/tasks/:id", getTaskAPI)
	r.POST("/api/tasks/:id/:action", taskActionAPI)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tasks/%d", task.Id), nil),
		httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/cancel", task.Id), strings.NewReader("{}")),
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("跨用户请求 %s 返回 %d", req.URL, w.Code)
		}
	}
	// 执行对话正在被用户手动使用时，任务轮次让路。
	if !tryMarkConversationBusy(task.ExecConversationID) {
		t.Fatal("空闲会话应能登记")
	}
	if tryMarkConversationBusy(task.ExecConversationID) {
		t.Fatal("同一会话被登记两次")
	}
	unmarkConversationBusy(task.ExecConversationID)
}

// 规格校验：没有验收条件不能建任务；模型在非自主档不能设置命令验收。
func TestTaskSpecValidation(t *testing.T) {
	if _, err := normalizeTaskSpec(taskSpec{Goal: "x"}, true); err == nil {
		t.Fatal("没有验收条件也建成了任务")
	}
	_, err := normalizeTaskSpec(taskSpec{Goal: "x", Criteria: []taskCriterion{{Text: "t", Method: "command", Check: &taskCheck{Program: "go"}}}}, false)
	if err == nil {
		t.Fatal("非自主档的模型设置了命令验收")
	}
}
