package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 本文件是长任务运行时的存储层（docs/agent/16-task-runtime.md）：
// 建任务、状态迁移、轮次租约、待投递消息（outbox）与审计事件。
// 调度与执行在 task_runner.go，验收在 task_verify.go。

// 验收方式：auto 由宿主用独立模型逐项核对证据；command 由宿主在工作区运行检查命令；
// manual 只能由人判断（进入待审）。
const (
	criterionAuto    = "auto"
	criterionCommand = "command"
	criterionManual  = "manual"
)

type taskCheck struct {
	Program string   `json:"program"`
	Args    []string `json:"args,omitempty"`
	CWD     string   `json:"cwd,omitempty"`
}

type taskCriterion struct {
	ID     string     `json:"id"`
	Text   string     `json:"text"`
	Method string     `json:"method"`
	Check  *taskCheck `json:"check,omitempty"`
}

type taskEvidenceItem struct {
	Criterion string `json:"criterion"`
	Evidence  string `json:"evidence"`
}

// taskSubmission 是执行器通过 task_submit 提交的结果与证据。
type taskSubmission struct {
	Summary string             `json:"summary"`
	Items   []taskEvidenceItem `json:"items"`
	Round   int                `json:"round"`
	At      time.Time          `json:"at"`
}

// 单项验收结论。
const (
	verdictPass   = "pass"
	verdictFail   = "fail"
	verdictReview = "review"
)

