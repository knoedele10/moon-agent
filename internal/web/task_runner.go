package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 本文件是长任务的调度与执行（docs/agent/16-task-runtime.md）。
//
// 调度器只认库里的任务行：running/verifying 且 next_run_at 已到、没有有效租约的任务
// 会被拿租约、跑一轮。每轮结束由宿主判断下一步——模型自然收尾（例如只写了「下一步……」）
// 不等于完成：没有 task_submit 就自动投递续跑消息，再跑下一轮；提交后进入验收。

// ---------------------------------------------------------------------------
// 调度器
// ---------------------------------------------------------------------------

var (
	taskSchedMu     sync.Mutex
	taskSchedCancel context.CancelFunc
	taskSchedDone   chan struct{}
	taskKick        = make(chan struct{}, 1)
	taskOwnerID     = fmt.Sprintf("p%d-%d", os.Getpid(), time.Now().UnixNano())

	// taskRunCancels 登记正在跑的轮次：暂停/取消时立即打断，循环在下一个工具前停下。
	taskRunMu      sync.Mutex
	taskRunCancels = map[uint]context.CancelFunc{}
	// taskInflight 防止同一进程对同一任务重复起 goroutine（租约之外的第二道门）。
	taskInflight = map[uint]bool{}
	taskWG       sync.WaitGroup
)

func taskTickInterval() time.Duration {
	if n := agentIntEnv("AGENT_TASK_TICK_MS"); n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return 3 * time.Second
}

func taskLeaseTTL() time.Duration {
	if n := agentIntEnv("AGENT_TASK_LEASE_SEC"); n > 0 {
		return time.Duration(n) * time.Second
	}
	return 2 * time.Minute
}

func taskConcurrency() int { return agentIntEnvOr("AGENT_TASK_CONCURRENCY", 2) }

// kickTaskScheduler 让调度器立刻扫一遍（新任务、用户点了继续等）。非阻塞。
func kickTaskScheduler() {
	select {
	case taskKick <- struct{}{}:
	default:
	}
}

// StartTaskScheduler 先做启动对账，再起后台调度循环。幂等。
func StartTaskScheduler(ctx context.Context) {
	taskSchedMu.Lock()
	defer taskSchedMu.Unlock()
	if taskSchedCancel != nil || model.DB == nil {
		return
	}
	reconcileTasksOnStart()
	ctx, cancel := context.WithCancel(ctx)
	taskSchedCancel = cancel
	done := make(chan struct{})
	taskSchedDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(taskTickInterval())
		defer ticker.Stop()
		log.Printf("[task] 调度已启动，每 %v 检查一次", taskTickInterval())
		for {
			scanDueTasks(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-taskKick:
			}
		}
	}()
}

// StopTaskScheduler 停调度并打断正在跑的轮次（轮次会被记为 interrupted，重启后对账续跑）。
func StopTaskScheduler() {
	taskSchedMu.Lock()
	cancel, done := taskSchedCancel, taskSchedDone
	taskSchedCancel, taskSchedDone = nil, nil
	taskSchedMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	taskRunMu.Lock()
	for _, c := range taskRunCancels {
		c()
	}
	taskRunMu.Unlock()
	taskWG.Wait()
}

func scanDueTasks(ctx context.Context) {
	if model.DB == nil || ctx.Err() != nil {
		return
	}
	now := time.Now()
	var due []model.AgentTask
	model.DB.Select("id").
		Where("status IN ?", []string{model.TaskRunning, model.TaskVerifying}).
		Where("(next_run_at IS NULL OR next_run_at <= ?)", now).
		Where("(lease_until IS NULL OR lease_until < ?)", now).
		Order("next_run_at asc").Limit(50).Find(&due)
	for _, t := range due {
		taskRunMu.Lock()
		if taskInflight[t.Id] || len(taskInflight) >= taskConcurrency() {
			taskRunMu.Unlock()
			continue
		}
		taskInflight[t.Id] = true
		taskRunMu.Unlock()
		taskWG.Add(1)
		go func(id uint) {
			defer taskWG.Done()
			defer func() {
				taskRunMu.Lock()
				delete(taskInflight, id)
				taskRunMu.Unlock()
				kickTaskScheduler()
			}()
			runTaskStep(ctx, id)
		}(t.Id)
	}
}

