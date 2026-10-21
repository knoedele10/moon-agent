package toolsys

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// argSpy 记录工具本体收到的参数：用来证明「动态激活的工具仍然带着参数装饰器」。
type argSpy struct {
	name string
	seen string
}

func (s *argSpy) Name() string        { return s.name }
func (s *argSpy) Description() string { return "记录收到的参数" }
func (s *argSpy) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n": map[string]any{"type": "integer"},
		},
		"required": []string{"n"},
	}
}

func (s *argSpy) Execute(_ context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	s.seen = string(raw)
	return agent.Result{Content: "ok"}, nil
}

// 契约 §3.1 说「每个工具自动套装饰器」。渐进披露下工具有两条进入路径：
// core 常驻、以及 tool_search 激活。激活只是改可见性，不能绕开装饰器——
// 否则同一次调用会「直接暴露时参数被修好、搜索出来后却报参数错」。
func TestActivatedToolsKeepDecorators(t *testing.T) {
	spy := &argSpy{name: "spy"}
	c := NewCatalog(
		Toolset{Name: "core", Desc: "核心", Tools: []agent.Tool{}},
		Toolset{Name: "misc", Desc: "其它", Tools: []agent.Tool{spy}},
	)

	// 未激活：不在定义清单里。
	for _, d := range c.Defs() {
		if d.Name == "spy" {
			t.Fatal("未激活的工具不该出现在 Defs 里")
		}
	}
	// 但目录里已经有它（能按名解析，只是模型看不到）。
	if _, ok := c.Lookup("spy"); !ok {
		t.Fatal("Lookup 应能按名找到未激活的工具")
	}

	c.Activate("spy")
	tool, ok := c.Lookup("spy")
	if !ok {
		t.Fatal("激活后应能解析到工具")
	}
	// 故意给错类型：字符串 "42" 应被 Coerce 修成整数 42，而不是把错误丢给模型。
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"n":"42"}`), nil); err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if !strings.Contains(spy.seen, `"n":42`) {
		t.Fatalf("装饰器没有生效，工具收到 %s", spy.seen)
	}
}
