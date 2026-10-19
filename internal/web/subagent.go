package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 本文件是子 agent 委派的运行器（契约 docs/agent/06-subagent.md §3.3）：
// 把 agent 层「立即返回」的委派请求变成「一条真实子会话 + 一行 run 记录 + 一个后台子轮次」，
// 子轮次跑完再回父会话落通知、按需唤醒父级。
//
// 为什么子轮次不走 SSE：父级的 SSE 流属于父级那一轮，而子任务要在父级收尾之后继续跑。
// 所以子级只把文本收进 run 行，用「通知消息 + 唤醒父级」的方式回到对话里。

const (
	subagentDefaultMaxDepth    = 2
	subagentDefaultMaxChildren = 4
	// subagentReplyMaxRunes 是通知里保留的结论上限（契约 §3.1：Reply 最多 2000 字）。
	subagentReplyMaxRunes = 2000
	// subagentMessageRole 是完成通知在父会话里的消息角色（契约 §3.5，冻结）。
	// 历史重建（web/chat.go）遇到它时当成一条 user 消息喂给父级模型。
	subagentMessageRole = "subagent"
)

// 收尾时写进 stop_reason 的原因：短（列宽 32）且互斥，接口与父模型据此判断结局。
const (
	subagentStopDone          = "done"
	subagentStopNeedInput     = "need_input"
	subagentStopMaxIterations = "max_iterations"
	subagentStopMaxToolCalls  = "max_tool_calls"
	subagentStopCanceled      = "canceled"
	subagentStopTimeout       = "timeout"
	subagentStopError         = "error"
)

// subagentRunner 实现 agent.Delegator（契约 §3.1）。
// 刻意不带内存状态：run 行本身就是状态，list_subagents 读的也是同一份。
type subagentRunner struct{}

func newSubagentRunner() *subagentRunner { return &subagentRunner{} }

// ---------------------------------------------------------------------------
// 环境变量与容量
// ---------------------------------------------------------------------------

// subagentMaxDepth 读 AGENT_MAX_DEPTH，默认 2（根 agent 可派子 agent，子 agent 可再派一层）。
func subagentMaxDepth() int {
	if n := agentIntEnv("AGENT_MAX_DEPTH"); n > 0 {
		return n
	}
	return subagentDefaultMaxDepth
}

// subagentMaxChildren 读 AGENT_MAX_CHILDREN，默认 4。
func subagentMaxChildren() int {
	if n := agentIntEnv("AGENT_MAX_CHILDREN"); n > 0 {
		return n
	}
	return subagentDefaultMaxChildren
}

// subagentAtCapacity 判断 running 个在跑的子任务是否已到 max 上限。
// max <= 0 视为默认上限：调用方漏传时按默认值收紧，而不是「上限 0」把一切派活都拒掉。
func subagentAtCapacity(running, max int) bool {
	if max <= 0 {
		max = subagentDefaultMaxChildren
	}
	return running >= max
}

// runningChildren 统计一棵根会话树下还在跑的委派数（容量按 root 统计，契约 §2）。
// 查询失败时按「已满员」处理：宁可拒一次派活，也不让容量失控。
func runningChildren(rootID uint) int {
	if model.DB == nil || rootID == 0 {
		return 0
	}
	var n int64
	if err := model.DB.Model(&model.AgentSubagentRun{}).
		Where("root_conversation_id = ? AND status = ?", rootID, model.SubagentRunning).
		Count(&n).Error; err != nil {
		log.Printf("[subagent] 统计运行中子任务失败（按满员处理）：%v", err)
		return subagentMaxChildren()
	}
	return int(n)
}

// runningSiblingsFor 统计某父会话下还在跑的子任务数，用于判断「这是不是最后一个收尾的」。
// 查询失败时按「还有别的在跑」处理：少唤醒一次只是慢一拍，重复唤醒会打断父级。
func runningSiblingsFor(parentID uint) int {
	if model.DB == nil || parentID == 0 {
		return 0
	}
	var n int64
	if err := model.DB.Model(&model.AgentSubagentRun{}).
		Where("parent_conversation_id = ? AND status = ?", parentID, model.SubagentRunning).
		Count(&n).Error; err != nil {
		log.Printf("[subagent] 统计父会话运行中子任务失败：%v", err)
		return 1
	}
	return int(n)
}

