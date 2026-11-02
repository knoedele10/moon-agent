package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 本文件是 Superagent 的 C 线：工单（= 长任务 AgentTask）与执行
// （docs/agent/18-superagent.md §3.3 / §3.5 / §3.7）。
//
// 三件事：
//   - saAcceptTicket：把前台的一次 accept 变成带授权的长任务（任务 + Mandate + origin 订阅 + 受理确认）。
//   - saFanOut：后续消息按**显式引用**认领给已有工单（回复某条工单消息 / 文本里写 #id / 唯一在等人的工单），
//     已结束的工单先复核授权再续跑。
//   - saStopTicket / saLiveTickets / saAttachTicket：叫停、给前台看工单卡、把消息交给已订阅的工单。
//
// 授权边界一律由宿主检查：强度、Kind/Mode 上限、规则是否仍启用，都不信任模型输出。

var errSATicketDuplicate = errors.New("superagent: 同一条消息已经被接单")

// init 把 C 线的实现装到跨线契约上（其余工作线通过 saAcceptTicket 等变量调用）。
func init() {
	saFanOut = saFanOutImpl
	saAcceptTicket = saAcceptTicketImpl
	saAttachTicket = saAttachTicketImpl
	saStopTicket = saStopTicketImpl
	saLiveTickets = saLiveTicketsImpl
}

// ---------------------------------------------------------------------------
// 接单
// ---------------------------------------------------------------------------

// saAcceptTicketImpl 建工单。同一 OriginTriggerID 只会有一个任务：唯一索引保证，
// 重复调用返回已有任务（受理确认由发件箱的 DedupeKey 保证只发一次）。
func saAcceptTicketImpl(ctx context.Context, req saAcceptReq) (model.AgentTask, error) {
	if model.DB == nil {
		return model.AgentTask{}, errors.New("数据库未初始化")
	}
	if err := ctx.Err(); err != nil {
		return model.AgentTask{}, err
	}
	if req.Origin.Id == 0 || req.Thread.Id == 0 {
		return model.AgentTask{}, errors.New("接单缺少来源消息或对话")
	}
	if strings.TrimSpace(req.UserID) == "" {
		return model.AgentTask{}, errors.New("接单缺少执行用户")
	}
	if strings.TrimSpace(req.Goal) == "" {
		return model.AgentTask{}, errors.New("接单缺少任务目标")
	}

	// 幂等：同一条消息被接单两次，只返回已有工单，不再新建执行对话。
	if m, ok := saMandateByOrigin(req.Origin.Id); ok {
		task, err := loadTask(m.TaskID)
		if err != nil {
			return model.AgentTask{}, err
		}
		saQueueTicketAck(task, req)
		return task, nil
	}

	var conv model.Conversation
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", req.Thread.ConversationID, req.UserID).
		First(&conv).Error; err != nil {
		return model.AgentTask{}, errors.New("Thread 没有可用的前台会话")
	}
	if strings.TrimSpace(conv.Model) == "" {
		// 入口新建的前台会话（如 QQ 首次私聊）可能还没选模型：沿用前台轮次的选择
		// （该用户最近用过的模型）并写回会话，否则工单没有执行模型可用。
		conv.Model = saFrontModel(&conv)
	}
	kind, mode := saTicketAuthority(req, conv)
	// 聊天档没有执行工具，而长任务至少按辅助工作执行（task_store 的既有规则）。
	// 上限是聊天档时，任何任务的实际配置都会超过这份授权，所以只能拒单；
	// 想给访客派活，由主人把 sa_policy.guest_kind 调高（契约 §3.5 的 Kind 上限）。
	if !saActorIsOwner(req) && normalizeKind(kind) == KindChat {
		return model.AgentTask{}, errors.New("发起人的接单上限是聊天档（guest_kind=chat），不能建工单；请主人调整接单策略")
	}
	roots := kindWorkspaceRootsFor(req.UserID, kind, agentFSRootsFor(req.UserID))
	spec := saTicketSpec(req)
	opt := taskCreateOptions{
		UserID: req.UserID, MainConversationID: conv.Id, Model: conv.Model, Kind: kind, Mode: mode,
		Roots: roots,
		// 命令验收会由宿主直接运行命令：模型代填时只在自主档允许（与 task_manage 一致）。
		AllowCommandChecks: mode == ModeAuto,
	}

	var task model.AgentTask
	var mandate model.SAMandate
	err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		t, err := createTaskTx(tx, spec, opt)
		if err != nil {
			return err
		}
		matched, _ := json.Marshal(req.MatchedRule)
		m := model.SAMandate{
			UserID: req.UserID, TaskID: t.Id, OriginTriggerID: req.Origin.Id,
			ThreadID: req.Thread.Id, Source: req.Thread.Source, ActorID: req.Origin.ActorID,
			Strength: req.Strength, Scope: clipRunes(req.Goal, 4000),
			// Kind/Mode 记任务实际生效的配置（createTask 会把闲聊档抬到 work）。
			// 它是这份授权允许的上限，续跑与复核都以它为准。
			Kind: t.Kind, Mode: t.Mode, MatchedRules: string(matched),
			PolicyVersion: req.Policy.Version,
		}
		if err := tx.Create(&m).Error; err != nil {
			if isSAUniqueViolation(err) {
				return errSATicketDuplicate
			}
			return err
		}
		mandate = m
		sub := model.SASubscription{TaskID: t.Id, ThreadID: req.Thread.Id, Role: model.SASubOrigin}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&sub).Error; err != nil {
			return err
		}
		task = t
		return nil
	})
	if errors.Is(err, errSATicketDuplicate) {
		m, ok := saMandateByOrigin(req.Origin.Id)
		if !ok {
			return model.AgentTask{}, errors.New("接单冲突，但找不到已有工单")
		}
		existing, e := loadTask(m.TaskID)
		if e != nil {
			return model.AgentTask{}, e
		}
		saQueueTicketAck(existing, req)
		return existing, nil
	}
	if err != nil {
		return model.AgentTask{}, err
	}

	recordTaskEvent(task.Id, "ticket_accepted",
		fmt.Sprintf("接单：来源 %s，发起人 %s，强度 %s，授权 %s/%s（策略 v%d）",
			taskSourceLabel(mandate.Source), clipRunes(mandate.ActorID, 40), mandate.Strength,
			mandate.Kind, mandate.Mode, mandate.PolicyVersion),
		map[string]any{"origin_trigger_id": req.Origin.Id, "matched_rules": req.MatchedRule})
	kickTaskScheduler()
	saQueueTicketAck(task, req)
	return task, nil
}

