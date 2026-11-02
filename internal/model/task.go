package model

import "time"

// 长任务运行时（docs/agent/16-task-runtime.md）。
//
// 一个任务 = 一行 AgentTask：保存用户原始目标、约束、验收条件、进度、阻碍与执行预算，
// 并关联「主对话」（发起任务、接收最终报告）与「执行对话」（执行器真正干活的会话）。
// 所有可恢复的状态都在库里：进程重启后，调度器只靠这几张表就能知道还剩什么工作。
//
// 状态机（宿主决定，不由模型的「完成了」决定）：
//
//	running ──轮次结束且未提交──────────────▶ running（自动续跑）
//	running ──task_submit──▶ verifying ──全部通过──▶ done
//	                              ├──有失败项──▶ running（失败项交回执行对话修正）
//	                              └──需人工判断──▶ review（待审）
//	running ──task_block / ask_user──▶ waiting_human ──用户补充──▶ running
//	任何非终态 ──暂停──▶ paused ──继续──▶ running
//	任何非终态 ──取消──▶ canceled（终态）
//	预算耗尽 ▶ budget_exhausted（终态，可「追加预算并重试」）；重复失败 ▶ failed（终态，可重试）
const (
	TaskRunning         = "running"
	TaskVerifying       = "verifying"
	TaskReview          = "review"
	TaskWaitingHuman    = "waiting_human"
	TaskPaused          = "paused"
	TaskDone            = "done"
	TaskFailed          = "failed"
	TaskCanceled        = "canceled"
	TaskBudgetExhausted = "budget_exhausted"
)

// TaskTerminal 判断任务是否已经停在终态（不会再被调度）。
func TaskTerminal(status string) bool {
	switch status {
	case TaskDone, TaskFailed, TaskCanceled, TaskBudgetExhausted:
		return true
	}
	return false
}

type AgentTask struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;size:36;index;not null"`
	// MainConversationID 是发起任务的主对话：最终报告、待审提醒都落在这里。
	MainConversationID uint `gorm:"column:main_conversation_id;index"`
	// ExecConversationID 是执行对话：一个任务独占一个执行对话（唯一），
	// 所以「同一执行对话同一时刻只跑一轮」可以落在任务行的租约上。
	ExecConversationID uint   `gorm:"column:exec_conversation_id;uniqueIndex"`
	Title              string `gorm:"column:title;size:200"`
	// Goal / Constraints 是用户原文，创建后不再改写：压缩与续跑每轮都原样注入。
	Goal        string `gorm:"column:goal;type:longtext"`
	Constraints string `gorm:"column:constraints;type:longtext"`
	// Criteria 是验收条件 JSON 数组（见 web/task_store.go 的 taskCriterion）。
	Criteria string `gorm:"column:criteria;type:longtext"`
	// Progress 是执行器最近一次汇报的进度；DoneSteps 是已完成步骤（JSON 数组，只增不减）。
	Progress  string `gorm:"column:progress;type:longtext"`
	DoneSteps string `gorm:"column:done_steps;type:longtext"`
	// Blocker 是当前阻碍（等待人工、失败原因等）的人话描述。
	Blocker string `gorm:"column:blocker;type:text"`
	Status  string `gorm:"column:status;size:24;index"`
	// StopReason 用短码说明为什么停：submitted / need_human / verify_failed / budget_rounds / ...
	StopReason string `gorm:"column:stop_reason;size:40"`
	// Verification 是最近一次验收的逐项结果（JSON），Evidence 是执行器最近一次提交的结果与证据（JSON）。
	Verification string `gorm:"column:verification;type:longtext"`
	Evidence     string `gorm:"column:evidence;type:longtext"`

	Model string `gorm:"column:model;size:128"`
	Kind  string `gorm:"column:kind;size:16"`
	Mode  string `gorm:"column:mode;size:16"`
	// Roots 是创建时的工作区快照（JSON 数组）：续跑不随用户后来改设置漂移。
	Roots string `gorm:"column:roots;type:text"`

	// 执行预算：轮数 / 工具调用 / token / 截止时间。0 表示该维度不限（轮数总有默认上限）。
	MaxRounds     int        `gorm:"column:max_rounds"`
	MaxToolCalls  int        `gorm:"column:max_tool_calls"`
	MaxTokens     int        `gorm:"column:max_tokens"`
	Deadline      *time.Time `gorm:"column:deadline"`
	RoundsUsed    int        `gorm:"column:rounds_used"`
	ToolCallsUsed int        `gorm:"column:tool_calls_used"`
	TokensUsed    int        `gorm:"column:tokens_used"`
	// VerifyAttempts 统计「提交后被驳回」的次数；超过上限按失败处理，防止无限修。
	VerifyAttempts int `gorm:"column:verify_attempts"`
	// ConsecutiveFailures 统计连续的执行错误（模型/网络）；成功跑完一轮清零。
	ConsecutiveFailures int    `gorm:"column:consecutive_failures"`
	LastError           string `gorm:"column:last_error;type:text"`

	// NextRunAt：调度器只拣 running 且 NextRunAt 已到的任务。退避重试就是把它往后推。
	NextRunAt *time.Time `gorm:"column:next_run_at;index"`
	// LeaseOwner / LeaseUntil 是轮次租约：拿租约是一条带条件的 UPDATE，
	// 两个调度方不可能同时拿到，同一执行对话也就不会同时跑两轮。
	LeaseOwner string     `gorm:"column:lease_owner;size:64"`
	LeaseUntil *time.Time `gorm:"column:lease_until"`
	// NeedsReconcile=true 表示上一轮被重启打断：下一轮先核对现场，再继续。
	NeedsReconcile bool `gorm:"column:needs_reconcile"`
	// Revision 每次状态变更 +1，界面按钮操作带它做乐观并发控制。
	Revision int `gorm:"column:revision;not null;default:0"`

	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

