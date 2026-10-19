package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/agent/skill"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 技能（skill）管理：把“怎么做某件事”的说明书存起来，让模型按需查阅。
//
// 设计要点是**渐进式披露**：
//   - 系统提示词里只列已启用技能的 name + description（一行一个，且有条数上限）；
//   - 需要正文时模型自己调 skill_read 取；
//   - 技能多到列不完时，用 skill_search 按关键词找。
//
// 这样即使技能库有几百个，也不会把上下文撑爆。

const (
	// skillPromptLimit 是提示词里**带简介**列出的条数。
	skillPromptLimit = 30
	// skillNameLimit 是简介列满之后，再**只列名字**的条数上限。
	// 技能库上百条时，光名字很小（几 KB），却能让模型知道“有这么个技能”；
	// 早期版本超限的技能连名字都不出现，模型只能靠瞎猜关键词去 search。
	skillNameLimit = 400
	// skillBodyMaxRunes 限制单次 skill_read 返回的正文长度（防止一个超大技能吃光上下文）。
	skillBodyMaxRunes = 6000
	// skillSearchDefault / skillSearchMax 是搜索返回条数。
	skillSearchDefault = 8
	skillSearchMax     = 20
)

// SKILL.md 的解析统一放在 agent/skill 包里：web 层与导入器共用一份，
// 两边各写一份必然会漂移（这里原本就抄了一份，块标量 description: > 的
// 处理只修在了其中一边）。

// ---------- HTTP 接口 ----------

type skillItem struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
	BodyRunes   int    `json:"body_runes"`
}

func listSkills(c *gin.Context) {
	userID := currentUserID(c)
	var rows []model.AgentSkill
	q := model.DB.Where("user_id = ?", userID).Order("enabled desc, name asc")
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + s + "%"
		q = q.Where("name LIKE ? OR description LIKE ?", like, like)
	}
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取技能失败"})
		return
	}
	items := make([]skillItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, skillItem{
			ID: r.Id, Name: r.Name, Description: r.Description,
			Enabled: r.Enabled, Source: r.Source, BodyRunes: len([]rune(r.Body)),
		})
	}
	c.JSON(http.StatusOK, gin.H{"skills": items, "total": len(items)})
}

func getSkill(c *gin.Context) {
	userID := currentUserID(c)
	var s model.AgentSkill
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&s).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "技能不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": s.Id, "name": s.Name, "description": s.Description, "body": s.Body, "enabled": s.Enabled})
}

func createSkill(c *gin.Context) {
	userID := currentUserID(c)
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
		Markdown    string `json:"markdown"` // 直接贴一段 SKILL.md 也行，会自动解析 frontmatter
		Enabled     *bool  `json:"enabled"`
		Source      string `json:"source"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	name, desc, text := strings.TrimSpace(body.Name), strings.TrimSpace(body.Description), body.Body
	if strings.TrimSpace(body.Markdown) != "" {
		pn, pd, pb := skill.ParseMarkdown(body.Markdown)
		if name == "" {
			name = pn
		}
		if desc == "" {
			desc = pd
		}
		text = pb
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "技能名不能为空"})
		return
	}
	if strings.TrimSpace(text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "技能正文不能为空"})
		return
	}

	enabled := false
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	// 同名视为更新（导入脚本重复跑不会产生一堆重复技能）。
	var s model.AgentSkill
	err := model.DB.Where("user_id = ? AND name = ?", userID, name).First(&s).Error
	if err == nil {
		s.Description, s.Body, s.Enabled = desc, text, enabled
		if strings.TrimSpace(body.Source) != "" {
			s.Source = body.Source
		}
		if err := model.DB.Save(&s).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存技能失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": s.Id, "updated": true})
		return
	}

	s = model.AgentSkill{
		UserID: userID, Name: name, Description: desc, Body: text,
		Enabled: enabled, Source: body.Source,
	}
	if err := model.DB.Create(&s).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存技能失败"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true, "id": s.Id})
}

func updateSkill(c *gin.Context) {
	userID := currentUserID(c)
	var body struct {
		Enabled     *bool   `json:"enabled"`
		Description *string `json:"description"`
		Body        *string `json:"body"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	var s model.AgentSkill
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&s).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "技能不存在"})
		return
	}
	if body.Enabled != nil {
		s.Enabled = *body.Enabled
	}
	if body.Description != nil {
		s.Description = strings.TrimSpace(*body.Description)
	}
	if body.Body != nil {
		s.Body = *body.Body
	}
	if err := model.DB.Save(&s).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存技能失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "enabled": s.Enabled})
}

