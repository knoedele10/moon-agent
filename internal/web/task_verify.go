package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 宿主验收（docs/agent/16-task-runtime.md §4）：执行器 task_submit 之后，
// 宿主逐项核对验收条件，而不是采信模型的「完成了」。
//
//   - command：在任务工作区经 run_command 同一套白名单与沙箱运行检查命令，退出码 0 才算通过；
//   - auto：用独立的、不带工具的模型调用当裁判，只看证据与本任务的工具流水（确定性事实）；
//     裁判拿不准时判 review，交给人；
//   - manual：一律进入待审。
//
// 任一项失败 → 失败项与原因交回原执行对话修正（计入 verify_attempts，超限判失败）；
// 无失败但有待审 → review；全部通过 → done。

const taskJudgePrompt = `你是严格的验收员。只根据给出的证据和工具执行记录判断一条验收条件是否已满足。
- 证据必须具体且与执行记录一致；只有声明、没有可核对的事实，判 fail。
- 执行记录显示相关步骤失败或没有执行，判 fail。
- 条件本身需要人的主观判断（审美、业务取舍）或证据不足以判断时，判 review。
只输出一行 JSON：{"verdict":"pass|fail|review","reason":"一句话理由"}`

// taskLLMCall 是裁判使用的无工具模型调用；走 agent 的模型缝，测试可替换。
var taskLLMCall = func(ctx context.Context, modelID, key, system, user string) (string, error) {
	stream := agent.Streamer()
	if stream == nil {
		return "", agent.ErrNoStreamer
	}
	ch, err := stream(ctx, modelID, key, llm.ChatRequest{Model: modelID, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: system}, {Role: llm.RoleUser, Content: user},
	}})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for ev := range ch {
		switch ev.Type {
		case llm.EventDelta:
			b.WriteString(ev.Content)
		case llm.EventError:
			return "", ev.Err
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// taskCommandRunner 运行 command 验收；默认复用 run_command 工具（同一白名单、同一沙箱）。
var taskCommandRunner = func(ctx context.Context, t model.AgentTask, c taskCheck) (bool, string) {
	roots := taskRoots(t)
	if len(roots) == 0 {
		roots = teamWorkspaceRootsFor(t.UserID, t.Kind)
	}
	if firstWorkspaceRoot(roots) == "" {
		return false, "任务没有工作区，无法运行检查命令"
	}
	cwd := strings.TrimSpace(c.CWD)
	if cwd == "" {
		cwd = "."
	}
	args, _ := json.Marshal(map[string]any{"cwd": cwd, "commands": []map[string]any{{"program": c.Program, "args": c.Args}}})
	res, err := runCommandTool{}.Execute(ctx, args, &agent.Exec{
		SessionID: "task-verify-" + taskIDString(t.Id), Root: firstWorkspaceRoot(roots), Roots: additionalWorkspaceRoots(roots),
	})
	if err != nil {
		return false, err.Error()
	}
	return !res.IsError, clipRunes(strings.TrimSpace(res.Content), 800)
}

func taskMaxVerifyAttempts() int {
	return agentIntEnvOr("AGENT_TASK_MAX_VERIFY_ATTEMPTS", taskMaxVerifyAttemptsDflt)
}

// taskJournalFacts 汇总该任务最近几轮的工具流水，作为裁判可依据的确定性事实。
func taskJournalFacts(taskID uint) string {
	var rounds []model.AgentTaskRound
	model.DB.Where("task_id = ?", taskID).Order("id desc").Limit(4).Find(&rounds)
	var b strings.Builder
	for i := len(rounds) - 1; i >= 0; i-- {
		var j []taskJournalEntry
		_ = json.Unmarshal([]byte(rounds[i].Journal), &j)
		for _, e := range j {
			if strings.HasPrefix(e.Name, "task_") {
				continue
			}
			fmt.Fprintf(&b, "- 第%d轮 %s %s → %s：%s\n", rounds[i].No, e.Name, clipRunes(e.Args, 160), e.Status, clipRunes(e.Summary, 200))
		}
	}
	s := b.String()
	if s == "" {
		return "（没有工具执行记录）"
	}
	return clipRunes(s, 6000)
}

func parseJudgeVerdict(out string) (string, string) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return verdictReview, "裁判输出无法解析，交人工判断"
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case verdictPass:
		return verdictPass, v.Reason
	case verdictFail:
		return verdictFail, v.Reason
	}
	return verdictReview, v.Reason
}

// verifyTaskSubmission 逐项核对，返回验收结果；err 非空表示验收过程本身失败（应退避重试）。
func verifyTaskSubmission(ctx context.Context, t model.AgentTask, sub taskSubmission) (taskVerification, error) {
	ev := map[string]string{}
	for _, it := range sub.Items {
		ev[it.Criterion] = it.Evidence
	}
	facts := ""
	key := subagentAPIKey(t.UserID, t.Model)
	result := taskVerification{Attempt: t.VerifyAttempts + 1, Round: sub.Round, At: time.Now()}
	for _, c := range taskCriteria(t) {
		v := taskVerdict{Criterion: c.ID, Text: c.Text, Method: c.Method}
		switch c.Method {
		case criterionManual:
			v.Verdict, v.By, v.Reason = verdictReview, "host", "该条件需要人工确认"
			if e := ev[c.ID]; e != "" {
				v.Reason += "；执行器证据：" + clipRunes(e, 300)
			}
		case criterionCommand:
			ok, out := taskCommandRunner(ctx, t, *c.Check)
			v.By, v.Reason = "command", out
			v.Verdict = verdictFail
			if ok {
				v.Verdict = verdictPass
			}
		default:
			e := strings.TrimSpace(ev[c.ID])
			if e == "" {
				v.Verdict, v.By, v.Reason = verdictFail, "host", "没有提交该条件的证据"
				break
			}
			if facts == "" {
				facts = taskJournalFacts(t.Id)
			}
			prompt := fmt.Sprintf("## 任务目标\n%s\n## 约束\n%s\n## 待判定的验收条件\n[%s] %s\n## 执行器提交的总结\n%s\n## 该条件的证据\n%s\n## 宿主记录的工具执行流水\n%s",
				clipRunes(t.Goal, 3000), clipRunes(t.Constraints, 1500), c.ID, c.Text, clipRunes(sub.Summary, 1500), e, facts)
			out, err := taskLLMCall(ctx, t.Model, key, taskJudgePrompt, prompt)
			if err != nil {
				return result, fmt.Errorf("验收裁判调用失败：%w", err)
			}
			v.Verdict, v.Reason = parseJudgeVerdict(out)
			v.By = "judge"
		}
		result.Items = append(result.Items, v)
	}
	result.Overall = verdictPass
	for _, it := range result.Items {
		if it.Verdict == verdictFail {
			result.Overall = verdictFail
			break
		}
		if it.Verdict == verdictReview {
			result.Overall = verdictReview
		}
	}
	return result, nil
}