// saQueueTicketAck 把受理确认交给发件箱。失败只记日志：任务已经落库，
// DedupeKey 让重试不会重复发（docs/agent/18-superagent.md §3.5）。
func saQueueTicketAck(task model.AgentTask, req saAcceptReq) {
	ack := strings.TrimSpace(req.Ack)
	if ack == "" {
		ack = "收到，我来处理这件事。"
	}
	text := fmt.Sprintf("%s（任务 #%d，回复这条消息可以补充要求）", clipRunes(ack, 100), task.Id)
	_, err := saEnqueueOutbound(saOutboundReq{
		UserID: task.UserID, ThreadID: req.Thread.Id, TaskID: task.Id,
		Purpose: model.SAPurposeAck, Text: text,
		ReplyToTriggerID: req.Origin.Id,
		DedupeKey:        fmt.Sprintf("task:%d:ack", task.Id),
	})
	if err != nil {
		log.Printf("[sa] 工单 %d 的受理确认入队失败：%v", task.Id, err)
		recordTaskEvent(task.Id, "outbound_error", "受理确认入队失败："+err.Error(), nil)
	}
}

// saTicketSpec 把路由给的接单请求变成任务规格。没有验收条件时退化成一条人工审核条件：
// 不替用户编造「自动通过」的条件，宁可由人确认（docs/agent/18-superagent.md §0）。
func saTicketSpec(req saAcceptReq) taskSpec {
	criteria := make([]taskCriterion, 0, len(req.Criteria))
	for _, c := range req.Criteria {
		c.Text = strings.TrimSpace(c.Text)
		if c.Text != "" {
			criteria = append(criteria, c)
		}
	}
	if len(criteria) == 0 {
		criteria = append(criteria, taskCriterion{
			Text: "目标达成：" + clipRunes(strings.TrimSpace(req.Goal), 160), Method: criterionManual,
		})
	}
	return taskSpec{Title: req.Title, Goal: req.Goal, Constraints: req.Constraints, Criteria: criteria}
}

