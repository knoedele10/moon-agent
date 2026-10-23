package toolsys

import (
	"testing"

	"blog-my/internal/agent/toolsys/policy"
)

func TestLookupMetaMatchesPolicyPackage(t *testing.T) {
	names := append(KnownToolNames(), "not_a_registered_tool", "mcp__demo__echo", "mcp__", "Read", " read ")
	for _, name := range names {
		if LookupMeta(name) != policy.LookupMeta(name) {
			t.Fatalf("toolsys.LookupMeta(%q) 与 policy 不一致", name)
		}
	}
}

func TestLookupMetaUnknownIsConservative(t *testing.T) {
	for _, name := range []string{"", "not_a_registered_tool", "Read", " read ", "MCP__demo"} {
		m := LookupMeta(name)
		if m.Known || m.ParallelOK || !m.Confirm || m.Mutating || m.Verifying {
			t.Fatalf("%q 应是未知保守默认，实际 %+v", name, m)
		}
		if m.Risk != RiskWrite || m.SideEffect != SideEffectNonIdempotent {
			t.Fatalf("%q 风险/副作用应保守，实际 %+v", name, m)
		}
	}
}

func TestLookupMetaMCPPrefix(t *testing.T) {
	for _, name := range []string{"mcp__", "mcp__demo__echo"} {
		m := LookupMeta(name)
		if !m.Known || m.ParallelOK || !m.Confirm || m.Mutating || m.Verifying {
			t.Fatalf("%q 应按外部进程处理，实际 %+v", name, m)
		}
		if m.Risk != RiskExec || m.SideEffect != SideEffectNonIdempotent {
			t.Fatalf("%q 风险/副作用不符，实际 %+v", name, m)
		}
	}
}

// 这些集合钉住「已知工具与今天硬编码名单一致」。
// 只读并发比旧 safety 只读名单多出来的名字，副作用也是 read_only（两边合成一张表）。
func TestLookupMetaCompatSets(t *testing.T) {
	wantParallel := map[string]bool{
		"read": true, "list": true, "read_image": true,
		"calculator": true, "datetime": true, "json_format": true, "web_search": true, "web_reader": true, "sql_query": true,
		"memory_search": true, "memory_list": true, "session_search": true,
		"skill_search": true, "skill_read": true, "skill_list": true, "skill_stats": true, "skill_lint": true,
		"read_tool_result": true,
		"list_agents":      true, "list_subagents": true, "team_task_list": true, "team_task_get": true,
	}
	wantConfirm := map[string]bool{
		"write": true, "edit": true, "move": true, "delete": true, "office_edit": true,
		"skill_run": true, "run_command": true,
		"skill_write": true, "skill_improve": true, "skill_archive": true, "memory_forget": true,
	}
	wantIdempotent := map[string]bool{
		"write": true, "edit": true, "memory_write": true, "skill_write": true,
	}
	wantMutating := map[string]bool{
		"write": true, "edit": true, "move": true, "delete": true,
		"office_edit": true, "skill_run": true, "skill_write": true, "run_command": true,
	}
	wantVerifying := map[string]bool{
		"read": true, "list": true, "read_image": true, "sql_query": true,
	}

	gotParallel := map[string]bool{}
	gotConfirm := map[string]bool{}
	gotIdempotent := map[string]bool{}
	gotMutating := map[string]bool{}
	gotVerifying := map[string]bool{}
	for _, name := range KnownToolNames() {
		m := LookupMeta(name)
		if !m.Known || !m.Risk.Valid() {
			t.Fatalf("%s 登记不完整：%+v", name, m)
		}
		if m.ParallelOK && (m.SideEffect != SideEffectReadOnly || m.Confirm) {
			t.Fatalf("%s 可并发就必须只读且不需确认", name)
		}
		if m.Verifying && (m.SideEffect != SideEffectReadOnly || !m.ParallelOK) {
			t.Fatalf("%s 充当验证就必须是可并发的只读", name)
		}
		if m.Mutating && (m.ParallelOK || m.SideEffect == SideEffectReadOnly) {
			t.Fatalf("%s 会改世界就不能只读并发", name)
		}
		if m.ParallelOK {
			gotParallel[name] = true
		}
		if m.Confirm {
			gotConfirm[name] = true
		}
		if m.SideEffect == SideEffectIdempotent {
			gotIdempotent[name] = true
		}
		if m.Mutating {
			gotMutating[name] = true
		}
		if m.Verifying {
			gotVerifying[name] = true
		}
	}
	assertSet(t, "parallel", gotParallel, wantParallel)
	assertSet(t, "confirm", gotConfirm, wantConfirm)
	assertSet(t, "idempotent", gotIdempotent, wantIdempotent)
	assertSet(t, "mutating", gotMutating, wantMutating)
	assertSet(t, "verifying", gotVerifying, wantVerifying)

	// ask_user 只读但不并发；web_search 出网但可并发；memory_write 不问确认。
	ask := LookupMeta("ask_user")
	if ask.SideEffect != SideEffectReadOnly || ask.ParallelOK || ask.Confirm {
		t.Fatalf("ask_user 应只读、不并发、不确认，实际 %+v", ask)
	}
	web := LookupMeta("web_search")
	if web.Risk != RiskNetwork || !web.ParallelOK || web.Confirm || web.SideEffect != SideEffectReadOnly {
		t.Fatalf("web_search 应是可并发的网络只读，实际 %+v", web)
	}
	mem := LookupMeta("memory_write")
	if mem.Confirm || mem.ParallelOK || mem.Mutating || mem.SideEffect != SideEffectIdempotent {
		t.Fatalf("memory_write 应保持不问确认的幂等写入，实际 %+v", mem)
	}
	if LookupMeta("web_reader").Risk != RiskNetwork || !LookupMeta("web_reader").ParallelOK {
		t.Fatal("web_reader 应与 web_search 一样：网络风险且可并发")
	}
}

func assertSet(t *testing.T, label string, got, want map[string]bool) {
	t.Helper()
	for name := range want {
		if !got[name] {
			t.Errorf("%s 缺少 %s", label, name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s 多了 %s", label, name)
		}
	}
}
