package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/agent/evolve"
)

// applySkillLintGuard 给 skill_write 套一层「体检」。
//
// 为什么放在装配层而不是改 skill_write 本身：技能创作工具是既有能力的稳定入口，
// 体检是可选策略（AGENT_SKILL_LINT），包装一层可以零风险地开/关。
//
// 三档语义：
//   - off：原样返回，不包装；
//   - warn（默认）：照常保存，但在结果里追加体检提醒，让模型下次写得更规范；
//   - block：有 error 级问题时拒绝写入，把问题清单回灌给模型让它改。
func applySkillLintGuard(tools []agent.Tool) []agent.Tool {
	mode := skillLintMode()
	if mode == "off" {
		return tools
	}
	out := make([]agent.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name() == "skill_write" {
			out = append(out, skillLintGuardTool{inner: t, mode: mode})
			continue
		}
		out = append(out, t)
	}
	return out
}

type skillLintGuardTool struct {
	inner agent.Tool
	mode  string
}

func (g skillLintGuardTool) Name() string               { return g.inner.Name() }
func (g skillLintGuardTool) Description() string        { return g.inner.Description() }
func (g skillLintGuardTool) Parameters() map[string]any { return g.inner.Parameters() }

func (g skillLintGuardTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		// 参数解析交给内层工具，它有自己的报错文案。
		return g.inner.Execute(ctx, raw, ex)
	}
	issues := evolve.Lint(evolve.SkillItem{
		Name: args.Name, Description: args.Description, Body: args.Body, Enabled: true,
	})
	if len(issues) == 0 {
		return g.inner.Execute(ctx, raw, ex)
	}
	text := formatSkillIssues(issues)
	if g.mode == "block" && evolve.HasError(issues) {
		return agent.Result{
			IsError: true,
			Content: "技能体检没通过，没有保存。" + text + "\n请按上面的问题修改后重写。",
			Display: map[string]any{"code": "SKILL_LINT_BLOCKED", "issues": len(issues)},
		}, nil
	}
	res, err := g.inner.Execute(ctx, raw, ex)
	if err != nil || res.IsError {
		return res, err
	}
	res.Content = strings.TrimSpace(res.Content) + "\n（技能体检提醒，已保存）" + text
	return res, nil
}

// formatSkillIssues 把体检问题排成模型能直接照改的清单。
func formatSkillIssues(issues []evolve.Issue) string {
	var b strings.Builder
	for _, it := range issues {
		level := "提醒"
		if it.Severity == "error" {
			level = "错误"
		}
		fmt.Fprintf(&b, "\n- [%s] %s：%s", level, it.Code, it.Message)
	}
	return b.String()
}
