package web

import (
	"testing"

	"blog-my/internal/agent/toolsys"
)

// web 的并发白名单和确认判断必须从 toolsys.LookupMeta 派生。
// 安全层在 agent 里对照同一张表（agent 不能 import toolsys）；三边因此对齐。
func TestWebHelpersAgreeWithToolMeta(t *testing.T) {
	allow := readOnlyToolNames()
	seen := map[string]bool{}
	for _, name := range toolsys.KnownToolNames() {
		seen[name] = true
		meta := toolsys.LookupMeta(name)
		if allow[name] != meta.ParallelOK {
			t.Errorf("%s 并发白名单=%v，元数据 ParallelOK=%v", name, allow[name], meta.ParallelOK)
		}
		if riskyTool(name) != meta.Confirm {
			t.Errorf("%s riskyTool=%v，元数据 Confirm=%v", name, riskyTool(name), meta.Confirm)
		}
	}
	for name := range allow {
		if !seen[name] {
			t.Errorf("并发白名单有表外名字 %s", name)
		}
	}
}

func TestWebUnknownToolIsConservative(t *testing.T) {
	allow := readOnlyToolNames()
	cases := []struct {
		name     string
		confirm  bool
		parallel bool
	}{
		{name: "not_a_registered_tool", confirm: true, parallel: false},
		{name: "shell", confirm: true, parallel: false},
		{name: "mcp__demo__echo", confirm: true, parallel: false},
		{name: "mcp__", confirm: true, parallel: false},
		{name: "read", confirm: false, parallel: true},
		{name: "memory_write", confirm: false, parallel: false},
		{name: "web_search", confirm: false, parallel: true},
	}
	for _, tc := range cases {
		meta := toolsys.LookupMeta(tc.name)
		if riskyTool(tc.name) != tc.confirm || meta.Confirm != tc.confirm {
			t.Errorf("%s confirm：helper=%v meta=%v want=%v", tc.name, riskyTool(tc.name), meta.Confirm, tc.confirm)
		}
		if allow[tc.name] != tc.parallel || meta.ParallelOK != tc.parallel {
			t.Errorf("%s parallel：helper=%v meta=%v want=%v", tc.name, allow[tc.name], meta.ParallelOK, tc.parallel)
		}
	}
	web := toolsys.LookupMeta("web_search")
	if web.Risk != toolsys.RiskNetwork || web.SideEffect != toolsys.SideEffectReadOnly {
		t.Fatalf("web_search 应是可并发的网络只读，实际 %+v", web)
	}
	mem := toolsys.LookupMeta("memory_write")
	if mem.SideEffect != toolsys.SideEffectIdempotent || mem.Mutating {
		t.Fatalf("memory_write 应保持幂等且不计入完成义务，实际 %+v", mem)
	}
}