// subagentWakePending 记「这个父会话有一批子任务在跑、跑完要唤醒一次」。
//
// 为什么需要它：多个子任务并发收尾时，每个收尾者都会看到 running=0（自己已落终态），
// 只靠计数会把父级唤醒多次。用一个「领走」标记保证一批只唤醒一次：
// 派活时置位，第一个看到「没有别的在跑」的收尾者取走它，其余收尾者直接跳过。
var (
	subagentWakeMu      sync.Mutex
	subagentWakePending = map[uint]bool{}
)

func markSubagentBatch(parentID uint) {
	subagentWakeMu.Lock()
	subagentWakePending[parentID] = true
	subagentWakeMu.Unlock()
}

func claimSubagentWake(parentID uint) bool {
	subagentWakeMu.Lock()
	defer subagentWakeMu.Unlock()
	if !subagentWakePending[parentID] {
		return false
	}
	if runningSiblingsFor(parentID) > 0 {
		return false
	}
	delete(subagentWakePending, parentID)
	return true
}

// ---------------------------------------------------------------------------
// agent.Delegator
// ---------------------------------------------------------------------------

// Delegate 受理一次委派：建子会话与 run 行后**立即返回**，子轮次在后台 goroutine 里跑。
func (r *subagentRunner) Delegate(ctx context.Context, req agent.SubagentRequest) (agent.SubagentHandle, error) {
	if model.DB == nil {
		return agent.SubagentHandle{}, errors.New("数据库未初始化，无法派发子任务")
	}
	parentID, err := parseSubagentConvID(req.ParentSessionID)
	if err != nil {
		return agent.SubagentHandle{}, fmt.Errorf("父会话 id 不合法：%w", err)
	}
	depth := req.Depth + 1
	if depth > subagentMaxDepth() {
		return agent.SubagentHandle{}, fmt.Errorf("子 agent 最多嵌套 %d 层，这一层不能再派活", subagentMaxDepth())
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return agent.SubagentHandle{}, errors.New("子任务内容不能为空")
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "子任务"
	}

	// 父会话是权威来源：UserID 从它取（SubagentRequest 里没有用户身份），
	// 顺便把「父会话真的存在」也校验掉。
	var parent model.Conversation
	if err := model.DB.Where("id = ?", parentID).First(&parent).Error; err != nil {
		return agent.SubagentHandle{}, fmt.Errorf("父会话不存在：%w", err)
	}

	// 根会话 id 由工具给出（缺省按父会话算）；父会话自己可能也是子会话（嵌套派活），
	// 这时以它那一行的根为准，保证整棵树的容量统计落在同一个池子里。
	rootID, err := parseSubagentConvID(req.RootSessionID)
	if err != nil {
		rootID = parentID
	}
	var parentRun model.AgentSubagentRun
	if err := model.DB.Where("child_conversation_id = ?", parentID).First(&parentRun).Error; err == nil && parentRun.RootConversationID != 0 {
		rootID = parentRun.RootConversationID
	}

	// 模型与 kind 继承父级（契约 §3.3 第 2 条）：工具的字段可能没填，
	// 所以这里以父会话为准兜底，避免子轮次拿空模型去调用、直接跑成 failed。
	if strings.TrimSpace(req.Model) == "" {
		req.Model = strings.TrimSpace(parent.Model)
	}
	if strings.TrimSpace(req.Kind) == "" {
		req.Kind = parent.Kind
	}
	// kind 只认现有四值：不造新 kind，normalizeKind 会把未知值吃成 work。
	req.Kind = normalizeKind(req.Kind)
	kind := req.Kind

	if subagentAtCapacity(runningChildren(rootID), subagentMaxChildren()) {
		return agent.SubagentHandle{}, fmt.Errorf("正在运行的子任务已达上限（%d 个），等它们跑完再派", subagentMaxChildren())
	}
	child := model.Conversation{UserID: parent.UserID, Title: label, Model: req.Model, Kind: kind}
	if err := model.DB.Create(&child).Error; err != nil {
		return agent.SubagentHandle{}, fmt.Errorf("创建子会话失败：%w", err)
	}

	run := newSubagentRun(parent, child.Id, parentID, rootID, depth, label, prompt, req, kind)
	if err := model.DB.Create(&run).Error; err != nil {
		// 没有 run 行的子会话是条空壳，直接清掉，别在侧栏留垃圾。
		model.DB.Delete(&model.Conversation{}, child.Id)
		return agent.SubagentHandle{}, fmt.Errorf("创建子任务记录失败：%w", err)
	}
	// 子会话要能在运行中打开：先落任务输入，结论会在 settle 时追加。
	saveChatMessage(child.Id, "user", prompt)
	touchConversation(child.Id)

	// 子轮次必须活过父请求：工具的 ctx 随父级这一轮结束而取消，
	// 直接把它传进 goroutine 会让子任务在父级收尾时被顺手杀掉。
	if ctx == nil {
		ctx = context.Background()
	}
	bg := context.WithoutCancel(ctx)
	// 先置位再起 goroutine：子轮次可能瞬间失败（比如模型未配置），
	// 置位晚了就会出现「跑完了但没人唤醒父级」。
	markSubagentBatch(parentID)
	go r.runChild(bg, run, child.Id, parentID, req, depth)

	return agent.SubagentHandle{
		ChildID: strconv.FormatUint(uint64(child.Id), 10),
		Label:   label,
		Depth:   depth,
	}, nil
}

