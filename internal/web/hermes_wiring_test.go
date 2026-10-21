package web

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
	"blog-my/internal/llm"
)

// 接线单测（契约 §7 第 1/2/3 条）：证明「所有 AGENT_* 开关关闭时行为与改造前一致」，
// 以及每个开关打开时只影响它自己那一条路径。这里不连数据库、不连模型。

// hermesWiringTool 是最小 agent.Tool 桩：只关心名字与是否被包装。
type hermesWiringTool struct{ name string }

func (t hermesWiringTool) Name() string        { return t.name }
func (t hermesWiringTool) Description() string { return "wiring test tool" }
func (t hermesWiringTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t hermesWiringTool) Execute(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error) {
	return agent.Result{Content: t.name + " ok"}, nil
}

// hermesClearEnv 把本轮新增的开关全部清空，并把落盘/进化目录指到临时目录，
// 避免测试写进真实的用户配置目录。
func hermesClearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AGENT_TOOLSETS", "AGENT_TOOL_DISCLOSURE", "AGENT_TOOL_PARALLEL",
		"AGENT_TOOL_PARALLEL_MAX", "AGENT_TOOL_SPILL_BYTES",
		"AGENT_EVOLVE", "AGENT_EVOLVE_DIR", "AGENT_SKILL_LINT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("AGENT_EVOLVE_DIR", t.TempDir())
}

func hermesWiringTools() []agent.Tool {
	return []agent.Tool{
		hermesWiringTool{"read"}, hermesWiringTool{"write"}, hermesWiringTool{"calculator"},
		hermesWiringTool{"memory_search"}, hermesWiringTool{"run_command"},
	}
}