// 档位高低：数字越大越放手。接单时取「策略上限」与「发起人设置」里较小的那个。
var (
	saKindRank = map[string]int{KindChat: 0, KindLearn: 1, KindWork: 2, KindCode: 3}
	saModeRank = map[string]int{ModePlan: 0, ModeConfirm: 1, ModeAuto: 2}
)

func saLowerKind(a, b string) string {
	a, b = normalizeKind(a), normalizeKind(b)
	if saKindRank[b] < saKindRank[a] {
		return b
	}
	return a
}

func saLowerMode(a, b string) string {
	a, b = normalizeMode(a), normalizeMode(b)
	if saModeRank[b] < saModeRank[a] {
		return b
	}
	return a
}

// saActorIsOwner 判断发起人是不是本机主人：网页上 ActorID 就是执行用户本人；
// 其它来源（QQ）看策略里登记的主人账号（saIsOwner，B 线提供）。其余一律按访客处理。
func saActorIsOwner(req saAcceptReq) bool {
	actor := strings.TrimSpace(req.Origin.ActorID)
	if actor == "" {
		return isOwnerUser(req.UserID)
	}
	if !isOwnerUser(req.UserID) {
		return false
	}
	return actor == req.UserID || saIsOwner(req.Policy, req.Origin.Source, actor)
}

// saTicketAuthority 计算这份工单允许的 Kind/Mode 上限。
//   - 本机主人：用自己的设置（会话用途 + agent_mode）；
//   - 其他人：用 sa_policy.guest_kind/guest_mode（默认 chat/confirm），并与来源会话用途取较小者。
func saTicketAuthority(req saAcceptReq, conv model.Conversation) (string, string) {
	ownKind := normalizeKind(conv.Kind)
	if saActorIsOwner(req) {
		return ownKind, agentModeFor(req.UserID)
	}
	guestKind := strings.TrimSpace(req.Policy.GuestKind)
	if guestKind == "" {
		guestKind = KindChat
	}
	guestMode := strings.TrimSpace(req.Policy.GuestMode)
	if guestMode == "" {
		guestMode = ModeConfirm
	}
	return saLowerKind(ownKind, guestKind), normalizeMode(guestMode)
}

// ---------------------------------------------------------------------------
// 扇出：后续消息交给已有工单
// ---------------------------------------------------------------------------

type saFanOutCandidate struct {
	taskID uint
	note   string
}

var saTaskRefPattern = regexp.MustCompile(`#(\d{1,10})`)

// saFanOutImpl 只按**显式引用**认领（契约 §3.3），不猜话题。
func saFanOutImpl(ctx context.Context, trig model.SATrigger) ([]uint, error) {
	if model.DB == nil || trig.Id == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cands := saFanOutCandidates(ctx, trig)
	if len(cands) == 0 {
		return nil, nil
	}
	claimed := make([]uint, 0, len(cands))
	for _, c := range cands {
		ok, err := saClaimTicket(ctx, c.taskID, trig, c.note)
		if err != nil {
			log.Printf("[sa] 工单 %d 认领消息 %d 失败：%v", c.taskID, trig.Id, err)
			continue
		}
		if ok {
			claimed = append(claimed, c.taskID)
		}
	}
	if len(claimed) > 0 {
		// C 线只写 disposition 这一列（契约 §5）；A 线重复写同值也没关系。
		model.DB.Model(&model.SATrigger{}).Where("id = ?", trig.Id).
			Update("disposition", model.SADispFannedOut)
	}
	return claimed, nil
}