func newSubagentRun(parent model.Conversation, childID, parentID, rootID uint, depth int, label, prompt string, req agent.SubagentRequest, kind string) model.AgentSubagentRun {
	return model.AgentSubagentRun{
		UserID:               parent.UserID,
		ParentConversationID: parentID,
		ChildConversationID:  childID,
		RootConversationID:   rootID,
		Depth:                depth,
		Label:                label,
		Name:                 strings.TrimSpace(req.Name),
		Prompt:               prompt,
		ContextMode:          normalizeSubagentContext(req.Context),
		Status:               model.SubagentRunning,
		Activity:             "正在启动子 agent",
		Model:                req.Model,
		Kind:                 kind,
	}
}

// ListSubagents 返回某父会话下所有子任务的一行行状态（list_subagents 工具用）。
func (r *subagentRunner) ListSubagents(ctx context.Context, parentSessionID string) ([]agent.SubagentInfo, error) {
	parentID, err := parseSubagentConvID(parentSessionID)
	if err != nil {
		return nil, fmt.Errorf("父会话 id 不合法：%w", err)
	}
	runs, err := subagentRunsFor(parentID)
	if err != nil {
		return nil, err
	}
	out := make([]agent.SubagentInfo, 0, len(runs))
	for _, run := range runs {
		out = append(out, agent.SubagentInfo{
			ChildID:    strconv.FormatUint(uint64(run.ChildConversationID), 10),
			Label:      run.Label,
			Status:     run.Status,
			StopReason: run.StopReason,
			Depth:      run.Depth,
			Reply:      run.Reply,
			Error:      run.ErrText,
			StartedAt:  run.CreatedAt,
			FinishedAt: run.FinishedAt,
		})
	}
	return out, nil
}