func toolDefNames(defs []llm.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

// 全关：工具清单原样返回、不套落盘包装、不建目录（source 必须为 nil，循环走老路）。
func TestHermesWiringAllOffIsUnchanged(t *testing.T) {
	hermesClearEnv(t)
	tools := hermesWiringTools()

	got, source := buildAgentToolSource(tools)
	if source != nil {
		t.Fatal("AGENT_* 全关时不该装配工具目录：循环必须走原来的静态注册表路径")
	}
	if len(got) != len(tools) {
		t.Fatalf("工具数量被改动：%d → %d", len(tools), len(got))
	}
	for i := range tools {
		if got[i] != tools[i] {
			t.Fatalf("第 %d 个工具被意外包装（应为同一个对象）", i)
		}
	}
	if !equalStrings(sortedToolNames(got), sortedToolNames(tools)) {
		t.Fatalf("工具名清单变了：%v → %v", sortedToolNames(tools), sortedToolNames(got))
	}
	// 默认档位与默认开关值。
	if toolDisclosureMode() != "off" || parallelMode() != "off" || evolveMode() != "off" {
		t.Fatalf("默认开关值不对：disclosure=%s parallel=%s evolve=%s",
			toolDisclosureMode(), parallelMode(), evolveMode())
	}
	if skillLintMode() != "warn" {
		t.Fatalf("技能体检默认应是 warn，实际 %s", skillLintMode())
	}
}

// AGENT_TOOLSETS 为空时过滤是恒等变换；只选 fs 时核心工具集必须常驻。
func TestFilterToolsByToolsets(t *testing.T) {
	hermesClearEnv(t)
	tools := hermesWiringTools()

	if got := filterToolsByToolsets(tools, nil); !equalStrings(sortedToolNames(got), sortedToolNames(tools)) {
		t.Fatalf("空选择集必须原样返回：%v", sortedToolNames(got))
	}

	got := filterToolsByToolsets(tools, []string{"fs"})
	names := sortedToolNames(got)
	for _, want := range []string{"read", "write", "calculator"} {
		if !containsString(names, want) {
			t.Fatalf("选 fs 后 %q 应保留（fs 工具或核心集）：%v", want, names)
		}
	}
	if containsString(names, "run_command") || containsString(names, "memory_search") {
		t.Fatalf("未选中的工具集不该出现：%v", names)
	}
}

// AGENT_TOOL_DISCLOSURE=search：模型只看到核心集 + tool_search，其余工具要搜到才激活。
func TestHermesWiringSearchDisclosesLazily(t *testing.T) {
	hermesClearEnv(t)
	t.Setenv("AGENT_TOOL_DISCLOSURE", "search")

	_, source := buildAgentToolSource(hermesWiringTools())
	if source == nil {
		t.Fatal("disclosure=search 必须装配工具目录")
	}
	catalog, ok := source.(*toolsys.Catalog)
	if !ok {
		t.Fatalf("工具来源类型不对：%T", source)
	}

	defs := toolDefNames(catalog.Defs())
	if !containsString(defs, "tool_search") {
		t.Fatalf("核心集里必须有 tool_search：%v", defs)
	}
	if !containsString(defs, "calculator") {
		t.Fatalf("核心集必须常驻：%v", defs)
	}
	if containsString(defs, "write") {
		t.Fatalf("未激活的写工具不该出现在定义里：%v", defs)
	}

	// 搜索并激活后，下一轮定义里就能看到它（与 loop 每轮重取 Defs 的语义配套）。
	if matches := catalog.Search("write", 5); len(matches) == 0 {
		t.Fatal("目录里应该搜得到 write")
	}
	catalog.Activate("write")
	if defs2 := toolDefNames(catalog.Defs()); !containsString(defs2, "write") {
		t.Fatalf("激活后 write 应进入定义：%v", defs2)
	}
}

// 落盘开关只加 read_tool_result，不改变原有工具与循环路径（source 仍为 nil）。
func TestHermesWiringSpillOnly(t *testing.T) {
	hermesClearEnv(t)
	t.Setenv("AGENT_TOOL_SPILL_BYTES", "1024")

	tools := hermesWiringTools()
	got, source := buildAgentToolSource(tools)
	if source != nil {
		t.Fatal("只开落盘不该装目录")
	}
	names := sortedToolNames(got)
	if !containsString(names, "read_tool_result") {
		t.Fatalf("开了落盘就要有 read_tool_result：%v", names)
	}
	for _, want := range []string{"read", "write", "calculator"} {
		if !containsString(names, want) {
			t.Fatalf("原有工具不该丢：%v", names)
		}
	}
}

// 进化默认关闭：没有工具、没有提示词、不会因为构造对象去碰数据库。
func TestHermesEvolveOffByDefault(t *testing.T) {
	hermesClearEnv(t)
	evo := newHermesEvolve("1")
	if evo.Enabled() {
		t.Fatal("AGENT_EVOLVE 未设置时不该启用")
	}
	if tools := evo.Tools(); len(tools) != 0 {
		t.Fatalf("未启用时不该追加进化工具：%d 个", len(tools))
	}
	if block := evo.PromptBlock(); strings.TrimSpace(block) != "" {
		t.Fatalf("未启用时不该注入提示词：%q", block)
	}
	if _, found := evo.LastReport(); found {
		t.Fatal("未启用时不该有固化报告")
	}
}

// 只读并发白名单不能混进任何会改世界的工具：并发只在「重复执行完全安全」时开启。
func TestReadOnlyToolNamesExcludeMutating(t *testing.T) {
	hermesClearEnv(t)
	allow := readOnlyToolNames()
	for _, name := range []string{
		"write", "edit", "delete", "move", "office_edit",
		"run_command", "skill_write", "skill_run", "memory_write", "memory_note",
		"spawn_teammate", "send_message", "tool_search", "evolution_report", "ask_user",
	} {
		if allow[name] {
			t.Fatalf("%q 有副作用或会中断轮次，不能进只读并发白名单", name)
		}
	}
	for _, name := range []string{"read", "list", "web_search", "read_tool_result", "team_task_list"} {
		if !allow[name] {
			t.Fatalf("%q 是纯只读工具，应在白名单里", name)
		}
	}
}

// 并行开关只在 read 档打开，且并发上限可配；默认关闭。
func TestParallelModeParsing(t *testing.T) {
	hermesClearEnv(t)
	if parallelMode() != "off" {
		t.Fatalf("默认应关闭，实际 %s", parallelMode())
	}
	t.Setenv("AGENT_TOOL_PARALLEL", "read")
	if parallelMode() != "read" {
		t.Fatalf("read 档没被识别：%s", parallelMode())
	}
	t.Setenv("AGENT_TOOL_PARALLEL", "yes")
	if parallelMode() != "off" {
		t.Fatalf("非法值必须按关闭处理，实际 %s", parallelMode())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
