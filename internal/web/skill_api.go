package web

import (
	"net/http"
	"strings"

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