type taskVerdict struct {
	Criterion string `json:"criterion"`
	Text      string `json:"text"`
	Method    string `json:"method"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	By        string `json:"by,omitempty"` // host / judge / command / user
}

type taskVerification struct {
	Attempt int           `json:"attempt"`
	Round   int           `json:"round"`
	Overall string        `json:"overall"` // pass / fail / review
	Items   []taskVerdict `json:"items"`
	At      time.Time     `json:"at"`
}

// taskSpec 是建任务的输入：来自界面表单或主对话的 task_manage 工具。
type taskSpec struct {
	Title           string          `json:"title"`
	Goal            string          `json:"goal"`
	Constraints     string          `json:"constraints"`
	Criteria        []taskCriterion `json:"criteria"`
	MaxRounds       int             `json:"max_rounds"`
	MaxToolCalls    int             `json:"max_tool_calls"`
	MaxTokens       int             `json:"max_tokens"`
	DeadlineMinutes int             `json:"deadline_minutes"`
}

// taskCreateOptions 是建任务时由宿主决定（而不是由请求方随意填写）的上下文。
type taskCreateOptions struct {
	UserID             string
	MainConversationID uint
	Model              string
	Kind               string
	Mode               string
	Roots              []string
	// AllowCommandChecks：command 验收会由宿主直接运行命令。界面上由用户本人填写时允许；
	// 模型代填时只在自主档允许，否则等于绕过了「风险操作先确认」。
	AllowCommandChecks bool
}

const (
	taskDefaultMaxRounds      = 20
	taskMaxCriteria           = 20
	taskMaxVerifyAttemptsDflt = 3
)

var errTaskConflict = errors.New("任务状态已变化，请刷新后重试")

func normalizeTaskSpec(spec taskSpec, allowCommand bool) (taskSpec, error) {
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Goal = strings.TrimSpace(spec.Goal)
	spec.Constraints = strings.TrimSpace(spec.Constraints)
	if spec.Goal == "" {
		return spec, errors.New("缺少任务目标")
	}
	if len([]rune(spec.Goal)) > 16000 || len([]rune(spec.Constraints)) > 8000 {
		return spec, errors.New("目标或约束过长（目标 ≤16000 字，约束 ≤8000 字）")
	}
	if spec.Title == "" {
		spec.Title = clipRunes(strings.SplitN(spec.Goal, "\n", 2)[0], 40)
	}
	spec.Title = clipRunes(spec.Title, 120)
	if len(spec.Criteria) == 0 {
		return spec, errors.New("至少需要一条明确的验收条件：没有验收条件就无法判断任务是否完成")
	}
	if len(spec.Criteria) > taskMaxCriteria {
		return spec, fmt.Errorf("验收条件最多 %d 条", taskMaxCriteria)
	}
	out := make([]taskCriterion, 0, len(spec.Criteria))
	for i, c := range spec.Criteria {
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			return spec, fmt.Errorf("第 %d 条验收条件为空", i+1)
		}
		c.ID = "c" + strconv.Itoa(i+1)
		switch strings.ToLower(strings.TrimSpace(c.Method)) {
		case "", criterionAuto:
			c.Method, c.Check = criterionAuto, nil
		case criterionManual:
			c.Method, c.Check = criterionManual, nil
		case criterionCommand:
			c.Method = criterionCommand
			if c.Check == nil || strings.TrimSpace(c.Check.Program) == "" {
				return spec, fmt.Errorf("第 %d 条验收条件是命令检查，但没有给出 program", i+1)
			}
			if !allowCommand {
				return spec, errors.New("当前档位不允许由模型设置命令验收；请在界面中创建，或改用 auto/manual")
			}
			c.Check.Program = strings.TrimSpace(c.Check.Program)
		default:
			return spec, fmt.Errorf("第 %d 条验收条件的 method 只能是 auto / command / manual", i+1)
		}
		out = append(out, c)
	}
	spec.Criteria = out
	if spec.MaxRounds <= 0 {
		spec.MaxRounds = agentIntEnvOr("AGENT_TASK_MAX_ROUNDS", taskDefaultMaxRounds)
	}
	if spec.MaxRounds > 500 {
		spec.MaxRounds = 500
	}
	if spec.MaxToolCalls < 0 || spec.MaxTokens < 0 || spec.DeadlineMinutes < 0 {
		return spec, errors.New("预算不能为负数")
	}
	return spec, nil
}

func agentIntEnvOr(name string, fallback int) int {
	if n := agentIntEnv(name); n > 0 {
		return n
	}
	return fallback
}

// createTask 建执行对话 + 任务行 + 首条任务说明（outbox），然后提醒调度器。
// 工单线要在同一个事务里追加 Mandate 与订阅，所以真正的建任务逻辑在 createTaskTx。
func createTask(spec taskSpec, opt taskCreateOptions) (model.AgentTask, error) {
	if model.DB == nil {
		return model.AgentTask{}, errors.New("数据库未初始化")
	}
	var task model.AgentTask
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		t, err := createTaskTx(tx, spec, opt)
		if err != nil {
			return err
		}
		task = t
		return nil
	})
	if err != nil {
		return model.AgentTask{}, err
	}
	recordTaskEvent(task.Id, "created", "任务已创建，执行对话 #"+strconv.FormatUint(uint64(task.ExecConversationID), 10), nil)
	kickTaskScheduler()
	return task, nil
}

// createTaskTx 在调用方的事务里建执行对话、任务行与首条任务说明。
// 不写事件、不唤醒调度器：调用方提交后再做，避免事务回滚却留下通知。
func createTaskTx(tx *gorm.DB, spec taskSpec, opt taskCreateOptions) (model.AgentTask, error) {
	if strings.TrimSpace(opt.UserID) == "" {
		return model.AgentTask{}, errors.New("缺少用户")
	}
	spec, err := normalizeTaskSpec(spec, opt.AllowCommandChecks)
	if err != nil {
		return model.AgentTask{}, err
	}
	if opt.MainConversationID != 0 {
		var main model.Conversation
		if err := tx.Where("id = ? AND user_id = ?", opt.MainConversationID, opt.UserID).First(&main).Error; err != nil {
			return model.AgentTask{}, errors.New("主对话不存在")
		}
		if strings.TrimSpace(opt.Model) == "" {
			opt.Model = main.Model
		}
		if strings.TrimSpace(opt.Kind) == "" {
			opt.Kind = main.Kind
		}
	}
	if strings.TrimSpace(opt.Model) == "" {
		return model.AgentTask{}, errors.New("缺少执行模型")
	}
	opt.Kind = normalizeKind(opt.Kind)
	if opt.Kind == KindChat {
		opt.Kind = KindWork // 闲聊模式没有执行工具，任务至少按辅助工作执行
	}
	if strings.TrimSpace(opt.Mode) == "" {
		opt.Mode = agentModeFor(opt.UserID)
	}
	criteria, _ := json.Marshal(spec.Criteria)
	roots, _ := json.Marshal(opt.Roots)
	now := time.Now()
	task := model.AgentTask{
		UserID: opt.UserID, MainConversationID: opt.MainConversationID,
		Title: spec.Title, Goal: spec.Goal, Constraints: spec.Constraints, Criteria: string(criteria),
		DoneSteps: "[]", Status: model.TaskRunning, StopReason: "created",
		Model: opt.Model, Kind: opt.Kind, Mode: normalizeMode(opt.Mode), Roots: string(roots),
		MaxRounds: spec.MaxRounds, MaxToolCalls: spec.MaxToolCalls, MaxTokens: spec.MaxTokens,
		NextRunAt: &now,
	}
	if spec.DeadlineMinutes > 0 {
		d := now.Add(time.Duration(spec.DeadlineMinutes) * time.Minute)
		task.Deadline = &d
	}
	exec := model.Conversation{UserID: opt.UserID, Title: "任务：" + spec.Title, Model: opt.Model, Kind: opt.Kind}
	if err := tx.Create(&exec).Error; err != nil {
		return model.AgentTask{}, fmt.Errorf("创建执行对话失败：%w", err)
	}
	task.ExecConversationID = exec.Id
	if err := tx.Create(&task).Error; err != nil {
		return model.AgentTask{}, fmt.Errorf("创建任务失败：%w", err)
	}
	if err := enqueueTaskMessageTx(tx, task.Id, "goal", taskBriefMessage(task, spec.Criteria), fmt.Sprintf("t%d:goal", task.Id)); err != nil {
		return model.AgentTask{}, err
	}
	return task, nil
}

// taskBriefMessage 是投递给执行对话的第一条消息：目标、约束、验收条件与工作方式。
func taskBriefMessage(t model.AgentTask, criteria []taskCriterion) string {
	var b strings.Builder
	b.WriteString("【任务】" + t.Title + "\n\n## 目标\n" + t.Goal + "\n")
	if strings.TrimSpace(t.Constraints) != "" {
		b.WriteString("\n## 约束\n" + t.Constraints + "\n")
	}
	b.WriteString("\n## 验收条件\n" + renderCriteria(criteria))
	b.WriteString("\n请直接开始执行。每完成一个阶段用 task_update_progress 记录进度；" +
		"全部完成并自查后，用 task_submit 逐条提交证据。缺少登录、权限或必要信息时用 task_block 说明需要人做什么。")
	return b.String()
}

func renderCriteria(criteria []taskCriterion) string {
	var b strings.Builder
	for _, c := range criteria {
		method := map[string]string{criterionAuto: "宿主核对证据", criterionCommand: "宿主运行命令", criterionManual: "人工审核"}[c.Method]
		fmt.Fprintf(&b, "- [%s] %s（%s", c.ID, c.Text, method)
		if c.Check != nil {
			fmt.Fprintf(&b, "：%s %s", c.Check.Program, strings.Join(c.Check.Args, " "))
		}
		b.WriteString("）\n")
	}
	return b.String()
}

func taskCriteria(t model.AgentTask) []taskCriterion {
	var out []taskCriterion
	_ = json.Unmarshal([]byte(t.Criteria), &out)
	return out
}

func taskRoots(t model.AgentTask) []string {
	var out []string
	_ = json.Unmarshal([]byte(t.Roots), &out)
	return out
}

func taskDoneSteps(t model.AgentTask) []string {
	var out []string
	_ = json.Unmarshal([]byte(t.DoneSteps), &out)
	return out
}

func taskLastVerification(t model.AgentTask) *taskVerification {
	if strings.TrimSpace(t.Verification) == "" {
		return nil
	}
	var v taskVerification
	if json.Unmarshal([]byte(t.Verification), &v) != nil {
		return nil
	}
	return &v
}

func taskLastSubmission(t model.AgentTask) *taskSubmission {
	if strings.TrimSpace(t.Evidence) == "" {
		return nil
	}
	var s taskSubmission
	if json.Unmarshal([]byte(t.Evidence), &s) != nil {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// 读取与状态迁移
// ---------------------------------------------------------------------------

func loadTask(id uint) (model.AgentTask, error) {
	var t model.AgentTask
	err := model.DB.First(&t, id).Error
	return t, err
}

func loadUserTask(userID string, id uint) (model.AgentTask, error) {
	var t model.AgentTask
	err := model.DB.Where("id = ? AND user_id = ?", id, userID).First(&t).Error
	return t, err
}

// transitionTask 只在当前状态属于 from 时才写入 updates（带条件的 UPDATE，并发安全），
// 并把 revision +1。返回是否真的迁移了。
func transitionTask(id uint, from []string, updates map[string]any) (bool, error) {
	if updates == nil {
		updates = map[string]any{}
	}
	updates["revision"] = gorm.Expr("revision + 1")
	updates["updated_at"] = time.Now()
	if status, ok := updates["status"].(string); ok && model.TaskTerminal(status) {
		if _, set := updates["finished_at"]; !set {
			updates["finished_at"] = time.Now()
		}
		updates["next_run_at"] = nil
	}
	q := model.DB.Model(&model.AgentTask{}).Where("id = ?", id)
	if len(from) > 0 {
		q = q.Where("status IN ?", from)
	}
	res := q.Updates(updates)
	return res.RowsAffected == 1, res.Error
}

// claimTaskLease 拿轮次租约：只有 running/verifying、已到期、且没有有效租约的任务能被拿到。
// 这是一条带条件的 UPDATE，多个调度方（或同进程里的多个 goroutine）最多一个成功。
func claimTaskLease(id uint, owner string, ttl time.Duration) bool {
	now := time.Now()
	until := now.Add(ttl)
	res := model.DB.Model(&model.AgentTask{}).
		Where("id = ? AND status IN ?", id, []string{model.TaskRunning, model.TaskVerifying}).
		Where("(lease_until IS NULL OR lease_until < ?)", now).
		Where("(next_run_at IS NULL OR next_run_at <= ?)", now).
		Updates(map[string]any{"lease_owner": owner, "lease_until": until})
	return res.Error == nil && res.RowsAffected == 1
}

func renewTaskLease(id uint, owner string, ttl time.Duration) {
	model.DB.Model(&model.AgentTask{}).Where("id = ? AND lease_owner = ?", id, owner).
		Update("lease_until", time.Now().Add(ttl))
}

func releaseTaskLease(id uint, owner string) {
	model.DB.Model(&model.AgentTask{}).Where("id = ? AND lease_owner = ?", id, owner).
		Updates(map[string]any{"lease_owner": "", "lease_until": nil})
}

// ---------------------------------------------------------------------------
// Outbox：先落库再投递，投递与「已投递」标记在同一事务里
// ---------------------------------------------------------------------------

func enqueueTaskMessage(taskID uint, kind, content, dedupe string) error {
	return enqueueTaskMessageTx(model.DB, taskID, kind, content, dedupe)
}

func enqueueTaskMessageTx(tx *gorm.DB, taskID uint, kind, content, dedupe string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	row := model.AgentTaskOutbox{TaskID: taskID, Kind: kind, Content: content, DedupeKey: clipRunes(dedupe, 96)}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// deliverTaskOutbox 把该任务所有未投递的消息按顺序写进执行对话。
// 每条消息在一个事务里「插入消息 + 标记已投递（带 delivered_at IS NULL 条件）」，
// 重启后重放也不会出现重复消息。
func deliverTaskOutbox(t model.AgentTask) (int, error) {
	var rows []model.AgentTaskOutbox
	if err := model.DB.Where("task_id = ? AND delivered_at IS NULL", t.Id).Order("id asc").Find(&rows).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		err := model.DB.Transaction(func(tx *gorm.DB) error {
			msg := model.Message{ConversationID: t.ExecConversationID, Role: "user", Content: row.Content}
			if err := tx.Create(&msg).Error; err != nil {
				return err
			}
			now := time.Now()
			res := tx.Model(&model.AgentTaskOutbox{}).Where("id = ? AND delivered_at IS NULL", row.Id).
				Updates(map[string]any{"delivered_at": now, "message_id": msg.Id})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errTaskConflict // 已被别人投递：回滚本次插入
			}
			return nil
		})
		if errors.Is(err, errTaskConflict) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		touchConversation(t.ExecConversationID)
	}
	return n, nil
}

func pendingTaskOutbox(taskID uint) int64 {
	var n int64
	model.DB.Model(&model.AgentTaskOutbox{}).Where("task_id = ? AND delivered_at IS NULL", taskID).Count(&n)
	return n
}

// ---------------------------------------------------------------------------
// 审计事件与主对话通知
// ---------------------------------------------------------------------------

func recordTaskEvent(taskID uint, kind, message string, data any) {
	if model.DB == nil {
		return
	}
	raw := ""
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			raw = string(b)
		}
	}
	if err := model.DB.Create(&model.AgentTaskEvent{TaskID: taskID, Kind: kind, Message: clipRunes(message, 2000), Data: raw}).Error; err != nil {
		log.Printf("[task] 记录事件失败 task=%d：%v", taskID, err)
	}
}

// notifyTaskMain 把任务的阶段性结局告诉用户。有 Mandate 的（工单）经发件箱发回原对话；
// 其余（界面/主对话建的长任务）仍写主对话的 role=subagent 消息，保持原行为。
// 只写不唤醒：任务的推进由任务调度器负责，主对话不必为每个状态变化跑一轮模型。
func notifyTaskMain(t model.AgentTask, text string) {
	notifyTask(t, saPurposeForStatus(t.Status), text)
}

// saPurposeForStatus 给通知挑一个发件箱用途。调用点知道更准确的语义时可以显式传。
func saPurposeForStatus(status string) string {
	switch status {
	case model.TaskDone:
		return model.SAPurposeResult
	case model.TaskWaitingHuman, model.TaskReview, model.TaskFailed, model.TaskBudgetExhausted:
		return model.SAPurposeQuestion
	}
	return model.SAPurposeNotice
}

// notifyTask 是通知的唯一出口。工单通知带 dedupe=task:<id>:<状态>:<revision>：
// 同一次状态变化重复通知只发一次，重试后又失败（revision 变了）才再发。
func notifyTask(t model.AgentTask, purpose, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if threadID, active, hasMandate := taskOriginThread(t.Id); hasMandate {
		if !active {
			log.Printf("[sa] 工单任务 %d 的授权已失效，丢弃通知：%s", t.Id, clipRunes(text, 120))
			return
		}
		cur, err := loadTask(t.Id)
		if err != nil {
			cur = t
		}
		replyTo := uint(0)
		if m, ok := saTaskMandate(t.Id); ok {
			replyTo = m.OriginTriggerID
		}
		_, err = saEnqueueOutbound(saOutboundReq{
			UserID: t.UserID, ThreadID: threadID, TaskID: t.Id,
			Purpose: purpose, Text: fmt.Sprintf("【任务 #%d %s】%s", t.Id, t.Title, text),
			ReplyToTriggerID: replyTo,
			DedupeKey:        fmt.Sprintf("task:%d:%s:%d", t.Id, cur.Status, cur.Revision),
		})
		if err != nil {
			log.Printf("[sa] 工单任务 %d 的通知入队失败：%v", t.Id, err)
			recordTaskEvent(t.Id, "outbound_error", "通知入队失败："+err.Error(), nil)
		}
		return
	}
	if t.MainConversationID == 0 {
		return
	}
	saveChatMessage(t.MainConversationID, subagentMessageRole, fmt.Sprintf("【任务 #%d %s】%s", t.Id, t.Title, text))
	touchConversation(t.MainConversationID)
}

// reconcileTasksOnStart 是启动对账：上次进程里跑到一半的轮次一律标为 interrupted，
// 相应任务清掉租约、打上 NeedsReconcile，下一轮先核对现场再继续。
// 不在这里推进任何状态——是否真的完成仍由下一轮执行与验收决定。
func reconcileTasksOnStart() {
	if model.DB == nil {
		return
	}
	var rounds []model.AgentTaskRound
	model.DB.Where("status = ?", model.TaskRoundRunning).Find(&rounds)
	now := time.Now()
	for _, r := range rounds {
		model.DB.Model(&model.AgentTaskRound{}).Where("id = ? AND status = ?", r.Id, model.TaskRoundRunning).
			Updates(map[string]any{"status": model.TaskRoundInterrupted, "err_text": "服务重启，本轮未正常结束", "finished_at": now})
		model.DB.Model(&model.AgentTask{}).Where("id = ?", r.TaskID).
			Where("status IN ?", []string{model.TaskRunning, model.TaskVerifying}).
			Updates(map[string]any{"needs_reconcile": true, "next_run_at": now})
		recordTaskEvent(r.TaskID, "restart", fmt.Sprintf("服务重启打断了第 %d 轮；下一轮先核对现场再继续", r.No), nil)
	}
	res := model.DB.Model(&model.AgentTask{}).Where("lease_owner <> ''").
		Updates(map[string]any{"lease_owner": "", "lease_until": nil})
	if len(rounds) > 0 || res.RowsAffected > 0 {
		log.Printf("[task] 启动对账：%d 个中断轮次，释放 %d 个租约", len(rounds), res.RowsAffected)
	}
}
