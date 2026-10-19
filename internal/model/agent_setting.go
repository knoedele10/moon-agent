package model

import "time"

// AgentSetting 保存用户级 agent 配置，包含文件工具可使用的一个或多个工作区。
//
// 为什么放数据库而不是只用环境变量：环境变量是**进程级**的，多用户部署下没法每人一个
// 工作区；而且桌面端用户需要「我自己设一个目录」的入口，不能要求改配置文件再重启。
// 环境变量仍然保留为默认值：用户没设过时回落到 AGENT_FS_ROOT。
type AgentSetting struct {
	UserID string `gorm:"column:user_id;primaryKey;size:36"`
	FSRoot string `gorm:"column:fs_root;size:1024"`
	// WorkspaceRoots 是用户同时启用的工作区目录列表，JSON 编码；FSRoot 保留为主目录，兼容旧配置。
	WorkspaceRoots string `gorm:"column:workspace_roots;type:text"`
	// CodeRoot / LearnRoot 是「代码协作」「学习陪伴」两个模式的**专属默认工作区**。
	// 为什么单独存：这两个模式的默认目录通常和通用工作区不同（代码目录 vs 笔记目录），
	// 每次手选一遍很烦。空串 = 没设，回落到 FSRoot（行为与升级前一致）。
	// 安全边界不变：文件工具始终受工作区沙箱约束，这里只是换个默认根。
	CodeRoot  string `gorm:"column:code_root;size:1024"`
	LearnRoot string `gorm:"column:learn_root;size:1024"`
	// Mode 是 agent 的自主档位：plan（只列计划）/ confirm（风险操作先问）/ auto（自主决定）。
	// 空串按 confirm 处理——默认“先问”，而不是默认放开。
	Mode string `gorm:"column:agent_mode;size:16"`
	// LastMemoryAt 是「上一次把会话补入知识库」的时间点。
	// 关闭时用它圈出还没入库的消息，避免重复写、也不漏掉。
	// 用指针：零值 time.Time 会被 MySQL 严格模式拒绝（Incorrect datetime value: '0000-00-00'），
	// nil 则写 NULL，表示“从未补齐过”。
	LastMemoryAt *time.Time `gorm:"column:last_memory_at"`
	// ProfileTurns 持久化画像刷新计数，避免进程重启后累计轮数清零。
	ProfileTurns int `gorm:"column:profile_turns;not null;default:0"`
	// SoulPersonality / SoulVoice / SoulWork 是用户给这个助手设的“灵魂”：人格、说话方式与做事方式。
	// 与自动汇总的画像不同：灵魂是**用户亲手写的**，优先级更高，每轮都注入。
	SoulPersonality string    `gorm:"column:soul_personality;type:text"`
	SoulGrowth      string    `gorm:"column:soul_growth;type:text"`
	SoulVoice       string    `gorm:"column:soul_voice;type:text"`
	SoulWork        string    `gorm:"column:soul_work;type:text"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (AgentSetting) TableName() string { return "agent_setting" }
