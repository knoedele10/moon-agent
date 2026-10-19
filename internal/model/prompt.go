package model

import "time"

// AgentPrompt 是「提示词库」里的一条：用户攒下来、随时复制到输入框的常用提示词。
//
// 与 AgentAssistant 分开：助手是「人格 + 配置」，提示词只是纯文本片段，没有行为语义，
// 所以不需要 soul / mode / workspace 这些字段，也不与会话绑定。
type AgentPrompt struct {
	Id        uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    string    `gorm:"column:user_id;index;size:36;not null"`
	Title     string    `gorm:"column:title;size:128;not null"`
	Content   string    `gorm:"column:content;type:longtext"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (AgentPrompt) TableName() string { return "agent_prompt" }