// runTaskStep 拿租约后推进一步：running 跑一轮执行，verifying 做验收。
func runTaskStep(parent context.Context, id uint) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[task] 任务 %d 推进时 panic：%v\n%s", id, rec, debug.Stack())
			// panic 跳过了轮次收尾：把仍是 running 的轮次落成 error，免得界面一直显示「执行中」。
			model.DB.Model(&model.AgentTaskRound{}).Where("task_id = ? AND status = ?", id, model.TaskRoundRunning).
				Updates(map[string]any{"status": model.TaskRoundError, "err_text": fmt.Sprintf("内部错误：%v", rec), "finished_at": time.Now()})
			failTaskStep(id, fmt.Errorf("内部错误：%v", rec), false)
		}
	}()
	owner := taskOwnerID
	ttl := taskLeaseTTL()
	if !claimTaskLease(id, owner, ttl) {
		return
	}
	defer releaseTaskLease(id, owner)
	stopRenew := make(chan struct{})
	defer close(stopRenew)
	go func() {
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-stopRenew:
				return
			case <-t.C:
				renewTaskLease(id, owner, ttl)
			}
		}
	}()
	task, err := loadTask(id)
	if err != nil {
		return
	}
	switch task.Status {
	case model.TaskRunning:
		runTaskRound(parent, task)
	case model.TaskVerifying:
		runTaskVerification(parent, task)
	}
}

// ---------------------------------------------------------------------------
// 预算
// ---------------------------------------------------------------------------

// taskBudgetExceeded 返回耗尽的预算维度（空串表示还有预算）。
func taskBudgetExceeded(t model.AgentTask, now time.Time) (code, why string) {
	switch {
	case t.MaxRounds > 0 && t.RoundsUsed >= t.MaxRounds:
		return "budget_rounds", fmt.Sprintf("已用完 %d 轮执行预算", t.MaxRounds)
	case t.MaxToolCalls > 0 && t.ToolCallsUsed >= t.MaxToolCalls:
		return "budget_tool_calls", fmt.Sprintf("已用完 %d 次工具调用预算", t.MaxToolCalls)
	case t.MaxTokens > 0 && t.TokensUsed >= t.MaxTokens:
		return "budget_tokens", fmt.Sprintf("已用完 %d token 预算", t.MaxTokens)
	case t.Deadline != nil && now.After(*t.Deadline):
		return "budget_deadline", "已超过截止时间 " + t.Deadline.Format("2006-01-02 15:04")
	}
	return "", ""
}

func markBudgetExhausted(t model.AgentTask, code, why string) {
	ok, _ := transitionTask(t.Id, []string{model.TaskRunning, model.TaskVerifying}, map[string]any{
		"status": model.TaskBudgetExhausted, "stop_reason": code,
		"blocker": "预算耗尽，任务未完成：" + why,
	})
	if ok {
		recordTaskEvent(t.Id, "budget_exhausted", why, nil)
		notifyTaskMain(t, "预算耗尽，任务**未完成**："+why+"。最近进度："+clipRunes(t.Progress, 400)+"。可在任务面板追加预算后重试。")
	}
}

// ---------------------------------------------------------------------------
// 一轮执行
// ---------------------------------------------------------------------------