// saFanOutCandidates 按优先级找认领者：回复工单消息 > 文本点名 #id > 唯一在等人的工单。
func saFanOutCandidates(ctx context.Context, trig model.SATrigger) []saFanOutCandidate {
	// 1. 回复了某工单发出的消息。
	if ref := strings.TrimSpace(trig.ReplyToMsgID); ref != "" {
		var outs []model.SAOutbound
		model.DB.WithContext(ctx).Where("sent_ref = ? AND task_id <> 0", ref).Order("id desc").Limit(5).Find(&outs)
		var out []saFanOutCandidate
		seen := map[uint]bool{}
		for _, o := range outs {
			if seen[o.TaskID] || !saTaskSubscribesThread(o.TaskID, trig.ThreadID) {
				continue
			}
			seen[o.TaskID] = true
			out = append(out, saFanOutCandidate{taskID: o.TaskID, note: "回复了工单发出的消息"})
		}
		if len(out) > 0 {
			return out
		}
	}

	// 2. 文本里写了 #<任务id>，且该任务订阅了这个 Thread。
	var byRef []saFanOutCandidate
	for _, id := range saTaskRefs(trig.Text) {
		if !saTaskSubscribesThread(id, trig.ThreadID) {
			continue
		}
		if _, err := loadTask(id); err != nil {
			continue
		}
		byRef = append(byRef, saFanOutCandidate{taskID: id, note: fmt.Sprintf("消息里点名了任务 #%d", id)})
		if len(byRef) >= 3 {
			break
		}
	}
	if len(byRef) > 0 {
		return byRef
	}

	// 3. 唯一一个在等人工回答、且发起人就是这个人的工单。
	var tasks []model.AgentTask
	model.DB.WithContext(ctx).
		Model(&model.AgentTask{}).
		Joins("JOIN sa_subscription s ON s.task_id = agent_task.id AND s.thread_id = ? AND s.dropped_at IS NULL", trig.ThreadID).
		Where("agent_task.status = ?", model.TaskWaitingHuman).
		Order("agent_task.id desc").Limit(2).Find(&tasks)
	if len(tasks) != 1 {
		return nil
	}
	m, ok := saTaskMandate(tasks[0].Id)
	if !ok || strings.TrimSpace(m.ActorID) == "" || m.ActorID != trig.ActorID || m.ThreadID != trig.ThreadID {
		return nil
	}
	return []saFanOutCandidate{{taskID: tasks[0].Id, note: "回答工单提出的问题"}}
}

// saTaskRefs 从文本里取 #id（去重，保持出现顺序）。
func saTaskRefs(text string) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, m := range saTaskRefPattern.FindAllStringSubmatch(text, 5) {
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil || n == 0 || seen[uint(n)] {
			continue
		}
		seen[uint(n)] = true
		out = append(out, uint(n))
	}
	return out
}

// saClaimTicket 把一条消息接到已有工单上：授权失效的不接；已结束的先复核再续跑；
// 被叫停/暂停的不接（消息回到前台判断）。返回是否真的认领。
func saClaimTicket(ctx context.Context, taskID uint, trig model.SATrigger, note string) (bool, error) {
	task, err := loadTask(taskID)
	if err != nil {
		return false, nil
	}
	m, ok := saTaskMandate(taskID)
	if !ok {
		return false, nil
	}
	if m.WithdrawnAt != nil || m.RevokedAt != nil {
		return false, nil
	}
	if task.Status == model.TaskCanceled || task.Status == model.TaskPaused {
		return false, nil
	}

	if model.TaskTerminal(task.Status) {
		// 结束的工单先按 §3.7 复核授权，通过才续跑。
		if ok, why := saReauthorizeMandate(m); !ok {
			saRevokeMandate(m, why)
			return false, nil
		}
		if !saResumeEndedTicket(&task) {
			return false, nil
		}
		recordTaskEvent(task.Id, "resumed", "收到后续消息，复核授权通过后继续执行", map[string]any{"trigger_id": trig.Id})
	}

	// 先落 outbox 再恢复运行：进程在两步之间崩掉也不会丢消息。
	if err := enqueueTaskMessage(task.Id, "follow_up", saFollowUpText(task, trig, note),
		fmt.Sprintf("t%d:follow:t%d", task.Id, trig.Id)); err != nil {
		return false, err
	}
	if task.Status == model.TaskWaitingHuman || task.Status == model.TaskReview {
		if ok, err := transitionTask(task.Id, []string{model.TaskWaitingHuman, model.TaskReview}, map[string]any{
			"status": model.TaskRunning, "stop_reason": "human_input", "blocker": "", "next_run_at": time.Now(),
		}); err != nil {
			return false, err
		} else if ok {
			kickTaskScheduler()
		}
	}
	recordTaskEvent(task.Id, "follow_up", "收到后续消息："+clipRunes(trig.Text, 300),
		map[string]any{"trigger_id": trig.Id, "actor_id": trig.ActorID, "note": note})
	return true, nil
}