func runTaskVerification(parent context.Context, task model.AgentTask) {
	sub := taskLastSubmission(task)
	if sub == nil {
		// 没有提交却在验收态（不应出现）：退回执行。
		transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{"status": model.TaskRunning, "next_run_at": time.Now()})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Minute)
	defer cancel()
	taskRunMu.Lock()
	taskRunCancels[task.Id] = cancel
	taskRunMu.Unlock()
	defer func() {
		taskRunMu.Lock()
		delete(taskRunCancels, task.Id)
		taskRunMu.Unlock()
	}()
	result, err := verifyTaskSubmission(ctx, task, *sub)
	if err != nil {
		if cur, e := loadTask(task.Id); e == nil && cur.Status == model.TaskVerifying {
			failTaskStep(task.Id, err, isTransientTaskError(err) || ctx.Err() == nil)
		}
		return
	}
	raw, _ := json.Marshal(result)
	recordTaskEvent(task.Id, "verified", "验收结果："+result.Overall, result)
	switch result.Overall {
	case verdictPass:
		// 验收期间又来了后续消息（前台可能已经认领并写进 outbox）：不能就这么结束，
		// 否则那条消息会永远停在待投递里。回到执行轮先把它处理掉，再重新验收。
		if pendingTaskOutbox(task.Id) > 0 {
			if ok, _ := transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{
				"status": model.TaskRunning, "stop_reason": "new_input", "verification": string(raw),
				"next_run_at": time.Now(), "consecutive_failures": 0,
			}); ok {
				recordTaskEvent(task.Id, "new_input", "验收期间收到后续消息，先处理再重新验收", nil)
				kickTaskScheduler()
			}
			return
		}
		if ok, _ := transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{
			"status": model.TaskDone, "stop_reason": "verified", "verification": string(raw), "blocker": "",
			"consecutive_failures": 0,
		}); ok {
			notifyTask(task, model.SAPurposeResult, "已通过全部验收条件，任务完成。\n交付概述："+clipRunes(sub.Summary, 1200)+"\n"+verificationText(result))
		}
	case verdictReview:
		if ok, _ := transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{
			"status": model.TaskReview, "stop_reason": "needs_review", "verification": string(raw),
			"blocker": "有验收条件需要人工判断", "consecutive_failures": 0,
		}); ok {
			notifyTask(task, model.SAPurposeQuestion, "自动核对已通过，剩余条件需要人工审核：\n"+verificationText(result))
		}
	default:
		attempts := task.VerifyAttempts + 1
		if attempts >= taskMaxVerifyAttempts() {
			if ok, _ := transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{
				"status": model.TaskFailed, "stop_reason": "verify_exhausted", "verification": string(raw), "verify_attempts": attempts,
				"blocker": fmt.Sprintf("验收连续 %d 次未通过，停止", attempts),
			}); ok {
				recordTaskEvent(task.Id, "failed", fmt.Sprintf("验收 %d 次未通过，停止", attempts), nil)
				notifyTask(task, model.SAPurposeQuestion, fmt.Sprintf("验收连续 %d 次未通过，任务失败：\n%s", attempts, verificationText(result)))
			}
			return
		}
		msg := "【验收未通过（第 " + fmt.Sprint(attempts) + " 次）】以下条件没有满足，请修正后重新验证并 task_submit：\n"
		for _, it := range result.Items {
			if it.Verdict == verdictFail {
				msg += fmt.Sprintf("- [%s] %s：%s\n", it.Criterion, it.Text, it.Reason)
			}
		}
		if err := enqueueTaskMessage(task.Id, "verify_failed", msg, fmt.Sprintf("t%d:verify:%d", task.Id, attempts)); err != nil {
			failTaskStep(task.Id, err, true)
			return
		}
		if ok, _ := transitionTask(task.Id, []string{model.TaskVerifying}, map[string]any{
			"status": model.TaskRunning, "stop_reason": "verify_failed", "verification": string(raw),
			"verify_attempts": attempts, "next_run_at": time.Now(), "consecutive_failures": 0,
		}); ok {
			kickTaskScheduler()
		}
	}
}

func verificationText(v taskVerification) string {
	var b strings.Builder
	icon := map[string]string{verdictPass: "✅", verdictFail: "❌", verdictReview: "🟡"}
	for _, it := range v.Items {
		fmt.Fprintf(&b, "%s [%s] %s — %s\n", icon[it.Verdict], it.Criterion, it.Text, clipRunes(it.Reason, 200))
	}
	return b.String()
}
