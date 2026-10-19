package model

import "time"

// AgentProactiveState 是「主动聊天」的每用户状态（借鉴 Cyrene 的策略状态机）。
//
// 一行一个用户，记录：代次（epoch）、未回复计数、上次主动时间 / 场景、
// 各场景最近一次触发时间（JSON），以及主动聊天总开关。
//
// Epoch 是整套机制的防错核心：生成是异步的，生成期间用户可能又说话了。
// 一旦用户有活动就 epoch++，生成结束时发现代次变了就丢弃这条过期消息，
// 否则会出现「用户刚说完话，一条过期的主动消息又冒出来」。
//
// Enabled 这里**刻意不写** gorm 的 default:true：GORM 对带默认值标签的零值字段
// 会在 INSERT 时改用默认值，导致「用户显式关掉主动聊天」被悄悄改回开启
// （与 agent_assistant 踩过的是同一个坑）。「默认开启」由接口层负责——
// 没有这一行时一律按开启处理，见 web/proactive.go 的 loadProactiveState。
type AgentProactiveState struct {
	UserID string `gorm:"column:user_id;primaryKey;size:36"`
	// Epoch 每次用户活动 / 普通对话开始结束都自增；生成前后对比它来判 stale。
	Epoch int64 `gorm:"column:epoch;not null"`
	// UnansweredCount 是「连续发了主动消息、用户都没回」的计数，封顶 2。
	UnansweredCount int `gorm:"column:unanswered_count;not null"`
	// LastProactiveAt / LastNormalConversationAt 用指针：零值 time.Time 会被 MySQL
	// 严格模式拒绝（Incorrect datetime value: '0000-00-00'），nil 写 NULL 表示「还没有过」。
	LastProactiveAt          *time.Time `gorm:"column:last_proactive_at"`
	LastNormalConversationAt *time.Time `gorm:"column:last_normal_conversation_at"`
	// LastProactiveScene 是上一条主动消息的话题（scene id），用于「追加消息不重复同一话题」。
	LastProactiveScene string `gorm:"column:last_proactive_scene;size:255"`
	// LastFiredJSON 是 sceneID → 最近一次触发时间的 JSON 串，用于场景级冷却。
	LastFiredJSON string `gorm:"column:last_fired_json;type:text"`
	// Enabled 是主动聊天总开关；没有状态行时按 true（默认开启）处理。
	Enabled   bool      `gorm:"column:enabled;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (AgentProactiveState) TableName() string { return "agent_proactive_state" }

// AgentProactiveMessage 给「主动消息」打一个旁注标记。
//
// 为什么不把标记塞进 message.content 前缀：message.content 会被历史重建原样喂回模型
// （见 web/chat.go 的 rebuildLLMHistory），前缀会污染模型的上下文；用户编辑、导出、
// 复制这条消息时也会看到它。也不改 message 表：那是全站共享的表，加列要动别人负责的
// 迁移与历史逻辑。单独一张轻量表只回答「这条 message 是主动消息」，正文按 message_id 取，
// 既不动共享表结构，也不污染正文。
type AgentProactiveMessage struct {
	Id             uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID         string `gorm:"column:user_id;index;size:36;not null"`
	MessageID      uint   `gorm:"column:message_id;index;not null"`
	ConversationID uint   `gorm:"column:conversation_id;index;not null"`
	// SceneID 是这条主动消息来自哪个话题（如 memory:123），便于排查与冷却。
	SceneID   string    `gorm:"column:scene_id;size:255"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (AgentProactiveMessage) TableName() string { return "agent_proactive_message" }
