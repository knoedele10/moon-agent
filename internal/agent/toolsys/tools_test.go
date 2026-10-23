package toolsys

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// fakeTool 是测试用工具：记录收到的原始参数，并允许伪造执行结果/耗时。
type fakeTool struct {
	name   string
	desc   string
	params map[string]any
	risk   Risk
	tags   []string
	exec   func(ctx context.Context, raw json.RawMessage) (agent.Result, error)

	mu    sync.Mutex
	calls []json.RawMessage
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Description() string {
	if f.desc == "" {
		return f.name
	}
	return f.desc
}

func (f *fakeTool) Parameters() map[string]any {
	if f.params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return f.params
}

// Risk 实现 RiskAnnotated：返回空值（非法）时 Catalog 改查 LookupMeta，不再用组默认风险。
func (f *fakeTool) Risk() Risk { return f.risk }

// Tags 实现 TagsAnnotated。
func (f *fakeTool) Tags() []string { return f.tags }

func (f *fakeTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append(json.RawMessage(nil), raw...))
	f.mu.Unlock()
	if f.exec != nil {
		return f.exec(ctx, raw)
	}
	return agent.Result{Content: f.name + " ok"}, nil
}

// callCount 返回被调用次数。
func (f *fakeTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// lastArgs 解码最近一次收到的参数。
func (f *fakeTool) lastArgs(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("工具 %s 从未被调用", f.name)
	}
	raw := f.calls[len(f.calls)-1]
	if len(raw) == 0 {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("工具 %s 收到的参数不是 JSON 对象: %v", f.name, err)
	}
	return args
}

// lastRaw 返回最近一次收到的原始参数（用于断言「原样放行」）。
func (f *fakeTool) lastRaw(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("工具 %s 从未被调用", f.name)
	}
	return string(f.calls[len(f.calls)-1])
}

// runTool 用给定参数执行一次工具（参数可以是 map、JSON 字符串或 nil）。
func runTool(t *testing.T, tool agent.Tool, args any) agent.Result {
	t.Helper()
	var raw json.RawMessage
	switch typed := args.(type) {
	case nil:
		raw = nil
	case string:
		raw = json.RawMessage(typed)
	case json.RawMessage:
		raw = typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			t.Fatalf("测试参数无法序列化: %v", err)
		}
		raw = encoded
	}
	res, err := tool.Execute(context.Background(), raw, &agent.Exec{SessionID: "test-session"})
	if err != nil {
		t.Fatalf("工具 %s 返回 error（应为 is_error 结果）: %v", tool.Name(), err)
	}
	return res
}

// displayCode 取结果里的错误码（没有则空串）。
func displayCode(res agent.Result) string {
	if res.Display == nil {
		return ""
	}
	code, _ := res.Display["code"].(string)
	return code
}

// objectSchema 构造一个只含 properties 的对象 schema。
func objectSchema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// names 把 Match 列表压成名字，便于断言。
func matchNames(matches []Match) []string {
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.Name)
	}
	return out
}

// defNames 把 ToolDef 列表压成名字。
func defNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Name)
	}
	return out
}

// equalStrings 比较两个字符串切片。
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

// goName 给并发测试生成工具名。
func goName(prefix string, index int) string { return fmt.Sprintf("%s%d", prefix, index) }