func saFollowUpText(task model.AgentTask, trig model.SATrigger, note string) string {
	var b strings.Builder
	b.WriteString("【来自原对话的后续消息】")
	if strings.TrimSpace(trig.ActorName) != "" {
		b.WriteString(trig.ActorName + "：")
	}
	b.WriteString("\n" + strings.TrimSpace(trig.Text) + "\n")
	if strings.TrimSpace(note) != "" {
		b.WriteString("\n（前台备注：" + note + "）\n")
	}
	b.WriteString(fmt.Sprintf("\n这条消息已认领给任务 #%d（消息 #%d）。按原目标与约束处理它；"+
		"如果是回答你之前提出的问题，就继续执行未完成的部分。"+
		"如果它明显超出本任务范围（换成了另一件事），调用 orchestrator_handback 交回前台判断，不要硬做。\n",
		task.Id, trig.Id))
	return b.String()
}

// ---------------------------------------------------------------------------
// 授权复核与收回
// ---------------------------------------------------------------------------

// saReauthorizeMandate 复核 §3.7：发起人当前强度不低于 Mandate 里的强度；
// 推断接单还要求模式仍是 auto 且命中的规则仍启用。
func saReauthorizeMandate(m model.SAMandate) (bool, string) {
	trig, err := loadSATrigger(m.OriginTriggerID)
	if err != nil {
		return false, "找不到原始消息，无法复核授权"
	}
	policy, err := saPolicyFor(m.UserID)
	if err != nil {
		return false, "读取接单策略失败：" + err.Error()
	}
	strength, reason := saGate(policy, trig, true)
	if saStrengthRank(strength) < saStrengthRank(m.Strength) {
		return false, fmt.Sprintf("发起人当前强度 %s 低于工单授权 %s（%s）", strength, m.Strength, reason)
	}
	if m.Strength == model.SAStrengthInferred {
		if strings.TrimSpace(policy.AcceptMode) != model.SAAcceptAuto {
			return false, "接单模式已不是 auto"
		}
		if !saRulesStillEnabled(policy.Rules, m.MatchedRules) {
			return false, "命中的接单规则已停用"
		}
	}
	return true, reason
}

func saStrengthRank(s string) int {
	switch strings.TrimSpace(s) {
	case model.SAStrengthExplicit:
		return 2
	case model.SAStrengthInferred:
		return 1
	}
	return 0
}

type saTicketRule struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// saJSONStrings 把 JSON 字符串数组解出来；坏数据按空数组处理。
func saJSONStrings(raw string) []string {
	var out []string
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &out) != nil {
		return []string{}
	}
	return out
}

// saRulesStillEnabled 检查推断接单时命中的规则仍然存在且启用。
func saRulesStillEnabled(rulesJSON, matchedJSON string) bool {
	var want []string
	if json.Unmarshal([]byte(matchedJSON), &want) != nil || len(want) == 0 {
		return false
	}
	var rules []saTicketRule
	if json.Unmarshal([]byte(rulesJSON), &rules) != nil {
		return false
	}
	enabled := map[string]bool{}
	for _, r := range rules {
		enabled[r.ID] = r.Enabled
	}
	for _, id := range want {
		if !enabled[id] {
			return false
		}
	}
	return true
}

