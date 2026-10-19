package model

import "time"

// 子 agent 委派的运行记录（契约 docs/agent/06-subagent.md §3.2）。
//
// 一行 = 一轮：父子会话关系、状态机、收尾文本都在这里。
// 同一个子会话可以有多行（v2 的 send_message 续跑），所以「最新一行」才是成员当前状态。
// 完成通知的正文落 message 表（role="subagent"），结构化字段则由本表按父会话查出——
// 这样 message 表不用加列，前端也能拿到 running 这种「还没有通知消息」的状态。

// 委派的状态机取值（契约 §3.1 的 SubagentInfo.Status 同一套字面量）。
const (
	SubagentRunning = "running"
	SubagentDone    = "done"
	SubagentFailed  = "failed"
	SubagentAborted = "aborted"
)

type AgentSubagentRun struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;size:36;not null"`
	// ParentConversationID 是「派活的那条会话」；通知消息落它下面，唤醒也按它判。
	ParentConversationID uint `gorm:"column:parent_conversation_id;index;not null"`
	// ChildConversationID 是这一轮跑在哪个子会话里；一行 run = 一轮。
	// 注意：v2 的「给已结束成员发消息 = 同一子会话起一轮续跑」会在同一个
	// ChildConversationID 上留下**多行** run（一行 = 一轮），所以这里只能是普通索引；
	// 老库上的历史唯一索引由 model/team.go 的 migrateTeamSchema 显式降级。
	ChildConversationID uint `gorm:"column:child_conversation_id;index;not null"`
	// RootConversationID 是整棵树的根会话：容量按它统计（契约 §2），
	// 所以嵌套派活时子级的子级也计入同一个池子。
	RootConversationID uint   `gorm:"column:root_conversation_id;index;not null"`
	Depth              int    `gorm:"column:depth;not null"`
	Label              string `gorm:"column:label;size:120"`
	// Name 是团队成员的短名（^[a-z][a-z0-9-]{1,31}$，父会话内唯一、永不复用）；
	// v1 的匿名委派为空串，团队名册只收有名字的行。
	Name   string `gorm:"column:name;size:64;index"`
	Prompt string `gorm:"column:prompt;type:longtext"`
	// ContextMode 取 fresh / fork（契约 §3.1）。
	ContextMode string    `gorm:"column:context_mode;size:16"`
	Status      string    `gorm:"column:status;size:16;index"`
	StopReason  string    `gorm:"column:stop_reason;size:32"`
	Reply       string    `gorm:"column:reply;type:longtext"`
	Progress    string    `gorm:"column:progress;type:longtext"`
	Activity    string    `gorm:"column:activity;type:text"`
	ErrText     string    `gorm:"column:err_text;type:text"`
	Model       string    `gorm:"column:model;size:128"`
	Kind        string    `gorm:"column:kind;size:16"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	// FinishedAt 用指针：还在跑的委派写 NULL，而不是零值时间
	// （MySQL 严格模式会拒绝 '0000-00-00'）。
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

func (AgentSubagentRun) TableName() string { return "agent_subagent_run" }
