package toolsys

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// riskFunc 把「工具名 → 风险」映射包成 Plan 需要的回调。
func riskFunc(risks map[string]Risk) func(string) (Risk, bool) {
	return func(name string) (Risk, bool) {
		risk, ok := risks[name]
		return risk, ok
	}
}

// TestPlanBatches 对应契约 §3.3/§7.6：连续只读调用并批，写/执行/网络永不混批。
func TestPlanBatches(t *testing.T) {
	tools := map[string]Risk{
		"read":    RiskRead,
		"list":    RiskRead,
		"write":   RiskWrite,
		"exec":    RiskExec,
		"web":     RiskNetwork,
		"evolve":  RiskSelf,
		"unknown": "",
	}
	calls := func(names ...string) []llm.ToolCall {
		out := make([]llm.ToolCall, len(names))
		for i, name := range names {
			out[i] = llm.ToolCall{ID: name, Name: name}
		}
		return out
	}

	cases := []struct {
		name  string
		calls []llm.ToolCall
		want  [][]int
	}{
		{name: "全部只读并成一批", calls: calls("read", "list", "read"), want: [][]int{{0, 1, 2}}},
		{name: "读写不混批", calls: calls("read", "write", "read", "web", "read"), want: [][]int{{0}, {1}, {2}, {3}, {4}}},
		{name: "连续只读之间被写打断", calls: calls("read", "write", "list", "exec"), want: [][]int{{0}, {1}, {2}, {3}}},
		{name: "只读-只读/写/只读-只读", calls: calls("read", "list", "write", "read", "list"), want: [][]int{{0, 1}, {2}, {3, 4}}},
		{name: "未知风险按非只读单独一批", calls: calls("read", "unknown", "read"), want: [][]int{{0}, {1}, {2}}},
		{name: "改变自身工具单独一批", calls: calls("evolve", "read"), want: [][]int{{0}, {1}}},
		{name: "空序列", calls: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(tc.calls, riskFunc(tools))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Plan=%v，期望 %v", got, tc.want)
			}
		})
	}

	if got := Plan(calls("read", "list"), nil); !reflect.DeepEqual(got, [][]int{{0}, {1}}) {
		t.Fatalf("riskOf=nil 时不能猜只读，应全部单独执行，实际 %v", got)
	}
}

// sleepTool 构造一个「睡 d 再返回 result-<name>」的假工具。
func sleepTool(name string, d time.Duration, counter *int32) *fakeTool {
	return &fakeTool{
		name: name,
		exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			if counter != nil {
				atomic.AddInt32(counter, 1)
			}
			time.Sleep(d)
			return agent.Result{Content: "result-" + name}, nil
		},
	}
}

// TestRunBatchConcurrent 对应契约 §7.6：3 个只读调用并发总耗时明显小于串行。
func TestRunBatchConcurrent(t *testing.T) {
	tools := []*fakeTool{
		sleepTool("t0", 120*time.Millisecond, nil),
		sleepTool("t1", 120*time.Millisecond, nil),
		sleepTool("t2", 120*time.Millisecond, nil),
	}
	reg := agent.NewRegistry(toAgentTools(tools)...)
	calls := []llm.ToolCall{
		{ID: "0", Name: "t0", Arguments: "{}"},
		{ID: "1", Name: "t1", Arguments: "{}"},
		{ID: "2", Name: "t2", Arguments: "{}"},
	}

	start := time.Now()
	results := RunBatch(context.Background(), &agent.Exec{SessionID: "s"}, reg, calls, 3)
	elapsed := time.Since(start)
	if len(results) != 3 {
		t.Fatalf("返回 %d 个结果，期望 3", len(results))
	}
	// 串行需要 360ms，并行应在 300ms 内完成；断言留足机器抖动余量。
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("3 个 120ms 调用并发耗时 %v，应 <300ms（串行 360ms）", elapsed)
	}
	// 结果顺序必须与输入顺序一致（上层按序发事件即稳定）。
	for i, res := range results {
		if res.IsError {
			t.Fatalf("第 %d 个结果不应失败：%+v", i, res)
		}
		if want := "result-" + calls[i].Name; res.Content != want {
			t.Fatalf("第 %d 个结果=%q，期望 %q（必须按输入顺序返回）", i, res.Content, want)
		}
	}
}

