package web

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
)

// 本文件把「平铺工具表」装配成 toolsys.Catalog，供两种用法：
//   - AGENT_TOOL_DISCLOSURE=search：只给模型核心工具 + tool_search，其余按需激活；
//   - 默认（off）：不装配目录，行为与改造前完全一致。
//
// 另外 AGENT_TOOL_SPILL_BYTES>0 时给每个工具套一层「大结果落盘」，
// 模型读不完的结果不再被截断丢掉，而是给路径让它分段读。

// lazyTool 允许「先建目录、后绑定真实工具」：tool_search 需要目录指针，
// 而目录的定义清单里又必须先有它自己。绑定时序由 newToolSource 保证。
type lazyTool struct {
	mu    sync.RWMutex
	inner agent.Tool
}

func (l *lazyTool) bind(t agent.Tool) {
	l.mu.Lock()
	l.inner = t
	l.mu.Unlock()
}

func (l *lazyTool) get() agent.Tool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner
}

func (l *lazyTool) Name() string {
	if t := l.get(); t != nil {
		return t.Name()
	}
	return "tool_search"
}

func (l *lazyTool) Description() string {
	if t := l.get(); t != nil {
		return t.Description()
	}
	return "按关键词搜索工具目录，并按需激活工具。"
}

func (l *lazyTool) Parameters() map[string]any {
	if t := l.get(); t != nil {
		return t.Parameters()
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (l *lazyTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	t := l.get()
	if t == nil {
		return agent.Result{IsError: true, Content: "工具目录未就绪，暂时不能搜索工具。"}, nil
	}
	return t.Execute(ctx, raw, ex)
}

// toolsetDesc 是工具集的一句话说明（进提示词/HTTP 展示）。
func toolsetDesc(name string) string {
	switch name {
	case toolsetCore:
		return "基础能力：计算、时间、JSON、网页、SQL、提问"
	case toolsetFS:
		return "工作区文件：读、写、编辑、列目录、移动、删除、Office 编辑"
	case toolsetMemory:
		return "记忆库检索与写入（RAG）"
	case toolsetSkill:
		return "技能库：检索、阅读、创作、执行"
	case toolsetExec:
		return "命令执行（构建、测试、脚本）"
	case toolsetAgent:
		return "子 agent 委派"
	case toolsetTeam:
		return "团队成员与共享任务板"
	case toolsetEvolve:
		return "自我进化：轨迹、技能账本、结构化记忆、固化报告"
	case toolsetMCP:
		return "外部 MCP 服务提供的工具"
	}
	return "未归类工具"
}

// toolsetRisk 是工具集默认风险；单工具另有 RiskAnnotated 时以工具为准。
func toolsetRisk(name string) toolsys.Risk {
	switch name {
	case toolsetExec, toolsetMCP:
		return toolsys.RiskExec
	case toolsetFS:
		return toolsys.RiskWrite
	case toolsetSkill, toolsetEvolve:
		return toolsys.RiskSelf
	case toolsetAgent, toolsetTeam:
		return toolsys.RiskRead
	}
	return toolsys.RiskRead
}

// buildAgentToolSource 按开关装配工具来源。
//
// 返回值约定：
//   - tools：可能被套了落盘包装的工具清单（仍然交给 cfg.Tools，供未启用目录时使用）；
//   - source：非 nil 时循环改为每轮从目录取定义（渐进披露生效），否则循环走原路径。
func buildAgentToolSource(tools []agent.Tool) ([]agent.Tool, agent.ToolSource) {
	return buildAgentToolSourceForUser(tools, "")
}

func buildAgentToolSourceForUser(tools []agent.Tool, userID string) ([]agent.Tool, agent.ToolSource) {
	spillBytes := hermesEnvInt("AGENT_TOOL_SPILL_BYTES")
	dir := hermesDataDir()
	if userID != "" {
		dir = hermesUserDataDir(userID)
	}
	policy := toolsys.SpillPolicy{
		Dir:       filepath.Join(dir, "spill"),
		MaxBytes:  spillBytes,
		HeadRunes: 2000,
	}
	if spillBytes > 0 {
		wrapped := make([]agent.Tool, 0, len(tools))
		for _, t := range tools {
			wrapped = append(wrapped, policy.Wrap(t))
		}
		tools = wrapped
	}
	if toolDisclosureMode() != "search" {
		if spillBytes > 0 {
			// 只开落盘、不开渐进披露：read_tool_result 直接补进工具表。
			tools = append(tools, toolsys.ReadResultTool(policy))
		}
		return tools, nil
	}

	// 渐进披露：按工具集分组，core 常驻（含 tool_search），其余等模型搜索后激活。
	grouped := make(map[string][]agent.Tool, len(toolsetNames()))
	for _, t := range tools {
		set := toolsetOf(t.Name())
		grouped[set] = append(grouped[set], t)
	}
	search := &lazyTool{}
	core := make([]agent.Tool, 0, len(grouped[toolsetCore])+2)
	core = append(core, search)
	core = append(core, grouped[toolsetCore]...)
	if spillBytes > 0 {
		core = append(core, toolsys.ReadResultTool(policy))
	}
	grouped[toolsetCore] = core

	sets := make([]toolsys.Toolset, 0, len(grouped))
	for _, name := range toolsetNames() {
		if len(grouped[name]) == 0 {
			continue
		}
		sets = append(sets, toolsys.Toolset{
			Name: name, Desc: toolsetDesc(name), Risk: toolsetRisk(name), Tools: grouped[name],
		})
	}
	if len(grouped[toolsetMisc]) > 0 {
		sets = append(sets, toolsys.Toolset{
			Name: toolsetMisc, Desc: toolsetDesc(toolsetMisc), Risk: toolsys.RiskRead, Tools: grouped[toolsetMisc],
		})
	}
	catalog := toolsys.NewCatalog(sets...)
	search.bind(toolsys.SearchTool(catalog))
	return tools, catalog
}

// hermesToolCatalogInfo 给 HTTP/调试用的目录视图。
func hermesToolCatalogInfo(source agent.ToolSource) any {
	catalog, ok := source.(*toolsys.Catalog)
	if !ok || catalog == nil {
		return nil
	}
	total, active := catalog.Len()
	return map[string]any{
		"total": total, "active": active,
		"toolsets": catalog.Toolsets(), "selected": selectedToolsets(),
	}
}
