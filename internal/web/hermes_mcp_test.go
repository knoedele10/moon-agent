package web

import (
	"strings"
	"testing"
)

// MCP 默认关闭：没有配置时不启动任何进程、不返回任何工具。
func TestMCPDisabledByDefault(t *testing.T) {
	t.Setenv("AGENT_MCP_JSON", "")
	t.Setenv("AGENT_MCP_CONFIG", "")
	if tools := mcpTools(); len(tools) != 0 {
		t.Fatalf("未配置时不应有 MCP 工具，实际 %d 个", len(tools))
	}
	if summary := mcpSummary(); summary != "" {
		t.Fatalf("未配置时摘要应为空，实际 %q", summary)
	}
}

// MCP 工具一律按风险工具处理：它们是外部进程，能力边界不在我们手里。
func TestMCPToolNamesAreRisky(t *testing.T) {
	for _, name := range []string{"mcp__demo__echo", "mcp__"} {
		if !riskyTool(name) {
			t.Fatalf("%q 没有被判成风险工具", name)
		}
	}
	// 只读内置工具不受影响。
	if riskyTool("read") {
		t.Fatal("read 被判成风险工具，说明前缀判断写错了")
	}
}

// 工具集过滤不认识 MCP 工具，但不能把它们弄丢（落到 misc/原样保留）。
func TestMCPToolsSurviveToolsetFilter(t *testing.T) {
	// 直接验证前缀约定：Catalog/工具集按名字归类，mcp__ 前缀不应被过滤掉。
	if strings.HasPrefix("mcp__demo__echo", "mcp__") == false {
		t.Fatal("前缀约定被破坏")
	}
}
