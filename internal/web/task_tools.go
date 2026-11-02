package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// 执行器工具：只在任务轮次里出现，绑定本轮的 taskRoundState。
// 它们只写任务行，不改变「是否完成」——完成与否由 task_verify.go 的宿主验收决定。

func taskExecutorTools(st *taskRoundState) []agent.Tool {
	return []agent.Tool{taskProgressTool{st: st}, taskSubmitTool{st: st}, taskBlockTool{st: st}, taskHandBackTool{st: st}}
}

func taskToolFail(msg string) (agent.Result, error) {
	return agent.Result{IsError: true, Content: msg}, nil
}

type taskProgressTool struct{ st *taskRoundState }

func (taskProgressTool) Name() string { return "task_update_progress" }
func (taskProgressTool) Description() string {
	return "记录长任务进度：progress 写当前状态与下一步；done_steps 追加已完成且已核实的步骤（会在重启/压缩后注入，避免重复执行）。"
}
func (taskProgressTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"progress":   map[string]any{"type": "string", "description": "当前进度与下一步，300 字以内"},
		"done_steps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "本次新完成的步骤，每条一句话"},
	}, "required": []string{"progress"}, "additionalProperties": false}
}
func (t taskProgressTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var args struct {
		Progress  string   `json:"progress"`
		DoneSteps []string `json:"done_steps"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Progress) == "" {
		return taskToolFail("需要 progress 文本")
	}
	cur, err := loadTask(t.st.task.Id)
	if err != nil {
		return taskToolFail("任务不存在")
	}
	steps := taskDoneSteps(cur)
	seen := map[string]bool{}
	for _, s := range steps {
		seen[s] = true
	}
	for _, s := range args.DoneSteps {
		s = clipRunes(strings.TrimSpace(s), 200)
		if s != "" && !seen[s] && len(steps) < 80 {
			steps = append(steps, s)
			seen[s] = true
		}
	}
	b, _ := json.Marshal(steps)
	model.DB.Model(&model.AgentTask{}).Where("id = ?", cur.Id).Updates(map[string]any{
		"progress": clipRunes(strings.TrimSpace(args.Progress), 2000), "done_steps": string(b), "updated_at": time.Now(),
	})
	return agent.Result{Content: fmt.Sprintf("进度已保存（已完成步骤 %d 条）", len(steps))}, nil
}

type taskSubmitTool struct{ st *taskRoundState }

func (taskSubmitTool) Name() string { return "task_submit" }
func (taskSubmitTool) Description() string {
	return "提交长任务结果供宿主验收。仅在你确认全部验收条件已满足时调用；为每条验收条件给出可核对的证据（文件路径、命令及输出、关键数据）。" +
		"提交不等于完成：宿主会逐项核对，未通过的项会交回给你修正。调用后结束本轮。"
}
func (taskSubmitTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"summary": map[string]any{"type": "string", "description": "交付结果概述"},
		"evidence": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"criterion": map[string]any{"type": "string", "description": "验收条件 id，例如 c1"},
			"evidence":  map[string]any{"type": "string", "description": "该条件满足的具体证据"},
		}, "required": []string{"criterion", "evidence"}}},
	}, "required": []string{"summary", "evidence"}, "additionalProperties": false}
}
func (t taskSubmitTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var args struct {
		Summary  string             `json:"summary"`
		Evidence []taskEvidenceItem `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Summary) == "" {
		return taskToolFail("需要 summary 与 evidence")
	}
	valid := map[string]bool{}
	for _, c := range taskCriteria(t.st.task) {
		valid[c.ID] = true
	}
	var missing []string
	got := map[string]bool{}
	items := make([]taskEvidenceItem, 0, len(args.Evidence))
	for _, it := range args.Evidence {
		it.Criterion = strings.TrimSpace(it.Criterion)
		if !valid[it.Criterion] {
			return taskToolFail("未知的验收条件 id：" + it.Criterion)
		}
		it.Evidence = clipRunes(strings.TrimSpace(it.Evidence), 4000)
		got[it.Criterion] = true
		items = append(items, it)
	}
	for _, c := range taskCriteria(t.st.task) {
		if !got[c.ID] && c.Method != criterionManual {
			missing = append(missing, c.ID)
		}
	}
	if len(missing) > 0 {
		return taskToolFail("以下验收条件缺少证据：" + strings.Join(missing, ", ") + "。补齐后再提交；还没做完就继续执行。")
	}
	t.st.mu.Lock()
	t.st.submitted = &taskSubmission{Summary: clipRunes(strings.TrimSpace(args.Summary), 4000), Items: items, Round: t.st.roundNo, At: time.Now()}
	t.st.mu.Unlock()
	// AwaitUser 让循环立刻停下：提交之后的验收由宿主做，模型不应再自说自话地宣布完成。
	return agent.Result{Content: "已提交验收，宿主将逐项核对。", AwaitUser: "已提交验收"}, nil
}

