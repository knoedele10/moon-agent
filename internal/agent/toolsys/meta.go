package toolsys

import "blog-my/internal/agent/toolsys/policy"

// 权限事实的类型与查找都定义在 policy（叶子包）。
// 这里只转出，调用方继续写 toolsys.LookupMeta / toolsys.Risk，不必知道分包。
//
// agent 不能 import toolsys（toolsys 已经 import agent），所以安全层直接用 policy。
// 两边看到的是同一张表。

// Risk 是工具风险等级。
type Risk = policy.Risk

const (
	RiskRead    = policy.RiskRead
	RiskWrite   = policy.RiskWrite
	RiskExec    = policy.RiskExec
	RiskNetwork = policy.RiskNetwork
	RiskSelf    = policy.RiskSelf
)

// Meta 是一个工具名的权限事实。
type Meta = policy.Meta

// SideEffect 是重试用的副作用语义。
type SideEffect = policy.SideEffect

const (
	SideEffectReadOnly      = policy.SideEffectReadOnly
	SideEffectIdempotent    = policy.SideEffectIdempotent
	SideEffectNonIdempotent = policy.SideEffectNonIdempotent
)

// LookupMeta 按名字取权限。未知名字保守，mcp__ 前缀按外部进程。
func LookupMeta(name string) Meta { return policy.LookupMeta(name) }

// KnownToolNames 返回已登记的内置工具名（稳定排序）。
func KnownToolNames() []string { return policy.KnownToolNames() }
