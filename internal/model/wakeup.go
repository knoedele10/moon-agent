package model

import "time"

// 定时唤醒：到点让 agent 在该会话里自己跑一轮。
//
// 与 proactive（主动聊天）的区别：proactive 是全局策略决定「现在要不要找用户说话」，
// 唤醒是用户或 agent 明确指定「某个时间点在这个会话里做某件事」，一次性、可取消、可追溯。
//
// 为什么不复用 AgentProactiveState：那张表是每用户一份的全局状态，而唤醒是
// 「每会话多条、带触发时间和正文」的队列，语义不同，硬塞会把两者都搞乱。
type AgentWakeup struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_wakeup_user_status,priority:1;index:idx_wakeup_user_created,priority:1"`
	// ConversationID 是被唤醒的会话；唤醒轮次写回它的历史里。
	ConversationID uint      `gorm:"column:conversation_id;not null;index:idx_wakeup_conv,priority:1"`
	Title          string    `gorm:"column:title;size:255"`
	Prompt         string    `gorm:"column:prompt;type:text;not null"` // 到点后交给 agent 的说明
	RunAt          time.Time `gorm:"column:run_at;not null;index:idx_wakeup_due,priority:1;index:idx_wakeup_user_created,priority:2"`
	// Status：pending / done / failed / canceled。
	Status string `gorm:"column:status;size:16;not null;index:idx_wakeup_user_status,priority:2;index:idx_wakeup_due,priority:2"`
	// Attempts 是已经失败过的次数（成功或取消不影响它）。
	Attempts int    `gorm:"column:attempts;not null;default:0"`
	Error    string `gorm:"column:error;size:512"`
	// NextAttemptAt 一列两用：失败退避时间 + 调度器领取时的租约。
	// 进程中途崩了，租约到期后这条唤醒会被重新捡起，不需要额外的「执行中」状态。
	NextAttemptAt *time.Time `gorm:"column:next_attempt_at"`
	FiredAt       *time.Time `gorm:"column:fired_at"`
	// CreatedBy：user（用户在界面上定的）/ assistant（agent 自己定的）/ api。
	CreatedBy string    `gorm:"column:created_by;size:16"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// 唤醒状态。
const (
	WakeupPending  = "pending"
	WakeupDone     = "done"
	WakeupFailed   = "failed"
	WakeupCanceled = "canceled"
)

func (AgentWakeup) TableName() string { return "agent_wakeup" }
