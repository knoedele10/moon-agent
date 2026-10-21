package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"blog-my/internal/llm"
)

// 并行只读批：契约 §7 第 6 条（3 个只读调用总耗时 < 串行一半；写工具不混批；事件顺序=输入顺序）。
//
// 这里用真实的 sleep 计时：单次 120ms，串行 360ms+；并发应落在 ~120-200ms。
// 阈值取「明显小于串行」而不是精确值，避免 CI 抖动误判。

func sleepTool(name string, d time.Duration) *fakeTool {
	return &fakeTool{name: name, exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		time.Sleep(d)
		return Result{Content: name + " ok"}, nil
	}}
}

// 工具执行途中取消后，瞬时失败也不能触发第二次调用。
func TestParallelCanceledReadIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var count atomic.Int32
	read := &fakeTool{name: "read_cancel", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		count.Add(1)
		cancel()
		return Result{IsError: true, Content: "temporary timeout"}, nil
	}}
	results := runReadOnlyBatch(ctx, NewRegistry(read), nil, []llm.ToolCall{{Name: read.name}}, 1)
	if count.Load() != 1 || len(results) != 1 || !results[0].IsError {
		t.Fatalf("取消后不应重试，调用次数=%d，结果=%+v", count.Load(), results)
	}
}

func resultIDs(evs []Event) []string {
	out := make([]string, 0, 4)
	for _, ev := range evs {
		if ev.Kind == EvToolResult {
			out = append(out, ev.ToolID)
		}
	}
	return out
}

func equalSeq(a, b []string) bool {
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

func TestRunParallelReadOnlyBatchIsFaster(t *testing.T) {
	const perTool = 120 * time.Millisecond
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			{ID: "1", Name: "read_a"}, {ID: "2", Name: "read_b"}, {ID: "3", Name: "read_c"},
		}},
		{text: "三个都读完了。"},
	}}
	cfg := Config{
		Model:         "mock",
		Tools:         []Tool{sleepTool("read_a", perTool), sleepTool("read_b", perTool), sleepTool("read_c", perTool)},
		Parallel:      true,
		ParallelMax:   3,
		ParallelTools: map[string]bool{"read_a": true, "read_b": true, "read_c": true},
	}

	start := time.Now()
	evs, err := runScripted(t, context.Background(), m, cfg, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	// 串行下限是 3×120ms=360ms；并发应明显低于它。
	if elapsed >= 3*perTool-time.Duration(float64(perTool)*0.35) {
		t.Fatalf("并发没有生效：耗时 %v，串行下限 %v", elapsed, 3*perTool)
	}
	// 事件顺序必须与输入顺序一致（并发只影响执行，不影响呈现）。
	if got, want := resultIDs(evs), []string{"1", "2", "3"}; !equalSeq(got, want) {
		t.Fatalf("事件顺序错乱：got=%v want=%v", got, want)
	}
}

func TestRunParallelOffStaysSerial(t *testing.T) {
	const perTool = 60 * time.Millisecond
	tools := []Tool{sleepTool("read_a", perTool), sleepTool("read_b", perTool)}
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{{ID: "1", Name: "read_a"}, {ID: "2", Name: "read_b"}}},
		{text: "完成。"},
	}}
	cfg := Config{
		Model: "mock", Tools: tools,
		Parallel:      false, // 默认关闭：与改造前完全一致
		ParallelTools: map[string]bool{"read_a": true, "read_b": true},
	}
	start := time.Now()
	evs, err := runScripted(t, context.Background(), m, cfg, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if elapsed < 2*perTool-10*time.Millisecond {
		t.Fatalf("开关关闭却并发了：耗时 %v", elapsed)
	}
	if got, want := resultIDs(evs), []string{"1", "2"}; !equalSeq(got, want) {
		t.Fatalf("事件顺序错乱：got=%v want=%v", got, want)
	}
}

