package rag

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// 本文件把两个记忆工具**真的挂进 agent.Run 主循环**跑一遍：
// 假模型（agent.SetStreamer 脚本化）第 1 轮要求调用 memory_search，
// 第 2 轮给纯文本答复。这样验的不是"工具自己能不能跑"，而是
// 「模型要调 → 循环执行 → 结果回灌 → 模型作答」这条完整链路里我的工具是否接得上。

type loopBackend struct {
	search *fakeSearcher
	write  *fakeWriter
}

func (b *loopBackend) Search(ctx context.Context, userID, query string, topK int) ([]Hit, error) {
	return b.search.Search(ctx, userID, query, topK)
}

func (b *loopBackend) IngestText(ctx context.Context, userID, title, text string) (Result, error) {
	return b.write.IngestText(ctx, userID, title, text)
}

// scriptedStreamer 按"历史里带 tool_calls 的 assistant 消息条数"判定轮次
// （与 llm/mock.go 同一套办法），把每一轮的收发记录下来供断言。
type scriptedStreamer struct {
	mu    sync.Mutex
	reqs  []llm.ChatRequest
	steps []func(req llm.ChatRequest, ch chan<- llm.Event)
}

func (s *scriptedStreamer) fn() agent.StreamFunc {
	return func(_ context.Context, _ string, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
		round := 0
		for _, m := range req.Messages {
			if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
				round++
			}
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		step := s.steps
		s.mu.Unlock()

		ch := make(chan llm.Event, 16)
		go func() {
			defer close(ch)
			if round >= len(step) {
				ch <- llm.Event{Type: llm.EventDelta, Content: "（脚本没有第 " + strconv.Itoa(round+1) + " 轮）"}
				ch <- llm.Event{Type: llm.EventDone}
				return
			}
			step[round](req, ch)
		}()
		return ch, nil
	}
}

func (s *scriptedStreamer) round(n int) llm.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.reqs) {
		return llm.ChatRequest{}
	}
	return s.reqs[n]
}

func toolCallStep(name, args string) func(llm.ChatRequest, chan<- llm.Event) {
	return func(_ llm.ChatRequest, ch chan<- llm.Event) {
		ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "call_1", Name: name, Arguments: args}}
		ch <- llm.Event{Type: llm.EventDone}
	}
}

func textStep(text string) func(llm.ChatRequest, chan<- llm.Event) {
	return func(_ llm.ChatRequest, ch chan<- llm.Event) {
		ch <- llm.Event{Type: llm.EventDelta, Content: text}
		ch <- llm.Event{Type: llm.EventDone}
	}
}

func runLoop(t *testing.T, s *scriptedStreamer, tools []agent.Tool) ([]agent.Event, error) {
	t.Helper()
	prev := agent.Streamer()
	agent.SetStreamer(s.fn())
	t.Cleanup(func() { agent.SetStreamer(prev) })

	var events []agent.Event
	err := agent.Run(context.Background(),
		agent.Config{Model: "mock-echo", SystemPrompt: "你是测试用助手", Tools: tools},
		[]llm.Message{{Role: llm.RoleUser, Content: "我平时爱喝什么？"}},
		func(ev agent.Event) { events = append(events, ev) },
	)
	return events, err
}

func names(events []agent.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Kind))
	}
	return out
}

