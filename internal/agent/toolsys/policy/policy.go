// Package policy 是工具权限/风险的唯一登记表。
//
// 它放在 toolsys 下面、但不 import agent 或 toolsys：agent 已经被 toolsys 依赖，
// 安全层若再 import toolsys 会成环。因此表放在这个叶子包里，toolsys.LookupMeta
// 原样转出，web 与 agent/safety 都从这里派生，不再各维护一份名单。
package policy

import (
	"sort"
	"strings"
)

// Risk 是工具风险等级。toolsys.Plan 只把 RiskRead 并成只读批。
// 是否要用户确认、能否进遗留循环的只读并发，看 Confirm / ParallelOK，不从 Risk 推导。
type Risk string

const (
	// RiskRead 不改外部状态。
	RiskRead Risk = "read"
	// RiskWrite 写用户文件。
	RiskWrite Risk = "write"
	// RiskExec 执行命令、代码，或拉起另一段会执行工具的轮次。
	RiskExec Risk = "exec"
	// RiskNetwork 出网。可以同时 ParallelOK（例如 web_search）。
	RiskNetwork Risk = "network"
	// RiskSelf 改变自身行为、记忆、人格或协作状态。
	RiskSelf Risk = "self"
)

// Valid 报告风险等级是否是约定的五档之一。
func (r Risk) Valid() bool {
	switch r {
	case RiskRead, RiskWrite, RiskExec, RiskNetwork, RiskSelf:
		return true
	}
	return false
}

// SideEffect 是重试/结果未知判断用的副作用语义。
// 字符串与 agent.SideEffectKind 保持一致，安全层只做薄转换。
type SideEffect string

const (
	SideEffectReadOnly      SideEffect = "read_only"
	SideEffectIdempotent    SideEffect = "idempotent_mutation"
	SideEffectNonIdempotent SideEffect = "non_idempotent_side_effect"
)

// Meta 是一个工具名的权限事实。
type Meta struct {
	Risk       Risk
	ParallelOK bool // 遗留循环 AGENT_TOOL_PARALLEL=read 的白名单
	Confirm    bool // confirm 档先问用户；plan 档不给
	SideEffect SideEffect
	Mutating   bool // 成功后值得读回来验证
	Verifying  bool // 能充当「写完之后的验证」
	// Known 为 false 表示名字不在表里，也不是 mcp__ 前缀；字段是保守默认。
	Known bool
}

// LookupMeta 按工具名取权限。名字大小写敏感、不裁剪空白，与旧名单的精确匹配一致。
//
// 未登记的名字按失败安全默认：不是只读、不能并行、需要确认、非幂等。
// Mutating/Verifying 保持 false：未知工具不能冒充验证，也不额外制造完成义务。
// mcp__ 前缀一律按外部进程：确认、不并行、非幂等、风险 exec。
func LookupMeta(name string) Meta {
	if m, ok := byName[name]; ok {
		return m
	}
	if strings.HasPrefix(name, "mcp__") {
		return mcpMeta
	}
	return unknownMeta
}

// KnownToolNames 返回表里的工具名（稳定排序）。不含 mcp__ 前缀类。
func KnownToolNames() []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// known(risk, parallelOK, confirm, sideEffect, mutating, verifying)
func known(risk Risk, parallel, confirm bool, effect SideEffect, mutating, verifying bool) Meta {
	return Meta{
		Risk:       risk,
		ParallelOK: parallel,
		Confirm:    confirm,
		SideEffect: effect,
		Mutating:   mutating,
		Verifying:  verifying,
		Known:      true,
	}
}

// unknownMeta 是表外名字的保守默认。
var unknownMeta = Meta{
	Risk:       RiskWrite,
	ParallelOK: false,
	Confirm:    true,
	SideEffect: SideEffectNonIdempotent,
	Mutating:   false,
	Verifying:  false,
	Known:      false,
}

// mcpMeta 对应外部 MCP 工具。能力边界不在本进程，确认档一律先问。
// 不进 mutatingTools：旧完成义务名单没有它们，验证义务保持原样。
var mcpMeta = Meta{
	Risk:       RiskExec,
	ParallelOK: false,
	Confirm:    true,
	SideEffect: SideEffectNonIdempotent,
	Mutating:   false,
	Verifying:  false,
	Known:      true,
}