func TestRunParallelSplitsAtWriteTool(t *testing.T) {
	const perTool = 80 * time.Millisecond
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			{ID: "1", Name: "read_a"}, {ID: "2", Name: "read_b"},
			{ID: "3", Name: "write"},
			{ID: "4", Name: "read_c"},
		}},
		// write 属于改动类工具：循环会在收尾时多要一轮确认/验证，脚本要给足。
		{text: "完成。"},
		{text: "完成。"},
	}}
	cfg := Config{
		Model: "mock",
		Tools: []Tool{
			sleepTool("read_a", perTool), sleepTool("read_b", perTool),
			sleepTool("write", perTool), sleepTool("read_c", perTool),
		},
		Parallel:      true,
		ParallelTools: map[string]bool{"read_a": true, "read_b": true, "read_c": true}, // write 不在白名单
	}
	start := time.Now()
	evs, err := runScripted(t, context.Background(), m, cfg, nil)
	elapsed := time.Since(start)
	if err != nil {
		for r := 0; r < m.callCount(); r++ {
			roles := make([]string, 0, len(m.messagesAt(r)))
			for _, msg := range m.messagesAt(r) {
				roles = append(roles, string(msg.Role))
			}
			t.Logf("第 %d 次模型调用看到 %d 条消息：%v", r+1, len(roles), roles)
		}
		t.Fatalf("Run 失败：%v（模型共调用 %d 次，脚本 %d 步）", err, m.callCount(), len(m.steps))
	}
	// 读-读并发，write 单独，read_c 单独：总耗时约 2×80ms，明显小于 4×80ms。
	if elapsed >= 4*perTool-time.Duration(float64(perTool)*0.35) {
		t.Fatalf("批次没有按写工具切开：耗时 %v", elapsed)
	}
	if got, want := resultIDs(evs), []string{"1", "2", "3", "4"}; !equalSeq(got, want) {
		t.Fatalf("事件顺序错乱：got=%v want=%v", got, want)
	}
}

// 并行批里一个工具报错不能拖垮同批其它工具，错误仍按 is_error 回灌给模型。
func TestRunParallelBatchErrorIsIsolated(t *testing.T) {
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{{ID: "1", Name: "read_ok"}, {ID: "2", Name: "read_bad"}}},
		{text: "一个失败，我换个办法。"},
	}}
	bad := &fakeTool{name: "read_bad", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		return Result{IsError: true, Content: "读失败：文件不存在"}, nil
	}}
	cfg := Config{
		Model:         "mock",
		Tools:         []Tool{sleepTool("read_ok", 20*time.Millisecond), bad},
		Parallel:      true,
		ParallelTools: map[string]bool{"read_ok": true, "read_bad": true},
	}
	evs, err := runScripted(t, context.Background(), m, cfg, nil)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	var okCount, errCount int
	for _, ev := range evs {
		if ev.Kind != EvToolResult {
			continue
		}
		if ev.OK {
			okCount++
		} else {
			errCount++
		}
	}
	if okCount != 1 || errCount != 1 {
		t.Fatalf("结果计数不对：ok=%d err=%d", okCount, errCount)
	}
}

