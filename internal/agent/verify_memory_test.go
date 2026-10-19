package agent_test

// 独立验收（task-43，verifier 自写）：记忆工具 / 记忆块注入 / 循环上限边界。
// 这些都是实现方在「未覆盖面」里承认没验的（AGENT_MAX_* 边界），以及契约 §6/§9.7 的记忆条款。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/llm"
	"blog-my/internal/rag"
)

// vFakeMemory 是可注入的假记忆后端：既能造命中，也能造失败。
type vFakeMemory struct {
	hits      []rag.Hit
	searchErr error
	gotQuery  string
	gotTopK   int
	wroteText string
	wroteTags []string
	writeErr  error
}

func (f *vFakeMemory) Search(ctx context.Context, userID, query string, topK int) ([]rag.Hit, error) {
	f.gotQuery, f.gotTopK = query, topK
	return f.hits, f.searchErr
}

func (f *vFakeMemory) IngestText(ctx context.Context, userID, title, text string) (rag.Result, error) {
	f.wroteText = text
	return rag.Result{}, f.writeErr
}

func vToolByName(t *testing.T, ts []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, x := range ts {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("没有工具 %s（实际：%v）", name, vToolNames(ts))
	return nil
}

func vToolNames(ts []agent.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, x := range ts {
		out = append(out, x.Name())
	}
	return out
}

// TestVerifyMemoryTools：契约 §6/§9.7 —— memory_search / memory_write 可用。
func TestVerifyMemoryTools(t *testing.T) {
	fake := &vFakeMemory{hits: []rag.Hit{{Title: "旧事", Text: "用户喜欢喝美式", Score: 0.9}}}
	ts := rag.NewMemoryTools(fake, "u1")
	names := vToolNames(ts)
	hasSearch, hasWrite := false, false
	for _, n := range names {
		if n == "memory_search" {
			hasSearch = true
		}
		if n == "memory_write" {
			hasWrite = true
		}
	}
	if !hasSearch || !hasWrite {
		t.Fatalf("记忆工具应同时提供 memory_search 与 memory_write，实际 %v", names)
	}

	ex := &agent.Exec{SessionID: "v-mem", Root: "", Signal: context.Background()}
	args, _ := json.Marshal(map[string]any{"query": "我喜欢喝什么", "limit": 3})
	res, err := vToolByName(t, ts, "memory_search").Execute(context.Background(), args, ex)
	if err != nil {
		t.Fatalf("memory_search 不应报错：%v", err)
	}
	if !strings.Contains(res.Content, "用户喜欢喝美式") {
		t.Fatalf("memory_search 结果应含命中正文，实际 %q", res.Content)
	}
	if fake.gotQuery != "我喜欢喝什么" {
		t.Fatalf("query 没传下去：%q", fake.gotQuery)
	}
	t.Logf("memory_search 结果 = %q（topK 实际 %d）", res.Content, fake.gotTopK)

	wargs, _ := json.Marshal(map[string]any{"text": "用户换了口味", "tags": []string{"偏好"}})
	wres, err := vToolByName(t, ts, "memory_write").Execute(context.Background(), wargs, ex)
	if err != nil {
		t.Fatalf("memory_write 不应报错：%v", err)
	}
	if res.IsError || wres.IsError {
		t.Fatalf("正常调用不该是 is_error：search=%v write=%v", res.IsError, wres.IsError)
	}
	if fake.wroteText != "用户换了口味" {
		t.Fatalf("写入正文没传下去：%q", fake.wroteText)
	}
	t.Logf("memory_write 结果 = %q，后端收到 %q", wres.Content, fake.wroteText)

	// 负向：检索后端报错 → 工具必须**如实报错**（不能假装检索到空结果）
	fake.searchErr = errors.New("嵌入服务不可达")
	bad, err := vToolByName(t, ts, "memory_search").Execute(context.Background(), args, ex)
	if err == nil && !bad.IsError {
		t.Fatalf("检索失败必须如实报错，实际 content=%q is_error=%v", bad.Content, bad.IsError)
	}
	t.Logf("检索失败时：err=%v is_error=%v content=%q", err, bad.IsError, bad.Content)
}

// TestVerifyMemoryBlock：契约 §5/§6 —— 自动注入的 <memory> 块；检索失败必须能把错误传出来（好让上层降级）。
func TestVerifyMemoryBlock(t *testing.T) {
	fake := &vFakeMemory{hits: []rag.Hit{{Title: "笔记", Text: "项目用 MySQL 5.7", Score: 0.8}}}
	block, err := rag.MemoryBlock(context.Background(), fake, "u1", "用什么数据库", rag.DefaultMemoryTopK)
	if err != nil {
		t.Fatalf("正常检索不该报错：%v", err)
	}
	if !strings.Contains(block, "<memory>") || !strings.Contains(block, "项目用 MySQL 5.7") {
		t.Fatalf("记忆块应含 <memory> 包裹的命中正文，实际 %q", block)
	}
	t.Logf("记忆块 = %q", block)

	// 负向：后端失败 → 必须返回 error（上层据此降级），且不能 panic / 伪造内容
	fail := &vFakeMemory{searchErr: errors.New("连接被拒")}
	b2, e2 := rag.MemoryBlock(context.Background(), fail, "u1", "q", rag.DefaultMemoryTopK)
	if e2 == nil {
		t.Fatalf("检索失败时 MemoryBlock 必须报错，实际 block=%q", b2)
	}
	if strings.Contains(b2, "<memory>") {
		t.Fatalf("失败时不该产出记忆块：%q", b2)
	}
	t.Logf("失败时：err=%v block=%q", e2, b2)

	// 边界：topK 上限夹取
	if got := rag.ClampMemoryLimit(9999); got > rag.MaxMemoryTopK {
		t.Fatalf("topK 应被夹到 %d，实际 %d", rag.MaxMemoryTopK, got)
	}
	if got := rag.ClampMemoryLimit(0); got <= 0 {
		t.Fatalf("topK=0 应回落到默认值，实际 %d", got)
	}
}

// vRunLoop 装 mock 后端跑一次主循环（不需要 MySQL）。
func vRunLoop(t *testing.T, cfg agent.Config, msgs []llm.Message) ([]agent.Event, error) {
	t.Helper()
	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	agent.SetStreamer(agent.RegistryStreamer(reg))
	t.Cleanup(func() { agent.SetStreamer(nil); llm.ClearMockScript() })
	var evs []agent.Event
	err := agent.Run(context.Background(), cfg, msgs, func(ev agent.Event) { evs = append(evs, ev) })
	return evs, err
}

func vAlwaysToolScript(n int, t *testing.T) {
	t.Helper()
	turns := make([]llm.MockTurn, 0, n)
	for i := 0; i < n; i++ {
		args, _ := json.Marshal(map[string]any{"file_path": "loop.txt", "content": fmt.Sprintf("v%d", i)})
		turns = append(turns, llm.MockTurn{ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "write", Arguments: string(args)}}})
	}
	llm.SetMockScript(turns)
}

