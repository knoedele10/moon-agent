package web

import (
	"context"
	"encoding/json"
	"strings"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/agent/toolkit"
)

// agentToolPolicy keeps tool selection and permission wrappers in one place.
// Kind selects capabilities; Mode and Child decide who may execute risky tools.
type agentToolPolicy struct {
	UserID      string
	AssistantID uint
	Root        string
	Roots       []string
	Kind        string
	Mode        string
	Confirmed   bool
	Depth       int
	Child       bool
}

// agentToolsFor preserves the unfiltered tool catalogue used by pending calls
// and existing callers. Normal chat runs use buildAgentTools with their Kind.
func agentToolsFor(userID, root, mode string, confirmed bool, depth int, roots ...string) []agent.Tool {
	return buildAgentTools(agentToolPolicy{
		UserID: userID, Root: root, Roots: roots, Mode: mode, Confirmed: confirmed, Depth: depth,
	})
}

func buildAgentTools(p agentToolPolicy) []agent.Tool {
	userID, root, mode, confirmed, depth := p.UserID, p.Root, p.Mode, p.Confirmed, p.Depth
	// 模式一（plan）：只列计划。文件类工具几乎全不给，只留两个例外：
	//   1. 记忆工具——不动用户文件，所有档位都开放；
	//   2. 只允许写 .md 的 write——把计划存档是**默认操作**（用户明确要求）。
	// edit / move / delete / read_image 一律不给。
	if normalizeMode(mode) == ModePlan {
		tools := kbMemoryTools(userID)
		// 技能工具是只读的（搜索/阅读说明书），所有档位都给。
		tools = append(tools, skillTools(userID)...)
		if strings.TrimSpace(root) != "" {
			// 计划模式唯一允许的写操作（存 .md）也要守「工作区首选」，但比其它档更严：
			// 工作区外直接拒绝，不给确认入口——plan 档的本意是「只列计划」，
			// 不能借一次确认把文件写到工作区外。拒绝逻辑在 planWriteTool 里。
			tools = append(tools, planWriteTool{inner: agentfs.WriteTool{}, roots: p.Roots})
		}
		return finalizeAgentTools(tools, p)
	}
	var tools []agent.Tool

	// 通用工具：不碰磁盘、不需要工作区，任何会话都给。
	tools = append(tools,
		toolkit.CalculatorTool{},
		toolkit.DateTimeTool{},
		toolkit.WebSearchTool{},
		toolkit.WebReaderTool{},
		toolkit.AskUserTool{},
		toolkit.JSONTool{},
		toolkit.SQLTool{},
	)

	// 文件工具只在有工作区时注册——没有就一个都不给，模型也就无从写盘。
	if strings.TrimSpace(root) != "" {
		tools = append(tools, agentfs.ToolsWithMutation()...)
	}

	tools = append(tools, kbMemoryTools(userID)...)
	tools = append(tools, soulEvolveTool{userID: userID, assistantID: p.AssistantID})
	tools = append(tools, skillTools(userID)...)
	// 技能创作：写的是技能库（不动用户文件），但会改变助手以后的行为，
	// 所以归入风险工具——confirm 档会先问一句；plan 档根本不给（那档只列计划）。
	tools = append(tools, skillWriteTool{userID: userID})
	// 技能脚本执行只给非 plan 档位（plan 档不跑代码）。它属于风险工具，
	// 在 confirm 档会被下面的确认壳拦下来先问用户。
	tools = append(tools, skillRunTools(userID)...)
	// 命令执行只在有工作区的非 plan 会话中注册；子 agent 在 auto 档可实际构建，
	// confirm 档会走主会话确认，plan 档不会注册。工具自身还会校验 cwd 与命令白名单。
	if strings.TrimSpace(root) != "" {
		tools = append(tools, runCommandTools()...)
	}
	// 模式二（confirm）：风险工具包一层“先问用户”。
	// confirmed=true 表示用户刚刚确认过，这一次放行真正的工具。
	// 外部路径确认壳先包（在风险确认的内层）：write/edit/move/delete/office_edit 在非 auto 档
	// 碰到工作区外的绝对路径时先停下来问用户。confirmTool 会把外部调用让给这层壳，
	// 所以一次调用只弹一次窗，且外部提示里带确切路径。
	if !confirmed {
		tools = wrapOutsideWorkspace(tools, root, mode, p.Roots...)
	}
	if normalizeMode(mode) == ModeConfirm && !confirmed {
		for i, t := range tools {
			if riskyTool(t.Name()) {
				tools[i] = confirmTool{inner: t}
			}
		}
	}
	// 子 agent 委派：还没到深度上限时才给这两个工具。
	// 放在 confirm 包裹**之后**：委派本身不动用户文件，不该让用户再确认一次；
	// 真正的风险操作发生在子 agent 内部，那里有更严的策略（见 web/subagent.go）。
	runner := newSubagentRunner()
	if depth < subagentMaxDepth() {
		tools = append(tools,
			agent.SubagentTool{Delegator: runner, Depth: depth, MaxDepth: subagentMaxDepth(), Mode: mode, Root: root, Roots: append([]string(nil), p.Roots...)},
			agent.ListSubagentsTool{Delegator: runner},
		)
	}
	// 团队层（docs/agent/07-team.md）：成员即使到达最大委派深度，也仍须能
	// 收发消息、查看并更新共享任务板。深度上限只限制继续派生，不限制成员协作。
	team := newTeamService()
	tools = append(tools,
		agent.SendMessageTool{Service: team},
		agent.ListAgentsTool{Service: team},
		agent.WaitAgentTool{Service: team},
		agent.TeamTaskCreateTool{Service: team},
		agent.TeamTaskListTool{Service: team},
		agent.TeamTaskGetTool{Service: team},
		agent.TeamTaskUpdateTool{Service: team},
	)
	// 只有仍有深度预算的 lead 才能创建新成员；teamService 还会校验 lead 身份。
	if depth < subagentMaxDepth() {
		tools = append(tools, agent.SpawnTeammateTool{Service: team, Mode: mode, Root: root, Roots: append([]string(nil), p.Roots...)})
	}
	return finalizeAgentTools(tools, p)
}