// saRevokeMandate 记一次授权复核失败：只写终结字段（契约 §5）。
func saRevokeMandate(m model.SAMandate, why string) {
	now := time.Now()
	res := model.DB.Model(&model.SAMandate{}).Where("id = ? AND revoked_at IS NULL", m.Id).
		Updates(map[string]any{"revoked_at": now, "end_reason": clipRunes(why, 255)})
	if res.Error != nil || res.RowsAffected == 0 {
		return
	}
	recordTaskEvent(m.TaskID, "mandate_revoked", "授权复核失败："+why, nil)
}

// saResumeEndedTicket 把已结束（done/failed/budget_exhausted）的工单续上。
func saResumeEndedTicket(task *model.AgentTask) bool {
	ok, err := transitionTask(task.Id, []string{model.TaskDone, model.TaskFailed, model.TaskBudgetExhausted}, map[string]any{
		"status": model.TaskRunning, "stop_reason": "resumed_by_message", "finished_at": nil,
		"next_run_at": time.Now(), "blocker": "", "consecutive_failures": 0,
	})
	if err != nil || !ok {
		return false
	}
	updated, err := loadTask(task.Id)
	if err != nil {
		return false
	}
	*task = updated
	kickTaskScheduler()
	return true
}

// ---------------------------------------------------------------------------
// 交回、叫停、查询
// ---------------------------------------------------------------------------

// 执行端的 orchestrator_handback 最终走 B 线的 saHandBack；这里只做「这条消息确实属于本任务」的检查。

func saAttachTicketImpl(ctx context.Context, taskID uint, trig model.SATrigger, note string) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	task, err := loadTask(taskID)
	if err != nil {
		return errors.New("任务不存在")
	}
	if task.Status == model.TaskCanceled {
		return errors.New("工单已停止，不能再接收消息")
	}
	// 已结束的工单走与扇出相同的复核；结束后被叫停/吊销的不接。
	ok, err := saClaimTicket(ctx, taskID, trig, note)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("工单授权已失效或当前状态不能接收消息")
	}
	// 订阅：已有（origin/attached）就不动角色，没有才补一条 attached（契约 §4）。
	if !saTaskSubscribesThread(taskID, trig.ThreadID) {
		sub := model.SASubscription{TaskID: taskID, ThreadID: trig.ThreadID, Role: model.SASubAttached}
		if err := model.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&sub).Error; err != nil {
			return err
		}
	}
	if err := model.DB.WithContext(ctx).Model(&model.SATrigger{}).Where("id = ?", trig.Id).
		Update("disposition", model.SADispFannedOut).Error; err != nil {
		return err
	}
	recordTaskEvent(taskID, "attached", "前台把一条消息交给本任务", map[string]any{"trigger_id": trig.Id, "note": note})
	return nil
}

// saWithdrawMandate 收回工单授权（幂等）。没有 Mandate 的普通长任务不受影响。
func saWithdrawMandate(taskID uint, reason string) error {
	m, ok := saTaskMandate(taskID)
	if !ok || m.WithdrawnAt != nil {
		return nil
	}
	return model.DB.Model(&model.SAMandate{}).Where("id = ? AND withdrawn_at IS NULL", m.Id).
		Updates(map[string]any{"withdrawn_at": time.Now(), "end_reason": clipRunes(reason, 255)}).Error
}

// saStopTicketImpl 叫停：取消任务、收回授权。被叫停的工单之后不再认领任何消息。
func saStopTicketImpl(ctx context.Context, taskID uint, by, reason string) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	task, err := loadTask(taskID)
	if err != nil {
		return errors.New("任务不存在")
	}
	if strings.TrimSpace(by) == "" {
		by = "router"
	}
	why := strings.TrimSpace(reason)
	if why == "" {
		why = "发起人在对话里叫停"
	}
	if m, ok := saTaskMandate(taskID); ok && m.WithdrawnAt == nil {
		if err := saWithdrawMandate(taskID, why); err != nil {
			return err
		}
	}
	if !model.TaskTerminal(task.Status) {
		ok, err := transitionTask(taskID, nonTerminalTaskStates, map[string]any{
			"status": model.TaskCanceled, "stop_reason": "stopped_by_" + by, "blocker": clipRunes(why, 1000),
		})
		if err != nil {
			return err
		}
		if ok {
			cancelTaskRun(taskID)
			recordTaskEvent(taskID, "stopped", "工单被叫停："+why, map[string]any{"by": by})
		}
	}
	return nil
}