type taskBlockTool struct{ st *taskRoundState }

func (taskBlockTool) Name() string { return "task_block" }
func (taskBlockTool) Description() string {
	return "任务需要人工介入时调用：缺少登录/凭据/权限、需要用户提供信息或做决定。写清楚需要人做什么。调用后任务暂停等待人工，补充后自动继续。"
}
func (taskBlockTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"reason": map[string]any{"type": "string", "description": "卡在哪里、需要人提供什么"},
		"kind":   map[string]any{"type": "string", "enum": []string{"login", "info", "permission", "decision"}},
	}, "required": []string{"reason"}, "additionalProperties": false}
}
func (t taskBlockTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var args struct {
		Reason string `json:"reason"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Reason) == "" {
		return taskToolFail("需要 reason")
	}
	why := strings.TrimSpace(args.Reason)
	if k := strings.TrimSpace(args.Kind); k != "" {
		why = "[" + k + "] " + why
	}
	t.st.mu.Lock()
	t.st.blocked = clipRunes(why, 2000)
	t.st.mu.Unlock()
	return agent.Result{Content: "已标记为等待人工。", AwaitUser: why}, nil
}

// taskHandBackTool 把明显超出本任务范围的后续消息交回前台判断
// （docs/agent/18-superagent.md §3.3）。它只对「已经扇出给本任务」的消息生效，
// 不扩大授权，也不改变任务的验收。
type taskHandBackTool struct{ st *taskRoundState }

func (taskHandBackTool) Name() string { return "orchestrator_handback" }
func (taskHandBackTool) Description() string {
	return "把一条明显超出本任务范围的后续消息交回前台判断（例如用户在原对话里改成了另一件事）。" +
		"只对经扇出交给本任务的后续消息使用；不要用它拒绝任务范围内的难活。"
}
func (taskHandBackTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"trigger_id": map[string]any{"type": "integer", "description": "后续消息的 trigger id（消息里标注了 #id）"},
		"reason":     map[string]any{"type": "string", "description": "为什么超出本任务范围，一句话"},
	}, "required": []string{"trigger_id", "reason"}, "additionalProperties": false}
}
func (t taskHandBackTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var args struct {
		TriggerID uint   `json:"trigger_id"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.TriggerID == 0 || strings.TrimSpace(args.Reason) == "" {
		return taskToolFail("需要 trigger_id 与 reason")
	}
	if model.DB == nil {
		return taskToolFail("数据库未就绪")
	}
	var trig model.SATrigger
	if err := model.DB.First(&trig, args.TriggerID).Error; err != nil {
		return taskToolFail("找不到这条消息")
	}
	if trig.Disposition != model.SADispFannedOut {
		return taskToolFail("这条消息不在任何工单名下")
	}
	if !saTaskSubscribesThread(t.st.task.Id, trig.ThreadID) {
		return taskToolFail("这条消息不属于本任务")
	}
	if m, ok := saTaskMandate(t.st.task.Id); ok && m.OriginTriggerID == trig.Id {
		return taskToolFail("不能把任务的原始消息交回")
	}
	if err := saHandBack(ctx, t.st.task.Id, trig, strings.TrimSpace(args.Reason)); err != nil {
		return taskToolFail("交回失败：" + err.Error())
	}
	recordTaskEvent(t.st.task.Id, "handback", "执行端把消息交回前台判断："+clipRunes(args.Reason, 300),
		map[string]any{"trigger_id": trig.Id})
	return agent.Result{Content: fmt.Sprintf("已把消息 #%d 交回前台判断。", trig.Id)}, nil
}

// ---------------------------------------------------------------------------
// 主对话工具：task_manage
// ---------------------------------------------------------------------------

// taskManageTool 让主对话创建与管理长任务。它只能看到当前用户、当前主对话的任务。
type taskManageTool struct{ policy agentToolPolicy }

func (taskManageTool) Name() string { return "task_manage" }
func (taskManageTool) Description() string {
	return "长任务：create 新建一个由宿主持续推进、逐项验收的任务（复杂、需多轮执行、有明确验收条件时使用）；" +
		"list/get 查看状态；pause/resume/cancel 控制。create 返回只代表受理，结果会自动写回本对话，不要轮询。"
}
func (taskManageTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"action":      map[string]any{"type": "string", "enum": []string{"create", "list", "get", "pause", "resume", "cancel"}},
		"id":          map[string]any{"type": "integer"},
		"title":       map[string]any{"type": "string"},
		"goal":        map[string]any{"type": "string", "description": "用户原始目标（尽量原文）"},
		"constraints": map[string]any{"type": "string", "description": "用户提出的约束（原文）"},
		"criteria": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"text":   map[string]any{"type": "string"},
			"method": map[string]any{"type": "string", "enum": []string{"auto", "command", "manual"}},
			"check": map[string]any{"type": "object", "properties": map[string]any{
				"program": map[string]any{"type": "string"}, "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"cwd": map[string]any{"type": "string"},
			}},
		}, "required": []string{"text"}}},
		"max_rounds": map[string]any{"type": "integer"},
	}, "required": []string{"action"}, "additionalProperties": false}
}