func (AgentTask) TableName() string { return "agent_task" }

// 轮次状态。
const (
	TaskRoundRunning     = "running"
	TaskRoundFinished    = "finished"
	TaskRoundInterrupted = "interrupted"
	TaskRoundError       = "error"
)

// AgentTaskRound 记录执行对话里的每一轮：开始即落库，所以重启时能发现「跑到一半」的轮次。
// Journal 是本轮工具调用流水（JSON 数组），每个调用开始与结束各写一次，
// 重启后据此区分「已完成」「结果未知」的外部操作。
type AgentTaskRound struct {
	Id         uint       `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID     uint       `gorm:"column:task_id;index;not null"`
	No         int        `gorm:"column:no"`
	Trigger    string     `gorm:"column:trigger;size:32"`
	Status     string     `gorm:"column:status;size:16;index"`
	Outcome    string     `gorm:"column:outcome;size:40"`
	Reply      string     `gorm:"column:reply;type:longtext"`
	Journal    string     `gorm:"column:journal;type:longtext"`
	ToolCalls  int        `gorm:"column:tool_calls"`
	Tokens     int        `gorm:"column:tokens"`
	Compacted  int        `gorm:"column:compacted"`
	ErrText    string     `gorm:"column:err_text;type:text"`
	StartedAt  time.Time  `gorm:"column:started_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

func (AgentTaskRound) TableName() string { return "agent_task_round" }

// AgentTaskOutbox 是待投递给执行对话的消息。先落 outbox、再在同一事务里
// 「写消息 + 标记已投递」，进程在两步之间崩掉也不会丢消息或重复投递。
type AgentTaskOutbox struct {
	Id          uint       `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID      uint       `gorm:"column:task_id;index;not null"`
	Kind        string     `gorm:"column:kind;size:24"`
	Content     string     `gorm:"column:content;type:longtext"`
	DedupeKey   string     `gorm:"column:dedupe_key;size:96;uniqueIndex"`
	DeliveredAt *time.Time `gorm:"column:delivered_at;index"`
	MessageID   uint       `gorm:"column:message_id"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

func (AgentTaskOutbox) TableName() string { return "agent_task_outbox" }

// AgentTaskEvent 是任务的审计流水：状态变化、验收结果、失败原因都在这里可查。
type AgentTaskEvent struct {
	Id        uint      `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    uint      `gorm:"column:task_id;index;not null"`
	Kind      string    `gorm:"column:kind;size:32"`
	Message   string    `gorm:"column:message;type:text"`
	Data      string    `gorm:"column:data;type:longtext"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (AgentTaskEvent) TableName() string { return "agent_task_event" }

// TaskModels 是需要迁移的全部任务表（MySQL 与 SQLite 两条迁移路径共用）。
func TaskModels() []any {
	return []any{&AgentTask{}, &AgentTaskRound{}, &AgentTaskOutbox{}, &AgentTaskEvent{}}
}