// TestRunBatchSerialLimit maxParallel=1 时应串行执行。
func TestRunBatchSerialLimit(t *testing.T) {
	tools := []*fakeTool{
		sleepTool("t0", 120*time.Millisecond, nil),
		sleepTool("t1", 120*time.Millisecond, nil),
		sleepTool("t2", 120*time.Millisecond, nil),
	}
	reg := agent.NewRegistry(toAgentTools(tools)...)
	calls := []llm.ToolCall{
		{ID: "0", Name: "t0", Arguments: "{}"},
		{ID: "1", Name: "t1", Arguments: "{}"},
		{ID: "2", Name: "t2", Arguments: "{}"},
	}
	start := time.Now()
	results := RunBatch(context.Background(), &agent.Exec{}, reg, calls, 1)
	elapsed := time.Since(start)
	if len(results) != 3 {
		t.Fatalf("返回 %d 个结果，期望 3", len(results))
	}
	if elapsed < 320*time.Millisecond {
		t.Fatalf("maxParallel=1 应串行（≥360ms），实际 %v", elapsed)
	}
}

// TestRunBatchCanceledBeforeStart ctx 已取消时不启动任何调用。
func TestRunBatchCanceledBeforeStart(t *testing.T) {
	inner := &fakeTool{name: "read"}
	reg := agent.NewRegistry(inner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := RunBatch(ctx, &agent.Exec{}, reg, []llm.ToolCall{{ID: "1", Name: "read"}}, 4)
	if len(results) != 1 || !results[0].IsError || displayCode(results[0]) != "TOOL_CANCELLED" {
		t.Fatalf("取消后应返回 TOOL_CANCELLED：%+v", results)
	}
	if inner.callCount() != 0 {
		t.Fatalf("取消后不应执行工具，实际 %d 次", inner.callCount())
	}
}

// TestRunBatchCancelStopsNewCalls 运行中取消：已启动的跑完，后续不再启动。
func TestRunBatchCancelStopsNewCalls(t *testing.T) {
	var slowRuns, fastRuns int32
	slow := sleepTool("slow", 150*time.Millisecond, &slowRuns)
	fast := sleepTool("fast", 0, &fastRuns)
	reg := agent.NewRegistry(slow, fast)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	results := RunBatch(ctx, &agent.Exec{}, reg, []llm.ToolCall{
		{ID: "1", Name: "slow", Arguments: "{}"},
		{ID: "2", Name: "fast", Arguments: "{}"},
	}, 1)
	if len(results) != 2 {
		t.Fatalf("返回 %d 个结果，期望 2", len(results))
	}
	if results[0].IsError {
		t.Fatalf("已启动的调用应正常完成：%+v", results[0])
	}
	if !results[1].IsError || displayCode(results[1]) != "TOOL_CANCELLED" {
		t.Fatalf("取消后未启动的调用应为 TOOL_CANCELLED：%+v", results[1])
	}
	if got := atomic.LoadInt32(&slowRuns); got != 1 {
		t.Fatalf("slow 应执行 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&fastRuns); got != 0 {
		t.Fatalf("取消后不应再启动 fast，实际 %d 次", got)
	}
}

// TestRunBatchFailureModes 未知工具/panic/业务报错都折成 is_error，不能打挂批处理。
func TestRunBatchFailureModes(t *testing.T) {
	boom := &fakeTool{name: "boom", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		panic("炸了")
	}}
	denied := &fakeTool{name: "denied", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{IsError: true, Content: "没有权限", Display: map[string]any{"code": "DENIED"}}, nil
	}}
	badArgs := &fakeTool{name: "bad_args"}
	reg := agent.NewRegistry(boom, denied, badArgs)
	results := RunBatch(context.Background(), &agent.Exec{}, reg, []llm.ToolCall{
		{ID: "1", Name: "missing", Arguments: "{}"},
		{ID: "2", Name: "boom", Arguments: "{}"},
		{ID: "3", Name: "denied", Arguments: "{}"},
		{ID: "4", Name: "bad_args", Arguments: "{oops"},
	}, 2)
	if len(results) != 4 {
		t.Fatalf("返回 %d 个结果，期望 4", len(results))
	}
	wantCodes := []string{"TOOL_UNKNOWN", "TOOL_PANIC", "DENIED", "TOOL_BAD_ARGS"}
	for i, want := range wantCodes {
		if !results[i].IsError || displayCode(results[i]) != want {
			t.Fatalf("第 %d 个结果 code=%q，期望 %q：%+v", i, displayCode(results[i]), want, results[i])
		}
	}
	if results[2].Content != "没有权限" {
		t.Fatalf("业务 is_error 文案应原样保留，实际 %q", results[2].Content)
	}
}

// TestRunBatchEmpty 空调用返回 nil。
func TestRunBatchEmpty(t *testing.T) {
	if got := RunBatch(context.Background(), nil, nil, nil, 4); got != nil {
		t.Fatalf("空输入应返回 nil，实际 %#v", got)
	}
}

// toAgentTools 把具体工具切片转成 agent.Tool 切片。
func toAgentTools[T agent.Tool](items []T) []agent.Tool {
	out := make([]agent.Tool, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
