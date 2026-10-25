package web

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
)

// 本文件是「工具集（toolset）」的接线层：把 buildAgentTools 产出的平铺工具表
// 归类成声明式工具集，供 AGENT_TOOLSETS 选择、供 AGENT_TOOL_DISCLOSURE 做渐进披露。
//
// 设计取舍：
//   - 分组**不改工具本身**，只加一层名字→工具集的映射。buildAgentTools 里那套
//     plan/confirm/auto、kind 过滤、子 agent 拒绝等策略保持原样执行，避免把已经
//     被测试覆盖的行为改出偏差。
//   - 未归类的新工具落到 "misc"，永远不被 AGENT_TOOLSETS 过滤掉：宁可多给一个
//     工具，也不要让新功能因为忘了登记就静默消失。
const (
	toolsetCore   = "core"   // 基础能力：不碰磁盘的通用工具
	toolsetFS     = "fs"     // 工作区文件读写
	toolsetMemory = "memory" // 记忆库
	toolsetSkill  = "skill"  // 技能库与自模型
	toolsetExec   = "exec"   // 命令执行
	toolsetAgent  = "agent"  // 子 agent 委派
	toolsetTeam   = "team"   // 团队成员与共享任务板
	toolsetEvolve = "evolve" // 自我进化：轨迹/账本/结构化记忆/固化
	toolsetMCP     = "mcp"     // 显式配置的外部工具服务
	toolsetBrowser = "browser" // 浏览器（静态快照 / 本机页面）
	toolsetMisc    = "misc"    // 未归类
)

// toolsetOf 把一个工具名归到工具集。规则集中在这里，新增工具时只改一处。
func toolsetOf(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return toolsetMCP
	}
	switch name {
	case "calculator", "datetime", "json_format", "ask_user", "web_search", "web_reader", "sql_query", "update_plan":
		return toolsetCore
	case "read", "write", "edit", "read_image", "list", "delete", "move", "office_edit", "grep", "glob", "restore", "restore_turn":
		return toolsetFS
	case "memory_search", "memory_write", "session_search":
		return toolsetMemory
	case "skill_search", "skill_read", "skill_write", "skill_run", "soul_evolve",
		"skill_list", "skill_stats", "skill_lint", "skill_improve", "skill_archive":
		return toolsetSkill
	case "run_command", "git_status", "git_diff", "git_log", "git_show", "git_add", "git_commit", "init_agents_md":
		return toolsetExec
	case "browser_open", "browser_snapshot", "browser_screenshot", "browser_network", "browser_click", "browser_type":
		return toolsetBrowser
	case "subagent", "list_subagents", "schedule_wakeup", "list_wakeups", "cancel_wakeup":
		return toolsetAgent
	case "spawn_teammate", "send_message", "list_agents", "wait_agent",
		"team_task_create", "team_task_list", "team_task_get", "team_task_update":
		return toolsetTeam
	case "memory_note", "memory_list", "memory_forget", "evolution_report", "tool_search", "read_tool_result":
		return toolsetEvolve
	}
	return toolsetMisc
}

// toolsetNames 返回所有已知工具集名（稳定顺序，供文档与 HTTP 展示）。
func toolsetNames() []string {
	return []string{toolsetCore, toolsetFS, toolsetMemory, toolsetSkill, toolsetExec, toolsetAgent, toolsetTeam, toolsetEvolve, toolsetMCP, toolsetBrowser}
}

// parseToolsetList 解析 AGENT_TOOLSETS：逗号/空格分隔，大小写不敏感，去重。
// 返回 nil 表示「没配置」，调用方据此走原路径（全量工具）。
func parseToolsetList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// filterToolsByToolsets 按选择保留工具。
//
// core 与 misc 总是保留：
//   - core 是 ask_user/calculator 这类不碰磁盘的基础能力，砍掉会让模型没法提问；
//   - misc 是尚未登记的新工具，按「宁多勿少」保留，避免新功能静默消失。
func filterToolsByToolsets(tools []agent.Tool, want []string) []agent.Tool {
	if len(want) == 0 {
		return tools
	}
	keep := make(map[string]bool, len(want)+2)
	for _, name := range want {
		keep[name] = true
	}
	out := make([]agent.Tool, 0, len(tools))
	for _, t := range tools {
		set := toolsetOf(t.Name())
		if keep[set] || set == toolsetCore || set == toolsetMisc {
			out = append(out, t)
		}
	}
	return out
}