// TestVerifyMaxToolCallsBoundary：实现方承认「AGENT_MAX_* 边界值未压测」，这里补上。
func TestVerifyMaxToolCallsBoundary(t *testing.T) {
	vAlwaysToolScript(8, t)
	cfg := agent.Config{
		Model:        "mock-echo",
		MaxToolCalls: 2, // 边界：故意设得比脚本轮数小
		Tools:        agentfs.Tools(),
		Exec:         &agent.Exec{SessionID: "v-maxcalls", Root: t.TempDir(), Signal: context.Background()},
	}
	evs, err := vRunLoop(t, cfg, []llm.Message{{Role: llm.RoleUser, Content: "一直写"}})
	if !errors.Is(err, agent.ErrMaxToolCalls) {
		t.Fatalf("应触顶 ErrMaxToolCalls，实际 err=%v", err)
	}
	results := 0
	for _, ev := range evs {
		if ev.Kind == agent.EvToolResult {
			results++
		}
	}
	if results != 2 {
		t.Fatalf("工具执行次数应正好等于上限 2，实际 %d", results)
	}
	t.Logf("触顶：err=%v，tool_result 次数=%d", err, results)
}

// TestVerifyMaxIterationsBoundary：轮数上限边界。
func TestVerifyMaxIterationsBoundary(t *testing.T) {
	vAlwaysToolScript(8, t)
	cfg := agent.Config{
		Model:         "mock-echo",
		MaxIterations: 2,
		MaxToolCalls:  10,
		Tools:         agentfs.Tools(),
		Exec:          &agent.Exec{SessionID: "v-maxiter", Root: t.TempDir(), Signal: context.Background()},
	}
	evs, err := vRunLoop(t, cfg, []llm.Message{{Role: llm.RoleUser, Content: "一直写"}})
	if !errors.Is(err, agent.ErrMaxIterations) {
		t.Fatalf("应触顶 ErrMaxIterations，实际 err=%v", err)
	}
	calls := 0
	for _, ev := range evs {
		if ev.Kind == agent.EvToolCall {
			calls++
		}
	}
	t.Logf("触顶：err=%v，tool_call 次数=%d（上限 2 轮）", err, calls)
	if calls == 0 {
		t.Fatalf("第一轮就该发过 tool_call")
	}
}
