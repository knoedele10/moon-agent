package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/agent/skill"
)

// AgentSkill 是一条技能：一段写给模型的“怎么做某件事”的说明书。
//
// 为什么放数据库而不是直接读文件系统：
//  1. 桌面应用要能在界面上开关技能，不想让用户去翻目录；
//  2. 技能是**按用户**的（不同账号可以用不同技能集）；
//  3. 避免为了列技能去递归扫描磁盘（技能库可能上千个文件）。
//
// 渐进式披露：提示词里只放 name + description（一行一个），
// 正文靠 skill_read 工具按需取——否则几百个技能的全文会把上下文直接撑爆。
type AgentSkill struct {
	Id uint `gorm:"column:id;primaryKey;autoIncrement"`
	// (user_id, name) 建唯一索引：同名技能只能有一条，导入脚本才能用 upsert 幂等重跑。
	UserID      string `gorm:"column:user_id;index;uniqueIndex:uniq_skill_user_name;size:36;not null"`
	Name        string `gorm:"column:name;size:128;not null;uniqueIndex:uniq_skill_user_name"`
	Description string `gorm:"column:description;size:1024"`
	Body        string `gorm:"column:body;type:longtext"`
	Enabled     bool   `gorm:"column:enabled;not null;default:false"`
	Source      string `gorm:"column:source;size:512"` // 从哪导入的（便于溯源与重复导入去重）
	// Embedding 是 name+description 的向量（JSON []float32），语义召回用。
	// 存 JSON 而不是二进制 blob：维度随嵌入模型变，可读性比省几个字节重要，
	// 技能是几百条量级，体积无所谓。为空 = 还没算过（Ollama 没开时就是空的）。
	Embedding string    `gorm:"column:embedding;type:mediumtext"`
	EmbedDim  int       `gorm:"column:embed_dim"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (AgentSkill) TableName() string { return "agent_skill" }

// SaveSkillEmbedding 保存一条技能的向量。
//
// 惰性补算：导入不依赖 Ollama（没装/没开也能导），第一次召回时再顺手把缺的向量补上。
func SaveSkillEmbedding(id uint, vec []float32) error {
	if DB == nil || id == 0 || len(vec) == 0 {
		return nil
	}
	b, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	return DB.Model(&AgentSkill{}).Where("id = ?", id).
		Updates(map[string]any{"embedding": string(b), "embed_dim": len(vec)}).Error
}

// DecodeEmbedding 解析存下来的向量。空值/坏值一律当作“没有语义信号”，
// 绝不让一条脏数据把整次召回打断。
func DecodeEmbedding(raw string) []float32 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var v []float32
	if err := json.Unmarshal([]byte(raw), &v); err != nil || len(v) == 0 {
		return nil
	}
	return v
}

// SkillImportStats 是一次目录导入的结果。
//
// Files 与 Imported 分开报，是为了让用户看懂“为什么扫到 111 个 SKILL.md 却只进 110 条”：
// 技能库里同名副本很多（gstack 下每个宿主目录都同步一份），去重后才是技能数。
// 注意 Files 是**这次扫描实际收下的**文件数；盘上总数（含隐藏目录里的同步副本）可能远大于它。
type SkillImportStats struct {
	Root       string   `json:"root"`
	Files      int      `json:"files"`
	Imported   int      `json:"imported"`
	Created    int      `json:"created"`
	Updated    int      `json:"updated"`
	Duplicates []string `json:"duplicates,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// UpsertSkill 按 (user_id, name) 写入或更新一条技能。
//
// 用「先查后写」而不是 ON DUPLICATE KEY UPDATE：行为与数据库无关，
// 也方便统计“新建了几条、更新了几条”（MySQL 的 affected-rows 语义太绕）。
// created=true 表示新建，false 表示覆盖了已有同名技能。
//
// enabled 是三态：nil 表示“别动已有记录的开关”（新建的默认启用），
// true/false 表示强制设置。重跑导入不该把用户在界面上手工关掉的技能又打开。
func UpsertSkill(userID string, it skill.Item, enabled *bool) (created bool, err error) {
	if DB == nil {
		return false, errors.New("数据库未初始化")
	}
	userID = strings.TrimSpace(userID)
	name := strings.TrimSpace(it.Name)
	if userID == "" {
		return false, errors.New("user_id 不能为空")
	}
	if name == "" {
		return false, errors.New("技能名不能为空")
	}
	if strings.TrimSpace(it.Body) == "" {
		return false, fmt.Errorf("技能 %q 正文为空", name)
	}

	// 用 Find 而不是 First：First 查不到会返回 ErrRecordNotFound，
	// 批量导入时这会在日志里刷出上百行“record not found”噪声（它不是错误，是正常分支）。
	var rows []AgentSkill
	if err := DB.Where("user_id = ? AND name = ?", userID, name).Limit(1).Find(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) > 0 {
		row := rows[0]
		row.Description = it.Description
		row.Body = it.Body
		if enabled != nil {
			row.Enabled = *enabled
		}
		if strings.TrimSpace(it.Source) != "" {
			row.Source = it.Source
		}
		if err := DB.Save(&row).Error; err != nil {
			return false, err
		}
		return false, nil
	}

	// 新建的技能默认启用：用户导入一整个技能库，意图显然是“我可以用它们”。
	on := true
	if enabled != nil {
		on = *enabled
	}
	row := AgentSkill{
		UserID: userID, Name: name, Description: it.Description, Body: it.Body,
		Enabled: on, Source: it.Source,
	}
	if err := DB.Create(&row).Error; err != nil {
		return false, err
	}
	return true, nil
}

// ImportSkillDir 扫描一个目录（通常就是技能库根目录）并把它下面的 SKILL.md 全部导入。
//
// 幂等：同一份技能库重复导入只会更新正文与简介，不会产生重复行，
// 也不会把用户在界面上手工关掉的技能重新打开——除非 enabled 显式传 true/false。
// （enabled 为 nil 时保留每条已有技能自己的开关，这是导入器“重跑”的语义。）
func ImportSkillDir(userID, root string, enabled *bool, opt skill.ScanOptions) (SkillImportStats, error) {
	stats := SkillImportStats{}
	if DB == nil {
		return stats, errors.New("数据库未初始化")
	}
	rep, err := skill.ScanDir(root, opt)
	if err != nil {
		return stats, err
	}
	stats.Root = rep.Root
	stats.Files = rep.Files
	stats.Duplicates = rep.Duplicates
	stats.Errors = append(stats.Errors, rep.Errors...)

	for _, it := range rep.Items {
		created, err := UpsertSkill(userID, it, enabled)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", it.Name, err))
			continue
		}
		stats.Imported++
		if created {
			stats.Created++
		} else {
			stats.Updated++
		}
	}
	return stats, nil
}