// saLiveTicketsImpl 给前台判断用的工单卡：本 Thread 订阅着、授权仍有效、
// 没被取消（含最近结束、可被续上的）。
func saLiveTicketsImpl(threadID uint) ([]saTicketCard, error) {
	if model.DB == nil || threadID == 0 {
		return nil, nil
	}
	var subs []model.SASubscription
	if err := model.DB.Where("thread_id = ? AND dropped_at IS NULL", threadID).
		Order("id desc").Limit(50).Find(&subs).Error; err != nil {
		return nil, err
	}
	cards := make([]saTicketCard, 0, len(subs))
	for _, s := range subs {
		task, err := loadTask(s.TaskID)
		if err != nil || task.Status == model.TaskCanceled {
			continue
		}
		if m, ok := saTaskMandate(s.TaskID); ok && (m.WithdrawnAt != nil || m.RevokedAt != nil) {
			continue
		}
		cards = append(cards, saTicketCard{
			TaskID: task.Id, Title: task.Title, Status: task.Status,
			Stage: saTicketStage(task.Status), Role: s.Role,
		})
		if len(cards) >= 20 {
			break
		}
	}
	return cards, nil
}

// saTicketStage 把任务状态映射成前台卡片上的阶段（契约 §3.5）。
func saTicketStage(status string) string {
	switch status {
	case model.TaskWaitingHuman:
		return "person/answer"
	case model.TaskReview:
		return "person/review"
	case model.TaskFailed, model.TaskBudgetExhausted:
		return "person/failed"
	case model.TaskPaused:
		return "paused"
	case model.TaskRunning, model.TaskVerifying:
		return "working"
	case model.TaskDone, model.TaskCanceled:
		return "ended"
	}
	return "ended"
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func saMandateByOrigin(originTriggerID uint) (model.SAMandate, bool) {
	var m model.SAMandate
	if model.DB == nil || originTriggerID == 0 {
		return m, false
	}
	if err := model.DB.Where("origin_trigger_id = ?", originTriggerID).First(&m).Error; err != nil {
		return m, false
	}
	return m, true
}

func saTaskMandate(taskID uint) (model.SAMandate, bool) {
	var m model.SAMandate
	if model.DB == nil || taskID == 0 {
		return m, false
	}
	if err := model.DB.Where("task_id = ?", taskID).First(&m).Error; err != nil {
		return m, false
	}
	return m, true
}

func saTaskSubscribesThread(taskID, threadID uint) bool {
	if model.DB == nil || taskID == 0 || threadID == 0 {
		return false
	}
	var n int64
	model.DB.Model(&model.SASubscription{}).
		Where("task_id = ? AND thread_id = ? AND dropped_at IS NULL", taskID, threadID).Count(&n)
	return n > 0
}

func loadSATrigger(id uint) (model.SATrigger, error) {
	var t model.SATrigger
	if model.DB == nil {
		return t, errors.New("数据库未初始化")
	}
	err := model.DB.First(&t, id).Error
	return t, err
}

// taskOriginThread 返回工单的原对话，以及这份授权现在还活着没有。
// 没有 Mandate 的任务（界面/主对话直接建的长任务）返回 ok=false，通知仍走主对话。
func taskOriginThread(taskID uint) (threadID uint, active, hasMandate bool) {
	m, ok := saTaskMandate(taskID)
	if !ok {
		return 0, false, false
	}
	return m.ThreadID, m.WithdrawnAt == nil && m.RevokedAt == nil, true
}

func isSAUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unique constraint") || strings.Contains(s, "duplicate entry") ||
		strings.Contains(s, "unique failed") || strings.Contains(s, "constraint failed")
}

func taskSourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case model.SASourceWeb:
		return "网页"
	case model.SASourceQQ:
		return "QQ"
	case model.SASourceWakeup:
		return "定时唤醒"
	case model.SASourceProactive:
		return "主动消息"
	}
	if strings.TrimSpace(source) == "" {
		return "未知"
	}
	return source
}