// finalizeAgentTools applies the conversation Kind and child confirmation policy.
// This order matches the original flow: register, wrap for mode, filter for Kind,
// then deny unconfirmable child operations.
func finalizeAgentTools(tools []agent.Tool, p agentToolPolicy) []agent.Tool {
	tools = filterToolsByKind(tools, p.Kind)
	if normalizeMode(p.Mode) == ModeAuto {
		for i, t := range tools {
			if t.Name() == "ask_user" {
				tools[i] = autonomousAskTool{}
			}
		}
	}
	if !p.Child || normalizeMode(p.Mode) == ModeAuto {
		return tools
	}
	for i, tool := range tools {
		if riskyTool(tool.Name()) {
			tools[i] = subagentDenyTool{inner: tool}
		}
	}
	return tools
}

// subagentToolsFor gives the child the same catalogue and Kind as its parent,
// then applies the child-specific confirmation rule above.
func subagentToolsFor(userID string, req agent.SubagentRequest, depth int) []agent.Tool {
	return buildAgentTools(agentToolPolicy{
		UserID: userID, Root: req.Root, Roots: req.Roots, Kind: req.Kind, Mode: req.Mode,
		Depth: depth, Child: true,
	})
}

// A child has no live user approval channel. It reports risky work to its parent.
type subagentDenyTool struct{ inner agent.Tool }

const subagentDenyMessage = "子 agent 不能执行需要用户确认的操作，请把这件事写进结论让主 agent 处理"

func (d subagentDenyTool) Name() string               { return d.inner.Name() }
func (d subagentDenyTool) Description() string        { return d.inner.Description() }
func (d subagentDenyTool) Parameters() map[string]any { return d.inner.Parameters() }

func (d subagentDenyTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	return agent.Result{
		IsError:   true,
		Content:   subagentDenyMessage,
		AwaitUser: subagentDenyMessage,
		Display:   map[string]any{"code": "SUBAGENT_DENIED", "tool": d.inner.Name()},
	}, nil
}