// subagentRunsFor 按父会话列出 run 行（HTTP 接口与工具共用同一份数据）。
func subagentRunsFor(convID uint) ([]model.AgentSubagentRun, error) {
	if model.DB == nil {
		return nil, errors.New("数据库未初始化")
	}
	var runs []model.AgentSubagentRun
	if err := model.DB.Where("parent_conversation_id = ?", convID).Order("id asc").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// ---------------------------------------------------------------------------
// 后台子轮次
// ---------------------------------------------------------------------------

// runChild 跑完一个子轮次并把结局落库、通知父会话、按需唤醒父级。
func (r *subagentRunner) runChild(ctx context.Context, run model.AgentSubagentRun, childID, parentID uint, req agent.SubagentRequest, depth int) {
	started := time.Now()
	status, stopReason, reply, errText := r.executeChild(ctx, req, depth, run.UserID, run.Id, childID, parentID)
	r.settle(run, parentID, childID, started, status, stopReason, reply, errText)
}

// executeChild 是子轮次本体：组装工具集与 Exec、跑 agent.Run、把结果折成终态。
// 返回值都是「可直接落库」的形态，异常路径也一定有终态——绝不静默丢弃一次子任务。
func (r *subagentRunner) executeChild(ctx context.Context, req agent.SubagentRequest, depth int, userID string, runID, childID, parentID uint) (status, stopReason, reply, errText string) {
	// 后台 goroutine 里的 panic 会直接带崩进程；兜住并把 run 行落成 failed，
	// 否则这一行会永远停在 running，容量也被永久占掉。
	defer func() {
		if rec := recover(); rec != nil {
			status, stopReason, reply = model.SubagentFailed, subagentStopError, ""
			errText = fmt.Sprintf("子任务内部错误：%v", rec)
		}
	}()

	var (
		acc       strings.Builder
		needInput string
		activity  = "正在启动子 agent"
		lastWrite time.Time
		lastBytes int
	)
	persistProgress := func(nextActivity string, force bool) {
		now := time.Now()
		if !force && !lastWrite.IsZero() && now.Sub(lastWrite) < 700*time.Millisecond && acc.Len()-lastBytes < 256 {
			return
		}
		if strings.TrimSpace(nextActivity) != "" {
			activity = clipRunes(strings.TrimSpace(nextActivity), 240)
		}
		if model.DB != nil {
			model.DB.Model(&model.AgentSubagentRun{}).Where("id = ? AND status = ?", runID, model.SubagentRunning).
				Updates(map[string]any{
					"progress":   clipRunes(acc.String(), subagentReplyMaxRunes),
					"activity":   activity,
					"updated_at": now,
				})
		}
		lastWrite = now
		lastBytes = acc.Len()
	}
	emit := func(ev agent.Event) {
		switch ev.Kind {
		case agent.EvDelta:
			acc.WriteString(ev.Content)
			persistProgress("正在生成回复", false)
		case agent.EvToolCall:
			if ev.ToolCall != nil {
				persistProgress("正在调用工具："+ev.ToolCall.Name, true)
			}
		case agent.EvToolResult:
			detail := strings.TrimSpace(ev.Summary)
			if detail == "" {
				detail = "已返回结果"
			}
			persistProgress(ev.ToolName+"："+detail, true)
		case agent.EvDone:
			// 与 runAgentChat 同一套收集口径：done 只在没有增量文本时补一次，避免重复。
			if ev.Content != "" && acc.Len() == 0 {
				acc.WriteString(ev.Content)
			}
		case agent.EvNeedInput:
			// 子级没人可问：把问题当成它的结论，交给父级决定怎么向用户转达。
			needInput = strings.TrimSpace(ev.Content)
			persistProgress("需要主 agent 处理一个问题", true)
		}
	}

	parentHistory := req.History
	if normalizeSubagentContext(req.Context) == "fork" {
		parentHistory = subagentForkHistory(parentID, parentHistory)
	}
	history := make([]llm.Message, 0, len(parentHistory)+1)
	history = append(history, parentHistory...)
	history = append(history, llm.Message{Role: llm.RoleUser, Content: req.Prompt})

	cfg := agent.Config{
		Model:               req.Model,
		APIKey:              subagentAPIKey(userID, req.Model),
		SystemPrompt:        subagentSystemPrompt(req),
		MaxIterations:       subagentMaxIterations(),
		MaxToolCalls:        subagentMaxToolCalls(),
		LimitRecoveryRounds: agentLimitRecoveryRounds(req.Mode),
		Tools:               subagentToolsFor(userID, req, depth),
		Exec: &agent.Exec{
			// SessionID 用**子会话 id**：agent/fs 的观察表按 SessionID 存，
			// 复用父会话 id 会让父子的文件读写互相踩锁。
			SessionID: strconv.FormatUint(uint64(childID), 10),
			Root:      req.Root,
			Roots:     append([]string(nil), req.Roots...),
			ReadRoot:  agentSkillsDir(),
			// auto 子 agent 与根 agent 使用相同的外部绝对路径策略；plan/confirm
			// 下的写操作已由 subagentDenyTool 拒绝，不会因此绕过确认。
			AllowOutside: subagentAllowOutside(req.Mode),
		},
	}
	if agentUsesEino() {
		cm, modelErr := registry.NewToolCallingModel(ctx, req.Model, cfg.APIKey, llm.ChatRequest{})
		if modelErr != nil {
			return model.SubagentFailed, subagentStopError, "", modelErr.Error()
		}
		cfg.EinoModel = cm
	}
	runErr := agent.Run(ctx, cfg, history, emit)

	reply = strings.TrimSpace(acc.String())
	if runErr != nil {
		status, stopReason = classifySubagentOutcome(runErr)
		return status, stopReason, clipRunes(reply, subagentReplyMaxRunes), runErr.Error()
	}
	if needInput != "" {
		if reply == "" {
			reply = needInput
		}
		return model.SubagentDone, subagentStopNeedInput, clipRunes(reply, subagentReplyMaxRunes), ""
	}
	return model.SubagentDone, subagentStopDone, clipRunes(reply, subagentReplyMaxRunes), ""
}

// settle 是收尾顺序（契约 §3.3 第 5 条，顺序冻结）：
// 更新 run 行 → 往父会话落通知消息 → touch 父会话 → 按需唤醒父级。
func (r *subagentRunner) settle(run model.AgentSubagentRun, parentID, childID uint, started time.Time, status, stopReason, reply, errText string) {
	if model.DB == nil {
		return
	}
	finished := time.Now()
	activity := terminalSubagentActivity(status)
	if status == model.SubagentFailed {
		var latest model.AgentSubagentRun
		if err := model.DB.Select("activity").First(&latest, run.Id).Error; err == nil && strings.TrimSpace(latest.Activity) != "" {
			activity += "（最后一步：" + clipRunes(strings.TrimSpace(latest.Activity), 120) + "）"
		}
	}
	if err := model.DB.Model(&model.AgentSubagentRun{}).Where("id = ?", run.Id).Updates(map[string]any{
		"status":      status,
		"stop_reason": stopReason,
		"reply":       reply,
		"progress":    "",
		"activity":    activity,
		"err_text":    errText,
		"finished_at": finished,
		"updated_at":  finished,
	}).Error; err != nil {
		log.Printf("[subagent] 更新 run 行失败（id=%d，行可能停在 running 并占住容量）：%v", run.Id, err)
	}

	// 子会话本身也要能点开看：任务已在委派时落库，收尾时追加最终结论。
	if reply != "" {
		saveChatMessage(childID, "assistant", reply)
	}
	touchConversation(childID)

	// 通知消息的正文就是 LLMText 的纯文本；结构化字段不塞 content（契约 §3.5），
	// 前端从 GET /api/conversations/:id/subagents 拿同一份 run 行。
	notice := agent.SubagentNotice{
		ChildID:    strconv.FormatUint(uint64(childID), 10),
		Label:      run.Label,
		Status:     status,
		StopReason: stopReason,
		Reply:      reply,
		Error:      errText,
		DurationMS: finished.Sub(started).Milliseconds(),
	}
	saveChatMessage(parentID, subagentMessageRole, subagentNoticeContent(notice))
	touchConversation(parentID)

	// 这一批子任务全跑完才唤醒父级；claim 保证并发收尾时只唤醒一次。
	if claimSubagentWake(parentID) {
		wakeParentAfterSubagents(parentID)
	}
}

func terminalSubagentActivity(status string) string {
	switch status {
	case model.SubagentDone:
		return "任务已完成"
	case model.SubagentAborted:
		return "任务已中止"
	default:
		return "任务失败"
	}
}

// ---------------------------------------------------------------------------
// 组装与文案（纯函数，单测钉住）
// ---------------------------------------------------------------------------

// classifySubagentOutcome 把子轮次的返回错误折成 run 行的终态。
// 取消/超时 → aborted，其余（模型失败、触顶）→ failed：两者都必须落库，不能静默丢弃。
func classifySubagentOutcome(err error) (status, stopReason string) {
	switch {
	case err == nil:
		return model.SubagentDone, subagentStopDone
	case errors.Is(err, context.Canceled):
		return model.SubagentAborted, subagentStopCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return model.SubagentAborted, subagentStopTimeout
	case errors.Is(err, agent.ErrMaxIterations):
		return model.SubagentFailed, subagentStopMaxIterations
	case errors.Is(err, agent.ErrMaxToolCalls):
		return model.SubagentFailed, subagentStopMaxToolCalls
	default:
		return model.SubagentFailed, subagentStopError
	}
}

// subagentNoticeContent 是落进父会话那条通知消息的正文（契约 §3.5：纯文本，不塞 JSON）。
func subagentNoticeContent(n agent.SubagentNotice) string {
	if s := strings.TrimSpace(n.LLMText()); s != "" {
		return s
	}
	// LLMText 理论上不会返回空串；真为空时也要保证父级被唤醒后能看到线索。
	return fmt.Sprintf("【子任务结束】%s（id=%s）\n状态：%s", n.Label, n.ChildID, n.Status)
}

// normalizeSubagentContext 归一化上下文模式：只认 fork，其余一律 fresh（契约 §3.1）。
func normalizeSubagentContext(m string) string {
	if strings.EqualFold(strings.TrimSpace(m), "fork") {
		return "fork"
	}
	return "fresh"
}

// subagentForkHistory 在 fork 模式但调用方没带历史时，回落到「按父会话消息重建」。
// fork 是显式选择的上下文模式，回落成空历史会让子级莫名失忆，不如按父会话补齐。
func subagentForkHistory(parentID uint, provided []llm.Message) []llm.Message {
	if len(provided) > 0 || parentID == 0 || model.DB == nil {
		return provided
	}
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ?", parentID).Order("id asc").Find(&rows).Error; err != nil {
		log.Printf("[subagent] fork 模式读取父会话历史失败（按 fresh 跑）：%v", err)
		return nil
	}
	return rebuildLLMHistory(rows)
}