// byName 是内置工具的唯一登记。
//
// 只读并发白名单与安全层只读名单曾经不一致（web 多了 memory_list、session_search、
// skill_list/stats/lint、read_tool_result、list_agents、team_task_list/get）。
// 那些名字在 web 注释里被判成「重复执行安全」，这里统一成 read_only，因此未知错误允许重试一次。
//
// 刻意保留的产品行为：
//   - memory_write：幂等写入记忆，不改用户文件，confirm 档不问（2026-10-12）。也不算 mutating。
//   - ask_user：只读，但会停轮，ParallelOK=false。
//   - web_search / web_reader：RiskNetwork，同时 ParallelOK。
//   - skill_improve / skill_archive / memory_forget：要确认，但旧 mutating 名单没有它们。
//   - soul_evolve 会改人格，旧 riskyTool 却没列入；保持不问，避免这次顺手改产品。
//   - subagent / 团队写操作：旧 riskyTool 不问（委派与协作不走文件确认）。
var byName = map[string]Meta{
	// 通用计算与提问。
	"calculator":  known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"datetime":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"json_format": known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"ask_user":    known(RiskRead, false, false, SideEffectReadOnly, false, false),
	// 计划只存在于进程内，不动用户文件、不出网；整体替换是幂等的。
	"update_plan": known(RiskSelf, false, false, SideEffectIdempotent, false, false),
	"web_search":  known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"web_reader":  known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"sql_query":   known(RiskRead, true, false, SideEffectReadOnly, false, true),

	// 工作区文件。读类风险是 read，不再沿用 fs 组的整组 write。
	"read":       known(RiskRead, true, false, SideEffectReadOnly, false, true),
	"list":       known(RiskRead, true, false, SideEffectReadOnly, false, true),
	"read_image": known(RiskRead, true, false, SideEffectReadOnly, false, true),
	// 检索只读、可并发；但它不冒充「写完之后的验证」：
	// 搜到某段文字存在，不等于目标文件整体正确。
	"grep":   known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"glob":   known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"write":  known(RiskWrite, false, true, SideEffectIdempotent, true, false),
	"edit":   known(RiskWrite, false, true, SideEffectIdempotent, true, false),
	"move":   known(RiskWrite, false, true, SideEffectNonIdempotent, true, false),
	"delete": known(RiskWrite, false, true, SideEffectNonIdempotent, true, false),
	// 恢复到同一检查点两次结果相同；但它确实改世界，confirm 档先问、成功后值得读回。
	"restore":      known(RiskWrite, false, true, SideEffectIdempotent, true, false),
	"restore_turn": known(RiskWrite, false, true, SideEffectIdempotent, true, false),
	"office_edit":  known(RiskWrite, false, true, SideEffectNonIdempotent, true, false),

	// 记忆。memory_write 见文件头的产品说明。
	"memory_search":  known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"memory_write":   known(RiskSelf, false, false, SideEffectIdempotent, false, false),
	"session_search": known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"memory_note":    known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),
	"memory_list":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"memory_forget":  known(RiskSelf, false, true, SideEffectNonIdempotent, false, false),

	// 技能与人格。只读检索从技能组的整组 self 里拆出来。
	"skill_search":  known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"skill_read":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"skill_list":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"skill_stats":   known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"skill_lint":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"skill_write":   known(RiskSelf, false, true, SideEffectIdempotent, true, false),
	"skill_run":     known(RiskExec, false, true, SideEffectNonIdempotent, true, false),
	"skill_improve": known(RiskSelf, false, true, SideEffectNonIdempotent, false, false),
	"skill_archive": known(RiskSelf, false, true, SideEffectNonIdempotent, false, false),
	"soul_evolve":   known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),

	"run_command": known(RiskExec, false, true, SideEffectNonIdempotent, true, false),
	"git_status":  known(RiskRead, true, false, SideEffectReadOnly, false, true),
	"git_diff":    known(RiskRead, true, false, SideEffectReadOnly, false, true),
	"git_log":     known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"git_show":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"git_add":     known(RiskWrite, false, true, SideEffectIdempotent, true, false),
	"git_commit":  known(RiskWrite, false, true, SideEffectNonIdempotent, true, false),

	// 子任务。list 只读可重试；派活重复会再起一个任务。
	"subagent":       known(RiskExec, false, false, SideEffectNonIdempotent, false, false),
	"list_subagents": known(RiskRead, true, false, SideEffectReadOnly, false, false),

	// 团队。列表/详情可并发；发消息、改任务板、等人都不能并发，但旧确认名单不问。
	"spawn_teammate":   known(RiskExec, false, false, SideEffectNonIdempotent, false, false),
	"send_message":     known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),
	"list_agents":      known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"wait_agent":       known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),
	"team_task_create": known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),
	"team_task_list":   known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"team_task_get":    known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"team_task_update": known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),

	// 目录与落盘结果。tool_search 会激活工具，不能并发。
	"tool_search":      known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),
	"read_tool_result": known(RiskRead, true, false, SideEffectReadOnly, false, false),
	"evolution_report": known(RiskSelf, false, false, SideEffectNonIdempotent, false, false),

	// 定时唤醒：设定一次未来的自主轮次属于「改自身行为」，confirm 档先问一句；
	// 取消与列表不动别人，不问。重复设定会排两条，所以不是幂等写。
	"schedule_wakeup": known(RiskSelf, false, true, SideEffectNonIdempotent, false, false),
	"cancel_wakeup":   known(RiskSelf, false, false, SideEffectIdempotent, false, false),
	"list_wakeups":    known(RiskRead, true, false, SideEffectReadOnly, false, false),

	// 生成 AGENTS.md 会写工作区文件；文件已存在时拒绝覆盖，重复调用结果相同。
	"init_agents_md": known(RiskWrite, false, true, SideEffectIdempotent, true, false),

	// 浏览器：打开/快照/截图/网络日志只读可并发；点击和输入会改变页面，confirm 档先问。
	"browser_open":       known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"browser_snapshot":   known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"browser_screenshot": known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"browser_network":    known(RiskNetwork, true, false, SideEffectReadOnly, false, false),
	"browser_click":      known(RiskNetwork, false, true, SideEffectNonIdempotent, false, false),
	"browser_type":       known(RiskNetwork, false, true, SideEffectNonIdempotent, false, false),
}
