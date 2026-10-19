package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// User 是站点用户。Password 存 bcrypt hash，绝不存明文。
type User struct {
	Id        string    `gorm:"column:id;primaryKey;size:36"`
	Username  string    `gorm:"column:username;uniqueIndex;size:64;not null"`
	Password  string    `gorm:"column:password;size:100;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.Id == "" {
		u.Id = uuid.New().String()
	}
	return nil
}

// UserKey 是用户自己填的某个端点的 API Key，用于覆盖服务端默认 key。
// APIKey 字段存的是 AES-GCM 密文（base64），不是明文。
type UserKey struct {
	Id        uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    string    `gorm:"column:user_id;index;size:36;not null"`
	Endpoint  string    `gorm:"column:endpoint;size:64;not null"`
	APIKey    string    `gorm:"column:api_key;type:text;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// Conversation 是一次多轮会话。
type Conversation struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;index;size:36;not null"`
	Title  string `gorm:"column:title;size:255"`
	Model  string `gorm:"column:model;size:128"`
	// Kind 是这个会话的**用途**：chat（日常聊天/陪伴）、work（辅助工作）、
	// code（代码协作）、learn（学习陪伴）。空串按 work 处理——升级前建的会话都是
	// 工作型，行为不变。
	//
	// 它和 agent_setting.agent_mode（plan/confirm/auto）是**两个正交维度**：
	// Kind 回答「这个会话在干什么」（决定给不给工具、用什么语气、记忆算哪一类），
	// Mode 回答「能不能放手让它做」（决定风险操作要不要先问）。
	// 设计参考 Cyrene-Agent 的四模式（Chat/Work/Code/Learn）——它的设计文档里
	// 专门论证过这两件事不能混成一个维度。
	Kind string `gorm:"column:kind;size:16"`
	// Summary / SummaryUpto 是长对话的滚动摘要（见 web/agent_compact.go）。
	// SummaryUpto 记「摘要已覆盖到哪一条 message.id」，用来判断要不要重算摘要。
	Summary     string    `gorm:"column:summary;type:longtext"`
	SummaryUpto uint      `gorm:"column:summary_upto;not null;default:0"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// Message 是会话里的一条消息。Role 取 system / user / assistant。
type Message struct {
	Id             uint      `gorm:"column:id;primaryKey;autoIncrement"`
	ConversationID uint      `gorm:"column:conversation_id;index;not null"`
	Role           string    `gorm:"column:role;size:16;not null"`
	Content        string    `gorm:"column:content;type:longtext"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (User) TableName() string         { return "user" }
func (UserKey) TableName() string      { return "user_key" }
func (Conversation) TableName() string { return "conversation" }
func (Message) TableName() string      { return "message" }