// taskJournalEntry 是一次工具调用的流水：开始写 started，结束改为 ok/error/blocked。
type taskJournalEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Args    string `json:"args"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

// taskRoundState 是一轮执行的共享状态：任务工具和流水包装都写它。
type taskRoundState struct {
	mu        sync.Mutex
	task      model.AgentTask
	roundID   uint
	roundNo   int
	journal   []taskJournalEntry
	submitted *taskSubmission
	blocked   string
	toolCalls int
	// priorOK 是被重启打断那一轮里**已成功**的风险操作（name+args），
	// 核对轮里完全相同的调用会被拦下，避免重复产生外部副作用。
	priorOK map[string]bool
}

func (s *taskRoundState) persistJournal() {
	s.mu.Lock()
	raw, _ := json.Marshal(s.journal)
	calls := s.toolCalls
	s.mu.Unlock()
	model.DB.Model(&model.AgentTaskRound{}).Where("id = ?", s.roundID).
		Updates(map[string]any{"journal": string(raw), "tool_calls": calls})
}

func taskCallKey(name, args string) string { return name + "\x00" + strings.TrimSpace(args) }

// taskJournalSource 包装 ToolSource：每个工具执行前后都落流水，并在核对轮拦截重复副作用。
type taskJournalSource struct {
	inner agent.ToolSource
	st    *taskRoundState
}

func (s taskJournalSource) Defs() []llm.ToolDef { return s.inner.Defs() }
func (s taskJournalSource) Lookup(name string) (agent.Tool, bool) {
	t, ok := s.inner.Lookup(name)
	if !ok {
		return nil, false
	}
	return taskJournalTool{inner: t, st: s.st}, true
}

type taskJournalTool struct {
	inner agent.Tool
	st    *taskRoundState
}

func (t taskJournalTool) Name() string               { return t.inner.Name() }
func (t taskJournalTool) Description() string        { return t.inner.Description() }
func (t taskJournalTool) Parameters() map[string]any { return t.inner.Parameters() }
func (t taskJournalTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	name, args := t.inner.Name(), clipRunes(string(raw), 2000)
	st := t.st
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err // 已取消/暂停：不再启动新步骤
	}
	st.mu.Lock()
	dup := st.priorOK[taskCallKey(name, string(raw))] && riskyTool(name)
	st.toolCalls++
	idx := len(st.journal)
	st.journal = append(st.journal, taskJournalEntry{ID: fmt.Sprintf("r%d-%d", st.roundNo, idx+1), Name: name, Args: args, Status: "started"})
	st.mu.Unlock()
	if dup {
		st.mu.Lock()
		st.journal[idx].Status, st.journal[idx].Summary = "skipped", "重启前已成功执行，拦截重复"
		st.mu.Unlock()
		st.persistJournal()
		return agent.Result{IsError: true, Content: "重启前的上一轮已成功执行过完全相同的 " + name + " 调用，为避免重复外部操作已拦截。" +
			"请先用只读工具核对现场；确有必要再次执行时，说明原因并调整参数。"}, nil
	}
	st.persistJournal()
	res, err := t.inner.Execute(ctx, raw, ex)
	status := "ok"
	summary := clipRunes(strings.TrimSpace(res.Content), 300)
	switch {
	case err != nil:
		status, summary = "error", clipRunes(err.Error(), 300)
	case strings.TrimSpace(res.AwaitUser) != "":
		status = "blocked"
	case res.IsError:
		status = "error"
	}
	st.mu.Lock()
	st.journal[idx].Status, st.journal[idx].Summary = status, summary
	st.mu.Unlock()
	st.persistJournal()
	return res, err
}

// taskContinuePrompt 是模型没提交就收尾时自动投递的续跑消息。
func taskContinuePrompt(t model.AgentTask, reply string) string {
	var b strings.Builder
	b.WriteString("【系统·自动续跑】任务尚未通过验收，本轮你没有调用 task_submit 就结束了。")
	if looksLikePlanOnly(reply) {
		b.WriteString("你上一条回复只写了计划/下一步，没有实际执行。")
	}
	b.WriteString("不要等待用户说「继续」：现在直接执行下一步。已完成的步骤不要重复；" +
		"全部验收条件满足后调用 task_submit 逐条给出证据；确实需要人提供信息或权限时调用 task_block。")
	if p := strings.TrimSpace(t.Progress); p != "" {
		b.WriteString("\n当前记录的进度：" + clipRunes(p, 600))
	}
	return b.String()
}

// looksLikePlanOnly 判断回复是不是「只说不做」：只用于措辞，续跑与否不依赖它。
func looksLikePlanOnly(reply string) bool {
	r := strings.TrimSpace(reply)
	if r == "" {
		return true
	}
	for _, kw := range []string{"下一步", "接下来", "我将", "我会", "计划", "next step", "I will", "I'll"} {
		if strings.Contains(r, kw) {
			return true
		}
	}
	return false
}

// taskCard 是每轮都注入系统提示的任务卡：原始目标、约束、验收条件、进度、已完成步骤、
// 上次验收的失败项与重启核对说明。它在系统提示里，入口压缩和循环内压缩都不会丢掉它。
func taskCard(t model.AgentTask, reconcile string) string {
	var b strings.Builder
	b.WriteString("\n\n【长任务 #" + strconv.FormatUint(uint64(t.Id), 10) + "（宿主持久保存，每轮原样注入，优先级高于摘要）】\n")
	b.WriteString("## 原始目标（用户原文）\n" + t.Goal + "\n")
	if strings.TrimSpace(t.Constraints) != "" {
		b.WriteString("## 必须遵守的约束（用户原文，压缩后依然有效）\n" + t.Constraints + "\n")
	}
	b.WriteString("## 验收条件\n" + renderCriteria(taskCriteria(t)))
	b.WriteString(fmt.Sprintf("## 预算\n已用 %d/%d 轮", t.RoundsUsed, t.MaxRounds))
	if t.MaxToolCalls > 0 {
		b.WriteString(fmt.Sprintf("，工具调用 %d/%d", t.ToolCallsUsed, t.MaxToolCalls))
	}
	b.WriteString("\n")
	if p := strings.TrimSpace(t.Progress); p != "" {
		b.WriteString("## 当前进度\n" + p + "\n")
	}
	if steps := taskDoneSteps(t); len(steps) > 0 {
		b.WriteString("## 已完成步骤（不要重复执行）\n")
		for _, s := range steps {
			b.WriteString("- " + s + "\n")
		}
	}
	if v := taskLastVerification(t); v != nil && v.Overall == verdictFail {
		b.WriteString("## 上次验收未通过的项（先修这些）\n")
		for _, it := range v.Items {
			if it.Verdict == verdictFail {
				b.WriteString(fmt.Sprintf("- [%s] %s：%s\n", it.Criterion, it.Text, it.Reason))
			}
		}
	}
	if reconcile != "" {
		b.WriteString(reconcile)
	}
	b.WriteString("## 工作规则\n" +
		"- 你的「完成了」不算完成：只有 task_submit 后宿主逐项验收通过，任务才结束。\n" +
		"- 每完成一个阶段调用 task_update_progress（progress + done_steps）；这是重启和压缩后恢复的依据。\n" +
		"- 不要只写计划然后停下；能执行就直接执行。\n" +
		"- 缺少登录、权限、密钥或只有用户能做的决定时，调用 task_block 说明需要人做什么，然后结束本轮。\n")
	return b.String()
}

// reconcileNotice 汇总被重启打断那一轮的工具流水，要求先核对现场。
func reconcileNotice(taskID uint) (string, map[string]bool) {
	var r model.AgentTaskRound
	if err := model.DB.Where("task_id = ? AND status = ?", taskID, model.TaskRoundInterrupted).
		Order("id desc").First(&r).Error; err != nil {
		return "## 恢复说明\n服务曾重启。先用只读工具核对工作区的实际状态，再继续执行。\n", nil
	}
	var journal []taskJournalEntry
	_ = json.Unmarshal([]byte(r.Journal), &journal)
	prior := map[string]bool{}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 恢复说明（第 %d 轮被服务重启打断）\n", r.No))
	b.WriteString("先核对现场（读文件、看状态、查命令输出），确认哪些已经生效，再继续；不要盲目重做。\n")
	if len(journal) > 0 {
		b.WriteString("被打断那一轮的工具流水：\n")
		for _, e := range journal {
			label := map[string]string{"ok": "已成功", "error": "失败", "started": "结果未知（执行中被打断，务必先核对）", "blocked": "等待人工", "skipped": "已拦截"}[e.Status]
			b.WriteString(fmt.Sprintf("- %s %s → %s\n", e.Name, clipRunes(e.Args, 160), label))
			if e.Status == "ok" {
				prior[taskCallKey(e.Name, e.Args)] = true
			}
		}
	}
	return b.String(), prior
}

func runTaskRound(parent context.Context, task model.AgentTask) {
	now := time.Now()
	if code, why := taskBudgetExceeded(task, now); code != "" {
		markBudgetExhausted(task, code, why)
		return
	}
	// 用户正在手动和执行对话说话：让一下，稍后再来（同一会话不并发两轮）。
	if !tryMarkConversationBusy(task.ExecConversationID) {
		later := now.Add(5 * time.Second)
		model.DB.Model(&model.AgentTask{}).Where("id = ?", task.Id).Update("next_run_at", later)
		return
	}
	defer unmarkConversationBusy(task.ExecConversationID)

	if _, err := deliverTaskOutbox(task); err != nil {
		failTaskStep(task.Id, fmt.Errorf("投递任务消息失败：%w", err), true)
		return
	}

	reconcile := ""
	var prior map[string]bool
	trigger := "continue"
	if task.RoundsUsed == 0 {
		trigger = "start"
	}
	if task.NeedsReconcile {
		reconcile, prior = reconcileNotice(task.Id)
		trigger = "reconcile"
	}

	round := model.AgentTaskRound{TaskID: task.Id, No: task.RoundsUsed + 1, Trigger: trigger, Status: model.TaskRoundRunning, StartedAt: now, Journal: "[]"}
	if err := model.DB.Create(&round).Error; err != nil {
		failTaskStep(task.Id, fmt.Errorf("记录轮次失败：%w", err), true)
		return
	}
	model.DB.Model(&model.AgentTask{}).Where("id = ?", task.Id).
		Updates(map[string]any{"rounds_used": round.No, "needs_reconcile": false, "updated_at": now})
	task.RoundsUsed = round.No

	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	stopParent := context.AfterFunc(parent, cancel)
	defer stopParent()
	taskRunMu.Lock()
	taskRunCancels[task.Id] = cancel
	taskRunMu.Unlock()
	defer func() {
		taskRunMu.Lock()
		delete(taskRunCancels, task.Id)
		taskRunMu.Unlock()
	}()
	ctx, usage := llm.WithUsage(ctx)

	st := &taskRoundState{task: task, roundID: round.Id, roundNo: round.No, priorOK: prior}
	reply, needInput, compacted, runErr := executeTaskRound(ctx, task, st, reconcile)

	tokens := usage.Snapshot().InputTokens + usage.Snapshot().OutputTokens
	if tokens == 0 {
		tokens = estimateTokensText(reply) // 供应商没返回用量时按输出粗估，至少不为 0
	}
	finished := time.Now()
	st.mu.Lock()
	submitted, blocked, calls := st.submitted, st.blocked, st.toolCalls
	journal, _ := json.Marshal(st.journal)
	st.mu.Unlock()

	if strings.TrimSpace(reply) != "" {
		saveChatMessage(task.ExecConversationID, "assistant", reply)
		touchConversation(task.ExecConversationID)
	}
	model.DB.Model(&model.AgentTask{}).Where("id = ?", task.Id).Updates(map[string]any{
		"tool_calls_used": task.ToolCallsUsed + calls, "tokens_used": task.TokensUsed + tokens,
	})
	task.ToolCallsUsed += calls
	task.TokensUsed += tokens

	outcome, roundStatus, errText := "", model.TaskRoundFinished, ""
	defer func() {
		model.DB.Model(&model.AgentTaskRound{}).Where("id = ?", round.Id).Updates(map[string]any{
			"status": roundStatus, "outcome": outcome, "reply": clipRunes(reply, 8000), "journal": string(journal),
			"tool_calls": calls, "tokens": tokens, "compacted": compacted, "err_text": errText, "finished_at": finished,
		})
	}()

	// 先看任务有没有在这一轮期间被暂停/取消：那种情况下不再做任何推进。
	latest, err := loadTask(task.Id)
	if err != nil {
		outcome = "lost"
		return
	}
	if latest.Status != model.TaskRunning {
		outcome, roundStatus = "stopped_by_user", model.TaskRoundInterrupted
		if runErr != nil {
			errText = runErr.Error()
		}
		return
	}

	if runErr != nil && !isLimitError(runErr) {
		if errors.Is(runErr, context.Canceled) && parent.Err() != nil {
			// 进程在关闭：轮次记为中断，下次启动对账续跑。
			outcome, roundStatus, errText = "shutdown", model.TaskRoundInterrupted, runErr.Error()
			model.DB.Model(&model.AgentTask{}).Where("id = ?", task.Id).Update("needs_reconcile", true)
			return
		}
		outcome, roundStatus, errText = "error", model.TaskRoundError, runErr.Error()
		failTaskStep(task.Id, runErr, isTransientTaskError(runErr))
		return
	}
	// 成功跑完一轮：连续失败清零。
	model.DB.Model(&model.AgentTask{}).Where("id = ?", task.Id).Updates(map[string]any{"consecutive_failures": 0, "last_error": ""})

	switch {
	case submitted != nil:
		outcome = "submitted"
		raw, _ := json.Marshal(submitted)
		if ok, _ := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
			"status": model.TaskVerifying, "stop_reason": "submitted", "evidence": string(raw), "next_run_at": time.Now(),
		}); ok {
			recordTaskEvent(task.Id, "submitted", "执行器提交了结果与证据，进入验收", submitted)
			kickTaskScheduler()
		}
	case blocked != "" || needInput != "":
		outcome = "need_human"
		why := blocked
		if why == "" {
			why = needInput
		}
		if ok, _ := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
			"status": model.TaskWaitingHuman, "stop_reason": "need_human", "blocker": clipRunes(why, 2000), "next_run_at": nil,
		}); ok {
			recordTaskEvent(task.Id, "need_human", why, nil)
			notifyTaskMain(task, "需要人工处理后才能继续："+clipRunes(why, 600)+"。在任务面板补充信息后会自动继续。")
		}
	default:
		// 模型自然收尾却没提交：宿主判定未完成，自动投递续跑消息。
		outcome = "auto_continue"
		if isLimitError(runErr) {
			outcome = "limit_continue"
		}
		key := fmt.Sprintf("t%d:continue:r%d", task.Id, round.No)
		if err := enqueueTaskMessage(task.Id, "continue", taskContinuePrompt(latest, reply), key); err != nil {
			failTaskStep(task.Id, fmt.Errorf("写入续跑消息失败：%w", err), true)
			return
		}
		next := time.Now().Add(taskContinueDelay())
		model.DB.Model(&model.AgentTask{}).Where("id = ? AND status = ?", task.Id, model.TaskRunning).
			Updates(map[string]any{"next_run_at": next, "stop_reason": outcome})
		recordTaskEvent(task.Id, "auto_continue", fmt.Sprintf("第 %d 轮结束但未提交验收，已自动续跑", round.No), map[string]any{"reply": clipRunes(reply, 300)})
	}
}

func taskContinueDelay() time.Duration {
	if n := agentIntEnv("AGENT_TASK_CONTINUE_DELAY_MS"); n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return time.Second
}

func isLimitError(err error) bool {
	if err == nil {
		return false
	}
	var lr *agent.LimitReportError
	return errors.Is(err, agent.ErrMaxIterations) || errors.Is(err, agent.ErrMaxToolCalls) || errors.As(err, &lr)
}

// executeTaskRound 组装执行对话的模型输入与工具，跑一次 agent.Run。
func executeTaskRound(ctx context.Context, task model.AgentTask, st *taskRoundState, reconcile string) (reply, needInput string, compacted int, err error) {
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", task.ExecConversationID, task.UserID).First(&conv).Error; err != nil {
		return "", "", 0, fmt.Errorf("执行对话不存在：%w", err)
	}
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&rows).Error; err != nil {
		return "", "", 0, err
	}
	key := subagentAPIKey(task.UserID, task.Model)
	history := rebuildLLMHistory(rows)
	cr := maybeCompactHistory(ctx, &conv, history, task.Model, key)
	history = cr.History
	if cr.Compacted {
		compacted = 1
	}

	roots := taskRoots(task)
	if len(roots) == 0 {
		roots = teamWorkspaceRootsFor(task.UserID, task.Kind)
	}
	req := agent.SubagentRequest{Model: task.Model, Kind: task.Kind, Mode: task.Mode, Root: firstWorkspaceRoot(roots), Roots: additionalWorkspaceRoots(roots)}
	policy := agentToolPolicy{UserID: task.UserID, Root: req.Root, Roots: req.Roots, Kind: task.Kind, Mode: task.Mode, Depth: subagentMaxDepth(), Child: true}
	base := buildAgentTools(policy)
	base = append(base, taskExecutorTools(st)...)
	evo := newHermesEvolve(task.UserID)
	tools, source := assembleRuntimeTools(base, policy, evo)
	if source == nil {
		// 未开启渐进披露时装配只返回静态工具表（可能已被落盘策略包装）；
		// 流水包装需要一个 ToolSource，这里用同一份工具建静态注册表。
		source = agent.NewRegistry(tools...)
	}

	maxCalls := subagentMaxToolCalls()
	if task.MaxToolCalls > 0 {
		if left := task.MaxToolCalls - task.ToolCallsUsed; left < maxCalls {
			maxCalls = left
		}
		if maxCalls < 1 {
			maxCalls = 1
		}
	}
	cfg := agent.Config{
		Model:         task.Model,
		APIKey:        key,
		SystemPrompt:  subagentSystemPrompt(req) + taskCard(task, reconcile) + compactSummaryBlock(cr.Summary),
		MaxIterations: subagentMaxIterations(),
		MaxToolCalls:  maxCalls,
		ToolSource:    taskJournalSource{inner: source, st: st},
		ContextTokens: modelContextTokens(task.Model),
		Exec: &agent.Exec{
			SessionID:    strconv.FormatUint(uint64(conv.Id), 10),
			TurnID:       fmt.Sprintf("task-%d-r%d", task.Id, st.roundNo),
			Root:         req.Root,
			Roots:        append([]string(nil), req.Roots...),
			ReadRoot:     agentSkillsDir(),
			AllowOutside: isOwnerUser(task.UserID) && subagentAllowOutside(task.Mode),
		},
	}
	if agentUsesEino() && registry != nil {
		cm, modelErr := registry.NewToolCallingModel(ctx, task.Model, key, llm.ChatRequest{})
		if modelErr != nil {
			return "", "", compacted, modelErr
		}
		cfg.EinoModel = cm
	}
	var acc strings.Builder
	emit := func(ev agent.Event) {
		switch ev.Kind {
		case agent.EvDelta:
			acc.WriteString(ev.Content)
		case agent.EvDone:
			if ev.Content != "" && acc.Len() == 0 {
				acc.WriteString(ev.Content)
			}
		case agent.EvNeedInput:
			needInput = strings.TrimSpace(ev.Content)
		}
	}
	err = agent.Run(ctx, cfg, history, emit)
	return strings.TrimSpace(acc.String()), needInput, compacted, err
}

func estimateTokensText(s string) int {
	return agent.EstimateMessagesTokens([]llm.Message{{Content: s}})
}

// ---------------------------------------------------------------------------
// 失败处理：临时故障退避重试，重复失败停止
// ---------------------------------------------------------------------------

func taskMaxFailures() int { return agentIntEnvOr("AGENT_TASK_MAX_FAILURES", 4) }

// isTransientTaskError 判断是否值得退避重试：网络、超时、限流、5xx。
func isTransientTaskError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{"timeout", "timed out", "deadline", "connection reset", "connection refused", "eof",
		"temporarily", "unavailable", "rate limit", "too many requests", "429", "500", "502", "503", "504", "overloaded", "投递任务消息失败", "记录轮次失败"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// taskBackoff 是第 n 次连续失败后的等待：2s、4s、8s……封顶 5 分钟（测试可用环境变量缩短）。
func taskBackoff(n int) time.Duration {
	base := time.Duration(agentIntEnvOr("AGENT_TASK_BACKOFF_MS", 2000)) * time.Millisecond
	d := base
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// failTaskStep 记一次失败：临时故障退避重试；非临时故障或连续失败到上限即停止为 failed。
func failTaskStep(id uint, cause error, transient bool) {
	t, err := loadTask(id)
	if err != nil || model.TaskTerminal(t.Status) {
		return
	}
	n := t.ConsecutiveFailures + 1
	limit := taskMaxFailures()
	if !transient && limit > 2 {
		limit = 2 // 非临时错误（配置、模型不存在等）重试一次即可判定
	}
	msg := clipRunes(cause.Error(), 1000)
	if n >= limit {
		if ok, _ := transitionTask(id, []string{model.TaskRunning, model.TaskVerifying}, map[string]any{
			"status": model.TaskFailed, "stop_reason": "repeated_failure", "consecutive_failures": n,
			"last_error": msg, "blocker": fmt.Sprintf("连续失败 %d 次后停止：%s", n, msg),
		}); ok {
			recordTaskEvent(id, "failed", fmt.Sprintf("连续失败 %d 次，停止：%s", n, msg), nil)
			notifyTaskMain(t, fmt.Sprintf("连续失败 %d 次已停止（任务未完成）：%s", n, clipRunes(msg, 300)))
		}
		return
	}
	next := time.Now().Add(taskBackoff(n))
	model.DB.Model(&model.AgentTask{}).Where("id = ?", id).Updates(map[string]any{
		"consecutive_failures": n, "last_error": msg, "next_run_at": next, "updated_at": time.Now(),
	})
	recordTaskEvent(id, "retry", fmt.Sprintf("第 %d 次失败（%s），%v 后重试：%s", n, map[bool]string{true: "临时故障", false: "错误"}[transient], taskBackoff(n).Round(time.Millisecond), msg), nil)
}

// cancelTaskRun 打断某任务正在跑的轮次（暂停/取消时调用）。
func cancelTaskRun(id uint) {
	taskRunMu.Lock()
	c := taskRunCancels[id]
	taskRunMu.Unlock()
	if c != nil {
		c()
	}
}
