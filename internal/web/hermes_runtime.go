package web

import (
	"os"

	"blog-my/internal/agent"
)

// assembleRuntimeTools 是主会话、子会话、确认重放共用的能力装配入口。
// 动态工具也经过原有用途、执行档位和子级策略，避免追加工具绕开权限。
func assembleRuntimeTools(base []agent.Tool, p agentToolPolicy, evo *hermesEvolve) ([]agent.Tool, agent.ToolSource) {
	extras := evo.Tools()
	if evo.Enabled() {
		extras = append(extras, sessionSearchTool{userID: p.UserID})
	}
	// MCP 使用进程级配置，可能持有本机路径和服务凭证，只交给主账号工作会话。
	if runtimeIncludesToolset(toolsetMCP) && isOwnerUser(p.UserID) && normalizeKind(p.Kind) == KindWork && normalizeMode(p.Mode) != ModePlan {
		extras = append(extras, mcpTools()...)
	}
	base = append(base, runtimeExtrasForPolicy(extras, p)...)
	base = filterToolsByToolsets(base, parseToolsetList(os.Getenv("AGENT_TOOLSETS")))
	base = applySkillLintGuard(base)
	return buildAgentToolSourceForUser(base, p.UserID)
}

func runtimeIncludesToolset(name string) bool {
	want := parseToolsetList(os.Getenv("AGENT_TOOLSETS"))
	if len(want) == 0 {
		return true
	}
	for _, set := range want {
		if set == name {
			return true
		}
	}
	return false
}

// runtimeCapability 将扩展能力映射到已存在的用途白名单，避免维护两套权限。
func runtimeCapability(name string) string {
	switch name {
	case "memory_note", "memory_forget":
		return "memory_write"
	case "memory_list", "session_search", "evolution_report":
		return "memory_search"
	case "skill_list", "skill_stats", "skill_lint":
		return "skill_read"
	case "skill_improve", "skill_archive":
		return "skill_write"
	}
	return name
}

func runtimeExtrasForPolicy(tools []agent.Tool, p agentToolPolicy) []agent.Tool {
	allowed := profileFor(p.Kind).Tools
	out := make([]agent.Tool, 0, len(tools))
	for _, tool := range tools {
		name := tool.Name()
		if allowed != nil && !allowed[runtimeCapability(name)] {
			continue
		}
		if normalizeMode(p.Mode) == ModePlan && riskyTool(name) {
			continue
		}
		if riskyTool(name) && normalizeMode(p.Mode) != ModeAuto && !p.Confirmed {
			if p.Child {
				tool = subagentDenyTool{inner: tool}
			} else {
				tool = confirmTool{inner: tool}
			}
		}
		out = append(out, tool)
	}
	return out
}

// 自动固化需要本轮原本就允许自主创作技能；子级只提供报告交由主级处理。
func runtimeMayLearn(p agentToolPolicy) bool {
	allowed := profileFor(p.Kind).Tools
	return !p.Child && normalizeMode(p.Mode) == ModeAuto && runtimeIncludesToolset(toolsetSkill) &&
		(allowed == nil || allowed["skill_write"])
}