func parseSubagentConvID(s string) (uint, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("会话 id %q 不合法", s)
	}
	return uint(n), nil
}

// 子 agent 只有 auto 档可以自行写文件，且需遵循与父 agent 相同的外部路径限制。
func subagentAllowOutside(mode string) bool { return normalizeMode(mode) == ModeAuto }

// 子任务通常需要“读任务 → 看文件 → 修改 → 验证 → 汇报”多轮工具往返。
// 复用主会话较小的轮数上限会让这类任务在交付前触顶；保留显式子任务配置，
// 未配置时至少给 24 轮，同时尊重主会话显式设得更高的上限。
func subagentMaxIterations() int {
	if n := agentIntEnv("AGENT_SUBAGENT_MAX_ITERATIONS"); n > 0 {
		return n
	}
	if n := agentIntEnv("AGENT_MAX_ITERATIONS"); n > 24 {
		return n
	}
	return 24
}

// 子任务经常需要多次读写与验证；工具调用预算与模型轮数分别限流，
// 因此也给子任务独立预算，避免模型轮数已放宽但仍被主会话的 24 次工具上限截断。
func subagentMaxToolCalls() int {
	if n := agentIntEnv("AGENT_SUBAGENT_MAX_TOOL_CALLS"); n > 0 {
		return n
	}
	if n := agentIntEnv("AGENT_MAX_TOOL_CALLS"); n > 48 {
		return n
	}
	return 48
}