// 主链路：模型要调 memory_search → 工具真的检索（假替身记录了 query）→ 结果回灌 → 模型作答。
func TestMemorySearchInsideAgentLoop(t *testing.T) {
	var search fakeSearcher
	search.hits = sampleHits()
	backend := &loopBackend{search: &search, write: &fakeWriter{}}
	tools := NewMemoryTools(backend, "u1")

	args := `{"query":"咖啡偏好","limit":3}`
	s := &scriptedStreamer{steps: []func(llm.ChatRequest, chan<- llm.Event){
		toolCallStep("memory_search", args),
		textStep("你偏好美式咖啡。"),
	}}

	events, err := runLoop(t, s, tools)
	if err != nil {
		t.Fatalf("主循环不应报错：%v", err)
	}

	got := strings.Join(names(events), ",")
	for _, want := range []string{"tool_call", "tool_result", "delta", "done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("事件序列缺少 %s，实际：%s", want, got)
		}
	}

	// 工具真的被执行了：假检索器收到了模型给的 query 与 limit
	if search.gotQuery != "咖啡偏好" {
		t.Fatalf("检索器收到的 query 应为「咖啡偏好」，实际 %q", search.gotQuery)
	}
	if search.gotTopK != 3 {
		t.Fatalf("检索器收到的 topK 应为 3，实际 %d", search.gotTopK)
	}

	// tool_result 事件：成功、带工具名与摘要
	var result *agent.Event
	for i := range events {
		if events[i].Kind == agent.EvToolResult {
			result = &events[i]
			break
		}
	}
	if result == nil {
		t.Fatal("没有发出 tool_result 事件")
	}
	if !result.OK || result.ToolName != "memory_search" {
		t.Fatalf("tool_result 应成功且工具名为 memory_search，实际 ok=%v name=%q", result.OK, result.ToolName)
	}
	if !strings.Contains(result.Detail, "用户喜欢喝美式咖啡") {
		t.Fatalf("tool_result 的 Detail 应带命中正文，实际 %q", clipRunes(result.Detail, 80))
	}

	// 第 2 轮请求里必须带回 role=tool 的结果 + 模型被暴露了这两个工具
	second := s.round(1)
	if len(second.Messages) == 0 {
		t.Fatal("没有观察到第 2 轮请求")
	}
	var sawToolMsg bool
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == "call_1" && strings.Contains(m.Content, "用户喜欢喝美式咖啡") {
			sawToolMsg = true
		}
	}
	if !sawToolMsg {
		t.Fatalf("第 2 轮请求里应带 role=tool 的检索结果，实际消息：%+v", second.Messages)
	}
	names := make([]string, 0, len(second.Tools))
	for _, d := range second.Tools {
		names = append(names, d.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "memory_search") || !strings.Contains(strings.Join(names, ","), "memory_write") {
		t.Fatalf("暴露给模型的工具清单应含 memory_search 与 memory_write，实际 %v", names)
	}

	// 最终答复透传
	if !strings.Contains(got, "delta") {
		t.Fatal("应收到模型的最终答复 delta")
	}
	t.Logf("事件序列：%s；检索 query=%q；第 2 轮工具清单=%v", got, search.gotQuery, names)
}

// 记忆检索失败**不能**把对话弄断：工具以 is_error 回灌，循环继续，模型照样作答。
func TestMemorySearchFailureDoesNotKillLoop(t *testing.T) {
	backend := &loopBackend{
		search: &fakeSearcher{err: errMemoryBoom},
		write:  &fakeWriter{},
	}
	tools := NewMemoryTools(backend, "u1")

	s := &scriptedStreamer{steps: []func(llm.ChatRequest, chan<- llm.Event){
		toolCallStep("memory_search", `{"query":"任意"}`),
		textStep("我想不起来了，但对话还在。"),
	}}

	events, err := runLoop(t, s, tools)
	if err != nil {
		t.Fatalf("工具失败不应让主循环报错（契约 §3.5），实际：%v", err)
	}
	got := strings.Join(names(events), ",")
	if strings.Contains(got, "error") {
		t.Fatalf("不应发出 error 事件（失败要作为 is_error 工具结果回灌），实际：%s", got)
	}
	if !strings.Contains(got, "tool_result") || !strings.Contains(got, "delta") || !strings.Contains(got, "done") {
		t.Fatalf("应走完 tool_result → delta → done，实际：%s", got)
	}
	for _, ev := range events {
		if ev.Kind == agent.EvToolResult {
			if ev.OK {
				t.Fatal("检索失败时 tool_result 应是失败态")
			}
			if !strings.Contains(ev.Detail, "记忆检索失败") {
				t.Fatalf("失败详情应说明是记忆检索失败，实际 %q", clipRunes(ev.Detail, 80))
			}
		}
	}
	t.Logf("事件序列：%s", got)
}

var errMemoryBoom = errMemoryBoomValue()

func errMemoryBoomValue() error { return errors.New("模拟记忆库故障") }