func deleteSkill(c *gin.Context) {
	userID := currentUserID(c)
	res := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).Delete(&model.AgentSkill{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除技能失败"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "技能不存在"})
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------- 给模型用的两个技能工具 ----------

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

// skillSearchTool 让模型按关键词找技能。
type skillSearchTool struct{ userID string }

func (t skillSearchTool) Name() string { return "skill_search" }

func (t skillSearchTool) Description() string {
	return "按关键词搜索已启用的技能，返回技能名与简介。需要某项专门能力（写文档、做设计、处理表格等）但不知道有没有对应技能时用它。"
}

func (t skillSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "关键词，例如“文档”“设计”“表格”"},
			"limit": map[string]any{"type": "integer", "description": "返回条数（默认 8，最多 20）"},
		},
		"required": []string{"query"},
	}
}

func (t skillSearchTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return agent.Result{}, fmt.Errorf("query 不能为空")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = skillSearchDefault
	}
	if limit > skillSearchMax {
		limit = skillSearchMax
	}
	if model.DB == nil {
		return agent.Result{}, fmt.Errorf("数据库未初始化")
	}

	like := "%" + q + "%"
	var rows []model.AgentSkill
	if err := model.DB.Where("user_id = ? AND enabled = ? AND (name LIKE ? OR description LIKE ? OR body LIKE ?)",
		t.userID, true, like, like, like).Order("name asc").Limit(limit).Find(&rows).Error; err != nil {
		return agent.Result{}, fmt.Errorf("搜索技能失败: %v", err)
	}
	if len(rows) == 0 {
		return agent.Result{Content: "没有匹配的已启用技能。可以换个关键词，或让用户在界面上启用更多技能。"}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "匹配到 %d 个技能：\n", len(rows))
	for _, r := range rows {
		b.WriteString("- ")
		b.WriteString(r.Name)
		if d := strings.TrimSpace(r.Description); d != "" {
			b.WriteString("：")
			b.WriteString(clipRunes(d, 200))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n用 skill_read 读取某个技能的完整说明。")
	return agent.Result{Content: b.String(), Display: map[string]any{"query": q, "count": len(rows)}}, nil
}

// skillReadTool 让模型读取某个技能的完整正文。
type skillReadTool struct{ userID string }

func (t skillReadTool) Name() string { return "skill_read" }

func (t skillReadTool) Description() string {
	return "读取某个技能的完整说明（正文可能很长）。准备按某个技能做事之前用它；先 skill_search 找名字。"
}

func (t skillReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "技能名（来自技能清单或 skill_search 的结果）"},
		},
		"required": []string{"name"},
	}
}

func (t skillReadTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return agent.Result{}, fmt.Errorf("name 不能为空")
	}
	if model.DB == nil {
		return agent.Result{}, fmt.Errorf("数据库未初始化")
	}

	var s model.AgentSkill
	if err := model.DB.Where("user_id = ? AND name = ?", t.userID, name).First(&s).Error; err != nil {
		return agent.Result{}, fmt.Errorf("没有名为 %q 的技能（用 skill_search 找准确名字）", name)
	}
	if !s.Enabled {
		return agent.Result{}, fmt.Errorf("技能 %q 当前是关闭状态，请让用户在界面上启用它", name)
	}

	body := s.Body
	truncated := false
	if len([]rune(body)) > skillBodyMaxRunes {
		body = string([]rune(body)[:skillBodyMaxRunes])
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "技能：%s\n", s.Name)
	if d := strings.TrimSpace(s.Description); d != "" {
		fmt.Fprintf(&b, "简介：%s\n", d)
	}
	b.WriteString("\n")
	b.WriteString(body)
	if truncated {
		b.WriteString("\n\n...（技能正文较长，已截断）")
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{"name": s.Name, "runes": len([]rune(s.Body)), "truncated": truncated},
	}, nil
}

// skillTools 返回绑定到某个用户的技能工具。
func skillTools(userID string) []agent.Tool {
	return []agent.Tool{skillSearchTool{userID: userID}, skillReadTool{userID: userID}}
}
