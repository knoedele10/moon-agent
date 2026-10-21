package web

import (
	"context"
	"sync"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
	"blog-my/internal/llm"
)

// 契约 §7 第 3 条的端到端一半：渐进披露不是「一次搜索全局生效」，
// 而是「本轮搜到 → 下一轮定义里才出现」。
//
// 步骤：第 1 轮模型只看到核心集（含 tool_search，不含 write）→ 调 tool_search
// 搜 write → 第 2 轮请求里的 tools 必须已经包含 write（说明 Catalog 被激活且
// 循环每轮重取 Defs）→ 调 write → 第 3 轮正常收尾。
func TestToolSearchActivationTakesEffectNextRound(t *testing.T) {
	hermesClearEnv(t)
	t.Setenv("AGENT_TOOL_DISCLOSURE", "search")

	tools, source := buildAgentToolSource(hermesWiringTools())
	if source == nil {
		t.Fatal("disclosure=search 应装配工具目录")
	}
	if _, ok := source.(*toolsys.Catalog); !ok {
		t.Fatalf("工具来源类型不对：%T", source)
	}

	var mu sync.Mutex
	var rounds [][]string
	prev := agent.Streamer()
	t.Cleanup(func() { agent.SetStreamer(prev) })
	agent.SetStreamer(func(_ context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
		mu.Lock()
		rounds = append(rounds, toolDefNames(req.Tools))
		n := len(rounds)
		mu.Unlock()

		ch := make(chan llm.Event, 4)
		go func() {
			defer close(ch)
			switch n {
			case 1:
				ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "1", Name: "tool_search", Arguments: `{"query":"write"}`}}
			case 2:
				ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "2", Name: "write", Arguments: `{}`}}
			default:
				ch <- llm.Event{Type: llm.EventDelta, Content: "写完了。"}
				ch <- llm.Event{Type: llm.EventDone}
			}
		}()
		return ch, nil
	})

	cfg := agent.Config{Model: "mock-model", Tools: tools, ToolSource: source, MaxIterations: 6}
	err := agent.Run(context.Background(), cfg,
		[]llm.Message{{Role: llm.RoleUser, Content: "把结果写下来"}},
		func(agent.Event) {})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(rounds) < 2 {
		t.Fatalf("模型调用轮数不足：%d", len(rounds))
	}
	if !containsString(rounds[0], "tool_search") {
		t.Fatalf("第 1 轮必须有 tool_search：%v", rounds[0])
	}
	if containsString(rounds[0], "write") {
		t.Fatalf("第 1 轮不该看到未激活的 write：%v", rounds[0])
	}
	if !containsString(rounds[1], "write") {
		t.Fatalf("第 2 轮必须看到被搜到并激活的 write：%v", rounds[1])
	}
}
