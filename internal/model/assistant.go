package model

import "time"

// AgentAssistant 是一个「自定义助手」：一条可切换的人格 + 它自己的一组配置。
//
// 它与用户级全局设置（AgentSetting）的关系是理解这张表的关键：SoulPersonality / SoulVoice / SoulWork /
// Mode / FSRoot 都是**覆盖项**——空字符串一律表示「继承用户全局设置」，而不是「设为空」。
// 这样用户改全局设置时，没覆盖过的助手会自动跟着变；只有显式填过的助手才固定在自己的值上。
//
// (user_id, name) 唯一：同一用户下助手名不能重复，「保存同名」才有确定语义（拒绝，而不是静默覆盖）。
type AgentAssistant struct {
	Id          uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      string `gorm:"column:user_id;index;uniqueIndex:uniq_assistant_user_name;size:36;not null"`
	Name        string `gorm:"column:name;size:128;not null;uniqueIndex:uniq_assistant_user_name"`
	Description string `gorm:"column:description;size:512"`
	// Model 是这个助手默认使用的模型名；空 = 不指定（沿用系统默认）。
	Model string `gorm:"column:model;size:128"`
	// SoulPersonality / SoulVoice / SoulWork 覆盖用户全局的「人格 / 说话方式 / 做事方式」；空 = 用全局。
	SoulPersonality string `gorm:"column:soul_personality;type:text"`
	SoulGrowth      string `gorm:"column:soul_growth;type:text"`
	SoulVoice       string `gorm:"column:soul_voice;type:text"`
	SoulWork        string `gorm:"column:soul_work;type:text"`
	// Mode 覆盖用户全局的自主档位（plan/confirm/auto）；空 = 用全局。
	Mode string `gorm:"column:agent_mode;size:16"`
	// FSRoot 是这个助手的工作区根目录（文件工具沙箱）；空 = 用用户全局工作区。
	FSRoot string `gorm:"column:fs_root;size:1024"`
	// Enabled 默认启用。这里**不写** gorm 的 default:true：GORM 对带默认值标签的零值字段
	// 会在 INSERT 时改用默认值，导致「显式建一个停用的助手」被悄悄改成启用。
	// 默认 true 由接口层（createAssistant）负责。
	Enabled   bool      `gorm:"column:enabled;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (AgentAssistant) TableName() string { return "agent_assistant" }