// selectedToolsets 返回本次运行实际生效的工具集名（已按 AGENT_TOOLSETS 过滤）。
// 没配置时返回全部已知工具集，仅用于展示。
func selectedToolsets() []string {
	want := parseToolsetList(os.Getenv("AGENT_TOOLSETS"))
	if len(want) == 0 {
		return toolsetNames()
	}
	keep := make(map[string]bool, len(want))
	for _, name := range want {
		keep[name] = true
	}
	out := make([]string, 0, len(toolsetNames()))
	for _, name := range toolsetNames() {
		if keep[name] || name == toolsetCore || name == toolsetMisc {
			out = append(out, name)
		}
	}
	return out
}

// hermesEnvString 读字符串型开关，未设置时返回 ""（= 走原路径）。
func hermesEnvString(name string) string {
	return strings.ToLower(strings.TrimSpace(os.Getenv(name)))
}

// hermesEnvInt 读整数型开关，非法值按 0 处理（0 的语义由调用方定义）。
func hermesEnvInt(name string) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// toolDisclosureMode：off（默认，全量工具）| search（核心集 + tool_search 渐进披露）。
func toolDisclosureMode() string {
	switch mode := hermesEnvString("AGENT_TOOL_DISCLOSURE"); mode {
	case "search", "on", "1", "true":
		return "search"
	default:
		return "off"
	}
}

// parallelMode：off（默认串行）| read（只读工具并发）。
func parallelMode() string {
	if hermesEnvString("AGENT_TOOL_PARALLEL") == "read" {
		return "read"
	}
	return "off"
}

// parallelEffective 返回**实际生效**的并发档位。
//
// 两个执行器都支持只读并发：
//   - legacy：planCallGroups 把连续只读调用切批，按输入顺序回放事件；
//   - eino（默认）：工具节点起 goroutine，callGated 的读写锁保证「只读共享、写独占」。
//
// 保留这个函数是为了让状态接口区分「配置值」与「生效值」；当前两者一致，
// 但不要再把引擎判断写死在这里——上一版就是因为写死 eino=off 而与实现脱节。
func parallelEffective() string {
	if parallelMode() != "read" {
		return "off"
	}
	return "read"
}

// evolveMode：off | record | suggest | auto（默认 off）。
func evolveMode() string {
	switch mode := hermesEnvString("AGENT_EVOLVE"); mode {
	case "record", "suggest", "auto":
		return mode
	case "on", "1", "true":
		return "record"
	default:
		return "off"
	}
}

// skillLintMode：off | warn（默认） | block。
func skillLintMode() string {
	switch mode := hermesEnvString("AGENT_SKILL_LINT"); mode {
	case "off", "block":
		return mode
	default:
		return "warn"
	}
}

// sortedToolNames 返回工具名清单（稳定排序），用于对照测试与日志。
func sortedToolNames(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	return names
}

// readOnlyToolNames 是允许并发执行的工具白名单（AGENT_TOOL_PARALLEL=read）。
//
// 名单来自 toolsys.LookupMeta 的 ParallelOK，这里不再手写第二份。
// 判据是「没有副作用、重复执行安全」，不是「看起来像读」：
//   - tool_search 会改目录状态（激活工具），不并发；
//   - memory_note 会写记忆文件，不并发（哪怕它内部有锁，顺序语义也该保留）；
//   - evolution_report 会落盘报告，不并发；
//   - subagent / spawn_teammate / send_message 会启动新轮次，不并发；
//   - skill_run / run_command / write 类一律不进；
//   - ask_user 只读但会停轮，不并发；
//   - web_search / web_reader 是网络风险，但重复执行安全，可以并发；
//   - read_image 读的是磁盘，安全。
//
// 白名单而不是黑名单：表外名字 ParallelOK 为 false，漏登记只是慢一点，不会出错。
func readOnlyToolNames() map[string]bool {
	names := toolsys.KnownToolNames()
	out := make(map[string]bool, len(names))
	for _, name := range names {
		if toolsys.LookupMeta(name).ParallelOK {
			out[name] = true
		}
	}
	return out
}
