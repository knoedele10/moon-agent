package model

import "time"

// AgentPendingTool 是一条「已被拦下、等用户确认」的风险工具调用。
//
// 为什么要把调用本身存下来：用户点确认之后，如果只是把「确认」当成一条普通消息
// 再喂给模型、指望它自己把同一个调用重发一遍——小模型经常不重发（实测
// qwen2.5:7b 收到「确认」后只会回一句话，工具压根没跑），于是用户点了确认却
// 什么都没发生。把调用原样存下来、确认时直接执行，就和模型听不听话无关了。
//
// 一个会话同时最多一条：confirm 档会在**第一个**风险调用处停轮。
type AgentPendingTool struct {
	ConversationID uint      `gorm:"column:conversation_id;primaryKey"`
	UserID         string    `gorm:"column:user_id;index;size:36"`
	ToolID         string    `gorm:"column:tool_id;size:128"`
	ToolName       string    `gorm:"column:tool_name;size:64;not null"`
	Arguments      string    `gorm:"column:arguments;type:longtext"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (AgentPendingTool) TableName() string { return "agent_pending_tool" }