// planCallGroups 是纯函数，单独覆盖分组边界（空序列、全读、全写、读写混合）。
func TestPlanCallGroups(t *testing.T) {
	cfg := Config{Parallel: true, ParallelTools: map[string]bool{"r": true}}
	call := func(name string) llm.ToolCall { return llm.ToolCall{Name: name} }

	cases := []struct {
		name  string
		calls []llm.ToolCall
		want  [][]int
	}{
		{"空", nil, nil},
		{"全写", []llm.ToolCall{call("w1"), call("w2")}, [][]int{{0}, {1}}},
		{"全读", []llm.ToolCall{call("r"), call("r")}, [][]int{{0, 1}}},
		{"读写混合", []llm.ToolCall{call("r"), call("w"), call("r")}, [][]int{{0}, {1}, {2}}},
		{"读尾批", []llm.ToolCall{call("w"), call("r"), call("r")}, [][]int{{0}, {1, 2}}},
	}
	for _, tc := range cases {
		got := planCallGroups(cfg, tc.calls)
		if len(got) != len(tc.want) {
			t.Fatalf("%s：组数 = %d，想要 %d（%v）", tc.name, len(got), len(tc.want), got)
		}
		for i := range got {
			if len(got[i]) != len(tc.want[i]) {
				t.Fatalf("%s：第 %d 组长度 = %d，想要 %d", tc.name, i, len(got[i]), len(tc.want[i]))
			}
			for j := range got[i] {
				if got[i][j] != tc.want[i][j] {
					t.Fatalf("%s：第 %d 组 = %v，想要 %v", tc.name, i, got[i], tc.want[i])
				}
			}
		}
	}

	// 开关关闭时任何调用都单独成组（走原串行路径）。
	off := Config{}
	for i, g := range planCallGroups(off, []llm.ToolCall{call("r"), call("r")}) {
		if len(g) != 1 {
			t.Fatalf("开关关闭时第 %d 组应只有 1 个调用，实际 %v", i, g)
		}
	}
}

// 回归（逻辑官审查 P1）：并发批里的读必须算「验证过」。
//
// 场景：写一次 → 同一轮并发读回两个文件。若记账丢失，收尾会误判「改动未验证」，
// 多烧一轮让模型解释「这一步没有验证」——但模型其实验了，用户拿到假阴性。
func TestParallelBatchRecordsVerification(t *testing.T) {
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{{ID: "1", Name: "write"}}},
		{calls: []llm.ToolCall{{ID: "2", Name: "read"}, {ID: "3", Name: "read"}}},
		{text: "改完并验证了。"},
		{text: "改完并验证了。"}, // 只有记账丢失时才会被消耗到
	}}
	cfg := Config{
		Model:         "mock",
		Parallel:      true,
		ParallelTools: map[string]bool{"read": true},
		Tools:         []Tool{sleepTool("write", time.Millisecond), sleepTool("read", time.Millisecond)},
	}
	evs, err := runScripted(t, context.Background(), m, cfg, nil)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if got := m.callCount(); got != 3 {
		t.Fatalf("模型调用 %d 次：验证记账丢失（应为 3 次）", got)
	}
	if got, want := resultIDs(evs), []string{"1", "2", "3"}; !equalSeq(got, want) {
		t.Fatalf("事件顺序错乱：got=%v want=%v", got, want)
	}
}

// 回归（逻辑官审查 P1）：被预算拒绝的调用**不许执行**。
//
// 三连读、工具上限 2：串行路径只执行 2 次；批路径如果先把整批跑完再覆盖结果，
// 副作用就已经发生了，而模型收到的是「没有执行」——假话。
func TestParallelBudgetRejectionDoesNotExecute(t *testing.T) {
	var hits int32
	counting := &fakeTool{name: "read", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		atomic.AddInt32(&hits, 1)
		return Result{Content: "ok"}, nil
	}}
	m := &fakeModel{steps: []scriptStep{
		{calls: []llm.ToolCall{{ID: "1", Name: "read"}, {ID: "2", Name: "read"}, {ID: "3", Name: "read"}}},
		{text: "到上限了，我先收尾。"},
	}}
	cfg := Config{
		Model:               "mock",
		Parallel:            true,
		ParallelTools:       map[string]bool{"read": true},
		Tools:               []Tool{counting},
		MaxToolCalls:        1,
		LimitRecoveryRounds: 1, // maxTools = 1*(1+1) = 2
	}
	_, err := runScripted(t, context.Background(), m, cfg, nil)
	if err == nil {
		t.Fatal("触顶应返回错误")
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("实际执行 %d 次，期望 2 次（被预算拒绝的调用不许执行）", got)
	}
}
