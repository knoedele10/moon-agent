package web

import (
	"fmt"
	"strings"

	"blog-my/internal/model"
)

// skillPromptBlock 生成系统提示词里的技能清单。
//
// 分两段：前 skillPromptLimit 条带简介，其余**只列名字**。
// 只列名字这一段是给“技能库上百条”准备的——全文太大，光名字却很小，
// 而模型只要能看见名字，就能用 skill_search / skill_read 拿到细节。
// 只查 id/name/description：正文（longtext，单条可达几十 KB）不该为了拼提示词读进内存。
func skillPromptBlock(userID string) string {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return ""
	}
	var rows []model.AgentSkill
	if err := model.DB.Select("id, name, description").
		Where("user_id = ? AND enabled = ?", userID, true).
		Order("name asc").Find(&rows).Error; err != nil || len(rows) == 0 {
		return ""
	}

	detail := rows
	if len(detail) > skillPromptLimit {
		detail = detail[:skillPromptLimit]
	}

	var b strings.Builder
	b.WriteString("\n可用技能（先看名字；要正文用 skill_read，找技能用 skill_search）：\n")
	for _, r := range detail {
		b.WriteString("- ")
		b.WriteString(r.Name)
		if d := strings.TrimSpace(r.Description); d != "" {
			b.WriteString("：")
			b.WriteString(clipRunes(d, 160))
		}
		b.WriteString("\n")
	}
	if len(rows) > len(detail) {
		names := make([]string, 0, len(rows)-len(detail))
		for _, r := range rows[len(detail):] {
			names = append(names, r.Name)
		}
		listed := names
		if len(listed) > skillNameLimit {
			listed = listed[:skillNameLimit]
		}
		b.WriteString("其余已启用技能（只列名字，细节用 skill_search / skill_read）：")
		b.WriteString(strings.Join(listed, "、"))
		b.WriteString("\n")
		if len(listed) < len(names) {
			b.WriteString(fmt.Sprintf("（另有 %d 个技能连名字都没列出，用 skill_search 按关键词找）\n", len(names)-len(listed)))
		}
	}
	b.WriteString(fmt.Sprintf("（当前共 %d 个已启用技能）\n", len(rows)))
	return b.String()
}
