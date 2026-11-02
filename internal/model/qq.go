package model

import "time"

// QQChatBinding 把一个 QQ 聊天（私聊某人 / 某个群）绑到本地的一个会话上。
//
// ChatKey 形如 "private:12345" 或 "group:67890"。同一个 QQ 聊天始终落到同一个会话里，
// 上下文才能跨消息延续；用户发 /new 时只是把绑定改指到一个新会话，旧会话保留可查。
type QQChatBinding struct {
	Id             uint      `gorm:"column:id;primaryKey;autoIncrement"`
	ChatKey        string    `gorm:"column:chat_key;size:64;not null;uniqueIndex"`
	UserID         string    `gorm:"column:user_id;size:36;not null;index"`
	ConversationID uint      `gorm:"column:conversation_id;not null"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (QQChatBinding) TableName() string { return "qq_chat_binding" }