func (t taskManageTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var args struct {
		Action string `json:"action"`
		ID     uint   `json:"id"`
		taskSpec
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return taskToolFail("参数必须为 JSON 对象")
	}
	if ex == nil || model.DB == nil {
		return taskToolFail("会话或数据库未就绪")
	}
	convID, err := parseSubagentConvID(ex.SessionID)
	if err != nil {
		return taskToolFail(err.Error())
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", convID, t.policy.UserID).First(&conv).Error; err != nil {
		return taskToolFail("只能管理当前用户自己的主对话中的任务")
	}
	out := func(v any) (agent.Result, error) {
		b, _ := json.Marshal(v)
		return agent.Result{Content: string(b)}, nil
	}
	switch args.Action {
	case "create":
		roots := append([]string{t.policy.Root}, t.policy.Roots...)
		if strings.TrimSpace(t.policy.Root) == "" {
			roots = nil
		}
		task, err := createTask(args.taskSpec, taskCreateOptions{
			UserID: t.policy.UserID, MainConversationID: conv.Id, Model: conv.Model, Kind: conv.Kind,
			Mode: t.policy.Mode, Roots: roots, AllowCommandChecks: normalizeMode(t.policy.Mode) == ModeAuto,
		})
		if err != nil {
			return taskToolFail(err.Error())
		}
		return out(map[string]any{"accepted": true, "id": task.Id, "exec_conversation_id": task.ExecConversationID,
			"notice": "已受理，宿主会持续执行并逐项验收；结果会写回本对话。现在结束本轮，不要轮询。"})
	case "list":
		var rows []model.AgentTask
		model.DB.Where("user_id = ? AND main_conversation_id = ?", t.policy.UserID, conv.Id).Order("id desc").Limit(20).Find(&rows)
		items := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			items = append(items, map[string]any{"id": r.Id, "title": r.Title, "status": r.Status, "progress": clipRunes(r.Progress, 200), "blocker": clipRunes(r.Blocker, 200)})
		}
		return out(items)
	}
	if args.ID == 0 {
		return taskToolFail("需要 id")
	}
	task, err := loadUserTask(t.policy.UserID, args.ID)
	if err != nil || task.MainConversationID != conv.Id {
		return taskToolFail("任务不存在")
	}
	switch args.Action {
	case "get":
		return out(taskView(task, false))
	case "pause", "resume", "cancel":
		if _, err := applyTaskAction(task, args.Action, taskActionInput{}); err != nil {
			return taskToolFail(err.Error())
		}
		return out(map[string]any{"ok": true, "id": task.Id})
	}
	return taskToolFail("action 仅支持 create / list / get / pause / resume / cancel")
}