// subagentAPIKey 给子轮次取模型 key：与父级同一套解析（用户自填 key → 环境变量）。
// registry/DB 未就绪时返回空串，让模型后端自己报「缺 key」——失败会落进 run 行，不会被吞。
func subagentAPIKey(userID, modelID string) string {
	if model.DB == nil || registry == nil {
		return ""
	}
	ep, ok := registry.FindByModel(modelID)
	if !ok {
		return ""
	}
	return resolveKey(userID, ep)
}

// subagentSystemPrompt 是子级的系统提示词：通用能力说明 + 会话模式说明 + 子级边界。
// 刻意不拼 modePrompt（「风险操作先确认」对子级不成立——它没有可以确认的用户），
// 改用下面这段把「风险工具会被直接拒绝」说清楚。
func subagentSystemPrompt(req agent.SubagentRequest) string {
	toolGuidance := "先用 read/list 等文件工具检查任务目录，再用 write/edit 等文件工具直接完成分配的文件工作；" +
		"run_command 仅用于安装依赖、构建或运行验证，不要把 npm 命令当成文件读写的替代方式。"
	if normalizeMode(req.Mode) != ModeAuto {
		toolGuidance = "先用 read/list 等只读工具检查任务目录；如果写操作被权限拦截，不要反复重试，说明需要主 agent 执行的具体文件操作。"
	}
	toolGuidance += "如果 npm 报网络不可用，先用 npm --offline --no-audit --no-fund 安装（仅当依赖在本地缓存或项目没有依赖时），构建可用 npm --offline run build；缓存缺少依赖时停止并报告，不要换 pnpm 反复探测。" +
		"其他命令失败时，根据错误信息定位一次；确认是环境缺失或路径问题后停止重复探测，把具体错误交给主 agent。"
	prompt := agentSystemPrompt(req.Root, "")
	if len(req.Roots) > 0 {
		prompt += "\n【多个活动工作区】普通相对路径默认在 workspace1；访问其他目录时使用 @workspaceN/相对路径。目录如下：\n"
		for i, root := range append([]string{req.Root}, req.Roots...) {
			prompt += fmt.Sprintf("- workspace%d: %s\n", i+1, root)
		}
	}
	return prompt +
		profileFor(req.Kind).Prompt +
		"\n【你是子 agent】你由主 agent 派出，独立完成交给你的这一件事。" +
		toolGuidance +
		"没有人会在中途回答你：需要用户确认的操作会被直接拒绝，把这类事写进结论交给主 agent 处理。" +
		"完成后用简洁的中文给出最终结论，不要复述任务原文。\n"
}
