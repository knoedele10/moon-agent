package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// ---------- 给模型用的两个技能工具 ----------
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