const taskManagePrompt = "\n【长任务】复杂、需要多轮执行且有明确验收条件的工作，用 task_manage create 交给宿主持续推进：" +
	"goal 与 constraints 尽量保留用户原文，criteria 写成可逐条核对的条件。创建后结束本轮，结果会写回本对话。\n"

// ---------------------------------------------------------------------------
// 人工操作（界面按钮与 task_manage 共用）
// ---------------------------------------------------------------------------

type taskActionInput struct {
	Message     string `json:"message"`
	Revision    int    `json:"revision"`
	ExtraRounds int    `json:"extra_rounds"`
	Approve     *bool  `json:"approve"`
}

var nonTerminalTaskStates = []string{model.TaskRunning, model.TaskVerifying, model.TaskReview, model.TaskWaitingHuman, model.TaskPaused}

// applyTaskAction 执行暂停/继续/取消/重试/补充信息/审核。返回更新后的任务。
func applyTaskAction(task model.AgentTask, action string, in taskActionInput) (model.AgentTask, error) {
	if in.Revision > 0 && in.Revision != task.Revision {
		return task, errTaskConflict
	}
	now := time.Now()
	var ok bool
	var err error
	switch action {
	case "pause":
		ok, err = transitionTask(task.Id, []string{model.TaskRunning, model.TaskVerifying, model.TaskWaitingHuman, model.TaskReview},
			map[string]any{"status": model.TaskPaused, "stop_reason": "paused_by_user", "next_run_at": nil})
		if ok {
			cancelTaskRun(task.Id)
			recordTaskEvent(task.Id, "paused", "用户暂停了任务", nil)
		}
	case "resume":
		ok, err = transitionTask(task.Id, []string{model.TaskPaused},
			map[string]any{"status": model.TaskRunning, "stop_reason": "resumed", "next_run_at": now, "consecutive_failures": 0})
		if ok {
			_ = enqueueTaskMessage(task.Id, "resume", "【系统】用户已恢复任务。先核对当前进度与现场，然后继续执行未完成的部分。", fmt.Sprintf("t%d:resume:%d", task.Id, task.Revision))
			recordTaskEvent(task.Id, "resumed", "用户继续了任务", nil)
		}
	case "cancel":
		ok, err = transitionTask(task.Id, nonTerminalTaskStates,
			map[string]any{"status": model.TaskCanceled, "stop_reason": "canceled_by_user", "blocker": "用户已取消"})
		if ok {
			cancelTaskRun(task.Id)
			// 取消一份工单 = 收回授权：之后的消息不再认领给它（契约 §3.5）。
			_ = saWithdrawMandate(task.Id, "用户在任务面板取消了工单")
			recordTaskEvent(task.Id, "canceled", "用户取消了任务；不会再启动新步骤", nil)
			notifyTask(task, model.SAPurposeNotice, "已被用户取消（未完成）。")
		}
	case "retry":
		updates := map[string]any{"status": model.TaskRunning, "stop_reason": "retry", "next_run_at": now,
			"consecutive_failures": 0, "last_error": "", "blocker": "", "finished_at": nil}
		if task.Status == model.TaskBudgetExhausted {
			extra := in.ExtraRounds
			if extra <= 0 {
				extra = 10
			}
			updates["max_rounds"] = task.RoundsUsed + extra
			if task.MaxToolCalls > 0 {
				updates["max_tool_calls"] = task.ToolCallsUsed + task.MaxToolCalls
			}
			if task.MaxTokens > 0 {
				updates["max_tokens"] = task.TokensUsed + task.MaxTokens
			}
			if task.Deadline != nil && now.After(*task.Deadline) {
				updates["deadline"] = nil
			}
		}
		ok, err = transitionTask(task.Id, []string{model.TaskFailed, model.TaskBudgetExhausted}, updates)
		if ok {
			_ = enqueueTaskMessage(task.Id, "retry", "【系统】用户要求重试。先核对现场和已完成步骤，再继续未完成的部分；上次的错误："+clipRunes(task.LastError+task.Blocker, 400), fmt.Sprintf("t%d:retry:%d", task.Id, task.Revision))
			recordTaskEvent(task.Id, "retry_by_user", "用户重试任务", nil)
		}
	case "input":
		msg := strings.TrimSpace(in.Message)
		if msg == "" {
			return task, errors.New("请填写补充信息")
		}
		ok, err = transitionTask(task.Id, []string{model.TaskWaitingHuman, model.TaskPaused},
			map[string]any{"status": model.TaskRunning, "stop_reason": "human_input", "blocker": "", "next_run_at": now})
		if ok {
			_ = enqueueTaskMessage(task.Id, "human", "【用户补充】"+msg+"\n\n已处理你之前提出的阻碍，请继续执行。", fmt.Sprintf("t%d:human:%d", task.Id, task.Revision))
			recordTaskEvent(task.Id, "human_input", clipRunes(msg, 500), nil)
		}
	case "review":
		if in.Approve == nil {
			return task, errors.New("需要 approve=true/false")
		}
		if *in.Approve {
			v := taskLastVerification(task)
			if v != nil {
				for i := range v.Items {
					if v.Items[i].Verdict == verdictReview {
						v.Items[i].Verdict, v.Items[i].By, v.Items[i].Reason = verdictPass, "user", "用户审核通过"
					}
				}
				v.Overall = verdictPass
			}
			raw, _ := json.Marshal(v)
			ok, err = transitionTask(task.Id, []string{model.TaskReview}, map[string]any{"status": model.TaskDone, "stop_reason": "approved", "verification": string(raw), "blocker": ""})
			if ok {
				recordTaskEvent(task.Id, "approved", "用户审核通过，任务完成", nil)
				notifyTask(task, model.SAPurposeResult, "人工审核通过，任务已完成。")
			}
		} else {
			reason := strings.TrimSpace(in.Message)
			if reason == "" {
				reason = "用户审核未通过"
			}
			ok, err = transitionTask(task.Id, []string{model.TaskReview}, map[string]any{"status": model.TaskRunning, "stop_reason": "review_rejected", "next_run_at": now, "blocker": ""})
			if ok {
				_ = enqueueTaskMessage(task.Id, "review", "【人工审核未通过】"+reason+"\n请据此修正，完成后重新 task_submit。", fmt.Sprintf("t%d:review:%d", task.Id, task.Revision))
				recordTaskEvent(task.Id, "rejected", reason, nil)
			}
		}
	default:
		return task, errors.New("未知操作：" + action)
	}
	if err != nil {
		return task, err
	}
	if !ok {
		return task, fmt.Errorf("当前状态（%s）不能执行「%s」", taskStatusLabel(task.Status), action)
	}
	kickTaskScheduler()
	return loadTask(task.Id)
}

func taskStatusLabel(s string) string {
	if l, ok := map[string]string{
		model.TaskRunning: "执行中", model.TaskVerifying: "验收中", model.TaskReview: "待人工审核",
		model.TaskWaitingHuman: "等待人工", model.TaskPaused: "已暂停", model.TaskDone: "已完成",
		model.TaskFailed: "失败", model.TaskCanceled: "已取消", model.TaskBudgetExhausted: "预算耗尽",
	}[s]; ok {
		return l
	}
	return s
}

func taskIDString(id uint) string { return strconv.FormatUint(uint64(id), 10) }
