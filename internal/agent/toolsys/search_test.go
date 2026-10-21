package toolsys

import (
	"fmt"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// newSearchCatalog 构造一个含 fs/net 两组、全部未激活的目录。
func newSearchCatalog() *Catalog {
	return NewCatalog(
		Toolset{Name: "fs", Desc: "文件", Risk: RiskRead, Tools: []agent.Tool{
			&fakeTool{name: "read", desc: "读取文件内容", tags: []string{"查看"}, risk: RiskRead},
			&fakeTool{name: "list", desc: "列出目录里的文件", risk: RiskRead},
		}},
		Toolset{Name: "net", Desc: "网络", Risk: RiskNetwork, Tools: []agent.Tool{
			&fakeTool{name: "web_search", desc: "联网搜索网页", risk: RiskNetwork},
		}},
	)
}

// TestSearchToolFindsAndActivates 对应契约 §7.3：tool_search 激活后下一轮 Defs 出现新工具。
func TestSearchToolFindsAndActivates(t *testing.T) {
	c := newSearchCatalog()
	tool := SearchTool(c)
	if tool.Name() != "tool_search" {
		t.Fatalf("工具名=%q，期望 tool_search", tool.Name())
	}

	res := runTool(t, tool, map[string]any{"query": "文件"})
	if res.IsError {
		t.Fatalf("搜索失败：%+v", res)
	}
	if code := displayCode(res); code != "TOOL_SEARCH_OK" {
		t.Fatalf("code=%q，期望 TOOL_SEARCH_OK", code)
	}
	for _, want := range []string{"read", "list", "下一轮可直接调用"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("搜索结果缺 %q：%s", want, res.Content)
		}
	}
	activated, _ := res.Display["activated"].([]string)
	if !equalStrings(activated, []string{"read", "list"}) {
		t.Fatalf("activated=%v，期望 [read list]", activated)
	}

	// 下一轮请求（Defs）必须出现刚激活的工具，且没激活的 web_search 不出现。
	defs := defNames(c.Defs())
	if !equalStrings(defs, []string{"read", "list"}) {
		t.Fatalf("激活后 Defs=%v，期望 [read list]", defs)
	}
	if total, active := c.Len(); total != 3 || active != 2 {
		t.Fatalf("Len=(%d,%d)，期望 (3,2)", total, active)
	}

	// 再搜同一批：已成为「此前都已激活」，不重复计新激活。
	again := runTool(t, tool, map[string]any{"query": "文件"})
	activated, _ = again.Display["activated"].([]string)
	if len(activated) != 0 {
		t.Fatalf("重复搜索不应再新激活，实际 %v", activated)
	}
	if !strings.Contains(again.Content, "此前都已激活") {
		t.Fatalf("重复搜索文案应说明此前已激活：%s", again.Content)
	}
}

// TestSearchToolActivateFalse activate=false 时只返回候选、不激活。
func TestSearchToolActivateFalse(t *testing.T) {
	c := newSearchCatalog()
	res := runTool(t, SearchTool(c), map[string]any{"query": "文件", "activate": false, "limit": 1})
	if res.IsError {
		t.Fatalf("搜索失败：%+v", res)
	}
	if count, _ := res.Display["count"].(int); count != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %v", res.Display["count"])
	}
	if total, active := c.Len(); total != 3 || active != 0 {
		t.Fatalf("activate=false 不应改变激活状态，Len=(%d,%d)", total, active)
	}
	if !strings.Contains(res.Content, "本次未激活") {
		t.Fatalf("文案应说明未激活：%s", res.Content)
	}
}

// TestSearchToolLimitClamp limit 缺省 5、上限 12。
func TestSearchToolLimitClamp(t *testing.T) {
	tools := make([]agent.Tool, 0, 15)
	for i := 0; i < 15; i++ {
		tools = append(tools, &fakeTool{name: fmt.Sprintf("tool%02d", i), desc: "工具说明", risk: RiskRead})
	}
	c := NewCatalog(Toolset{Name: "many", Desc: "多", Tools: tools})
	tool := SearchTool(c)

	res := runTool(t, tool, map[string]any{"query": "工具", "limit": 99})
	if count, _ := res.Display["count"].(int); count != 12 {
		t.Fatalf("limit=99 应被夹到 12，实际 %d", count)
	}
	res = runTool(t, tool, map[string]any{"query": "工具"})
	if count, _ := res.Display["count"].(int); count != 5 {
		t.Fatalf("缺省 limit 应为 5，实际 %d", count)
	}
	if matches, _ := res.Display["matches"].([]string); len(matches) != 5 {
		t.Fatalf("matches 应有 5 条，实际 %v", res.Display["matches"])
	}
}

// TestSearchToolEmptyQuery 空 query 要给出可修复报错。
func TestSearchToolEmptyQuery(t *testing.T) {
	c := newSearchCatalog()
	for _, args := range []any{map[string]any{}, map[string]any{"query": "   "}, nil} {
		res := runTool(t, SearchTool(c), args)
		if !res.IsError || displayCode(res) != "TOOL_SEARCH_QUERY_MISSING" {
			t.Fatalf("args=%v 应报 TOOL_SEARCH_QUERY_MISSING：%+v", args, res)
		}
		if !strings.Contains(res.Content, "query") {
			t.Fatalf("报错应点名字段 query：%s", res.Content)
		}
	}
}

// TestSearchToolNoMatch 没有命中时明确告诉模型换关键词，不要猜工具。
func TestSearchToolNoMatch(t *testing.T) {
	c := newSearchCatalog()
	res := runTool(t, SearchTool(c), map[string]any{"query": "不存在的关键词"})
	if res.IsError {
		t.Fatalf("无命中不是错误：%+v", res)
	}
	if code := displayCode(res); code != "TOOL_SEARCH_EMPTY" {
		t.Fatalf("code=%q，期望 TOOL_SEARCH_EMPTY", code)
	}
	if !strings.Contains(res.Content, "没有找到") {
		t.Fatalf("文案应说明没找到：%s", res.Content)
	}
}
