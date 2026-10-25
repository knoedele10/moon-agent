package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"blog-my/internal/llm"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func einoMockModel(t *testing.T, turns []llm.MockTurn) einomodel.ToolCallingChatModel {
	t.Helper()
	llm.SetMockScript(turns)
	t.Cleanup(llm.ClearMockScript)
	base, err := llm.NewMockBackend(context.Background(), llm.Endpoint{ID: "mock", Models: []string{"mock-echo"}}, "",
		llm.ChatRequest{Model: "mock-echo"})
	if err != nil {
		t.Fatal(err)
	}
	return base.(einomodel.ToolCallingChatModel)
}

func TestEinoReActToolEvents(t *testing.T) {
	model := einoMockModel(t, []llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Arguments: `{"file_path":"a.txt"}`}}},
		{Text: "文件已经读取。"},
	})
	read := &fakeTool{name: "read", exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
		return Result{Content: "hello", Display: map[string]any{"summary": "读取成功"}}, nil
	}}
	var events []Event
	err := Run(context.Background(), Config{Model: "mock-echo", EinoModel: model, Tools: []Tool{read}},
		[]llm.Message{{Role: llm.RoleUser, Content: "读取文件"}}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if read.count() != 1 {
		t.Fatalf("工具执行次数 = %d", read.count())
	}
	if got := read.argAt(0); got != `{"file_path":"a.txt"}` {
		t.Fatalf("工具参数分片未完整归并：%q", got)
	}
	if got := fmt.Sprint(kindSeq(events)); got != "[tool_call tool_result delta delta delta done]" {
		t.Fatalf("Eino 事件序列 = %s", got)
	}
	result := onlyResult(t, events)
	if result.ToolID != "call-1" || result.ToolName != "read" || !result.OK || result.Detail != "hello" {
		t.Fatalf("工具结果丢字段：%+v", result)
	}
}

func TestEinoReActStreamsBeforeModelCompletes(t *testing.T) {
	model := einoMockModel(t, []llm.MockTurn{{Text: strings.Repeat("流", 60)}})
	deltas := make(chan string, 32)
	done := make(chan error, 1)
	var streamed strings.Builder
	var doneText string
	go func() {
		done <- Run(context.Background(), Config{Model: "mock-echo", EinoModel: model},
			[]llm.Message{{Role: llm.RoleUser, Content: "继续"}}, func(ev Event) {
				if ev.Kind == EvDelta {
					streamed.WriteString(ev.Content)
					deltas <- ev.Content
				} else if ev.Kind == EvDone {
					doneText = ev.Content
				}
			})
	}()

	select {
	case delta := <-deltas:
		if delta == "" {
			t.Fatal("收到空的首段输出")
		}
	case err := <-done:
		t.Fatalf("模型完成前没有产生 delta，Run 结果: %v", err)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("250ms 内没有收到首段 delta")
	}

	select {
	case err := <-done:
		t.Fatalf("首段 delta 到达时整轮已经完成，Run 结果: %v", err)
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("流式运行没有在 2 秒内结束")
	}
	if got := streamed.String(); got != doneText {
		t.Fatalf("普通文本的流式输出和最终内容不一致：stream=%q, done=%q", got, doneText)
	}
}

func TestEinoReActStreamsPreambleAndStillCallsTool(t *testing.T) {
	const preamble = "我先检查一下。"
	const answer = "已经完成。"
	model := einoMockModel(t, []llm.MockTurn{
		{
			Text:                preamble,
			TextBeforeToolCalls: true,
			ToolCalls:           []llm.ToolCall{{ID: "late-call", Name: "read", Arguments: `{"file_path":"a.txt"}`}},
		},
		{Text: answer},
	})
	read := &fakeTool{name: "read", exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
		return Result{Content: "文件内容"}, nil
	}}
	var events []Event
	err := Run(context.Background(), Config{Model: "mock-echo", EinoModel: model, Tools: []Tool{read}},
		[]llm.Message{{Role: llm.RoleUser, Content: "读取文件"}}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if read.count() != 1 {
		t.Fatalf("先输出文字后仍应执行工具，执行次数 = %d；事件：%+v", read.count(), events)
	}

	firstDelta, toolCall, toolResult, lastDelta := -1, -1, -1, -1
	var streamed strings.Builder
	var doneContent string
	for i, ev := range events {
		switch ev.Kind {
		case EvDelta:
			if firstDelta < 0 {
				firstDelta = i
			}
			lastDelta = i
			streamed.WriteString(ev.Content)
		case EvToolCall:
			toolCall = i
		case EvToolResult:
			toolResult = i
		case EvDone:
			doneContent = ev.Content
		}
	}
	if firstDelta < 0 || toolCall <= firstDelta || toolResult <= toolCall || lastDelta <= toolResult {
		t.Fatalf("应先流出说明，再调用工具，最后流出结果：%+v", events)
	}
	if got, want := streamed.String(), preamble+answer; got != want {
		t.Fatalf("流式正文重复或丢失：got %q, want %q", got, want)
	}
	if got, want := doneContent, preamble+answer; got != want {
		t.Fatalf("最终答复与流式正文不一致：got %q, want %q", got, want)
	}
}

func TestEinoReActAskUserStopsWithoutSecondModelTurn(t *testing.T) {
	model := einoMockModel(t, []llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "ask-1", Name: "ask_user", Arguments: `{"question":"继续吗"}`}}},
		{Text: "不应由模型代答"},
	})
	ask := &fakeTool{name: "ask_user", exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
		return Result{Content: "等待用户回答", AwaitUser: "继续吗？", Display: map[string]any{"options": []string{"继续", "取消"}}}, nil
	}}
	var events []Event
	err := Run(context.Background(), Config{Model: "mock-echo", EinoModel: model, Tools: []Tool{ask}},
		[]llm.Message{{Role: llm.RoleUser, Content: "开始"}}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if ask.count() != 1 {
		t.Fatalf("提问工具执行次数 = %d", ask.count())
	}
	if got := fmt.Sprint(kindSeq(events)); got != "[tool_call tool_result need_input]" {
		t.Fatalf("等待用户的事件序列 = %s", got)
	}
}

// 回归：非幂等工具超时后，同一指纹的再次调用必须被拦住，不能重放副作用。
// decideRetry 对非幂等调用不重试，所以真实执行应停在第一次。
func TestEinoUncertainEffectBlocksReplay(t *testing.T) {
	const args = `{"command":"echo hi"}`
	model := einoMockModel(t, []llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "cmd-1", Name: "run_command", Arguments: args}}},
		{ToolCalls: []llm.ToolCall{{ID: "cmd-2", Name: "run_command", Arguments: args}}},
		{Text: "上一步结果未知，请你确认是否已经生效。"},
	})
	command := &fakeTool{name: "run_command", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		return Result{}, fmt.Errorf("timeout after request sent")
	}}
	var events []Event
	err := Run(context.Background(), Config{Model: "mock-echo", EinoModel: model, Tools: []Tool{command}},
		[]llm.Message{{Role: llm.RoleUser, Content: "执行命令"}}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if command.count() != 1 {
		t.Fatalf("非幂等命令实际执行 %d 次，超时后的同参重放不应再执行", command.count())
	}

	var calls, results []Event
	done := 0
	for _, ev := range events {
		switch ev.Kind {
		case EvToolCall:
			calls = append(calls, ev)
		case EvToolResult:
			results = append(results, ev)
		case EvDone:
			done++
		}
	}
	if len(calls) != 2 || len(results) != 2 || done != 1 {
		t.Fatalf("事件不完整：calls=%d results=%d done=%d 序列=%s", len(calls), len(results), done, kindSeq(events))
	}
	if calls[0].ToolCall == nil || calls[1].ToolCall == nil || calls[0].ToolCall.Arguments != calls[1].ToolCall.Arguments {
		t.Fatalf("两次调用应是同一参数指纹：%+v", calls)
	}
	if results[0].OK || !strings.Contains(results[0].Detail, "timeout") {
		t.Fatalf("第一次应是真实的超时失败：%+v", results[0])
	}
	if code, _ := results[0].Display["code"].(string); code == "NOT_EXECUTED_AFTER_UNCERTAIN" || code == "UNCERTAIN_EFFECT_BLOCKED" {
		t.Fatalf("第一次不应被未知副作用拦截：%+v", results[0])
	}
	code, _ := results[1].Display["code"].(string)
	if results[1].OK || (code != "NOT_EXECUTED_AFTER_UNCERTAIN" && code != "UNCERTAIN_EFFECT_BLOCKED") {
		t.Fatalf("同参重放应被拦住：%+v", results[1])
	}
	blocked := map[string]string{
		"NOT_EXECUTED_AFTER_UNCERTAIN": "前一个操作的结果未知，为避免重复副作用，这次调用没有执行。请先让用户确认上一步是否生效。",
		"UNCERTAIN_EFFECT_BLOCKED":     "这次调用与之前一次「结果未知」的操作完全相同（run_command），已拒绝重复执行。请先确认上一步是否生效，不要重试。",
	}
	if results[1].Detail != blocked[code] || results[1].ToolName != "run_command" {
		t.Fatalf("拦截文案与工具名不一致：%+v", results[1])
	}
}

// 回归：Eino 路径上 write 成功后再 read 成功，必须记入验证。
// 记账丢失时 runEino 会再注入 verificationFeedback，多烧模型轮次，并把收尾文本重复进 done。
func TestEinoMutationThenReadCountsAsVerified(t *testing.T) {
	const answer = "改完并读回确认了。"
	obs := &budgetObservation{}
	model := &observedBudgetModel{
		ToolCallingChatModel: einoMockModel(t, []llm.MockTurn{
			{ToolCalls: []llm.ToolCall{{ID: "w1", Name: "write", Arguments: `{"file_path":"a.txt","content":"hi"}`}}},
			{ToolCalls: []llm.ToolCall{{ID: "r1", Name: "read", Arguments: `{"file_path":"a.txt"}`}}},
			{Text: answer},
		}),
		observation: obs,
	}
	write := &fakeTool{name: "write", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		return Result{Content: "已写入 a.txt"}, nil
	}}
	read := &fakeTool{name: "read", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		return Result{Content: "hi"}, nil
	}}
	var events []Event
	err := Run(context.Background(), Config{Model: "mock-echo", EinoModel: model, Tools: []Tool{write, read}},
		[]llm.Message{{Role: llm.RoleUser, Content: "写入并确认"}}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if write.count() != 1 || read.count() != 1 {
		t.Fatalf("write=%d read=%d，期望各执行 1 次", write.count(), read.count())
	}
	if len(obs.requests) != 3 {
		t.Fatalf("模型调用 %d 次，验证记账正常时应为 3 次（丢失时会因验证提醒再跑模型）", len(obs.requests))
	}
	for i, msgs := range obs.requests {
		for _, msg := range msgs {
			if msg.Content == verificationFeedback() || strings.Contains(msg.Content, "你刚刚改动了文件") {
				t.Fatalf("第 %d 次模型调用收到验证提醒，read 没有记入 lastVerifySeq：%s", i, msg.Content)
			}
		}
	}
	last := obs.requests[len(obs.requests)-1]
	verified := false
	for _, msg := range last {
		if strings.Contains(msg.Content, "已调用核对工具") {
			verified = true
		}
	}
	if !verified {
		t.Fatal("收尾轮未见「已调用核对工具」，lastVerifySeq 没有在 read 之后更新")
	}

	var streamed strings.Builder
	var doneText string
	doneN, resultN := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case EvDelta:
			streamed.WriteString(ev.Content)
		case EvDone:
			doneN++
			doneText = ev.Content
		case EvToolResult:
			resultN++
			if !ev.OK {
				t.Fatalf("工具结果不应失败：%+v", ev)
			}
		}
	}
	if resultN != 2 || doneN != 1 {
		t.Fatalf("结果/完成事件数量不对：results=%d done=%d 序列=%s", resultN, doneN, kindSeq(events))
	}
	if streamed.String() != answer || doneText != answer {
		t.Fatalf("收尾被验证提醒多跑了一轮：stream=%q done=%q", streamed.String(), doneText)
	}
}

// Eino 路径的只读并发：三个 120ms 的只读调用应当重叠执行，而不是串行 360ms。
func TestEinoParallelReadOnlyIsFaster(t *testing.T) {
	const perTool = 120 * time.Millisecond
	model := einoMockModel(t, []llm.MockTurn{
		{ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "read_a"}, {ID: "2", Name: "read_b"}, {ID: "3", Name: "read_c"},
		}},
		{Text: "三个都读完了。"},
	})
	tools := []Tool{sleepTool("read_a", perTool), sleepTool("read_b", perTool), sleepTool("read_c", perTool)}
	var results int
	start := time.Now()
	err := Run(context.Background(), Config{
		Model: "mock-echo", EinoModel: model, Tools: tools,
		Parallel: true, ParallelMax: 3,
		ParallelTools: map[string]bool{"read_a": true, "read_b": true, "read_c": true},
	}, []llm.Message{{Role: llm.RoleUser, Content: "并行读三个"}}, func(ev Event) {
		if ev.Kind == EvToolResult {
			results++
		}
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if results != 3 {
		t.Fatalf("应收到 3 个工具结果，实际 %d", results)
	}
	if elapsed >= 3*perTool-time.Duration(float64(perTool)*0.35) {
		t.Fatalf("Eino 只读并发没有生效：耗时 %v（串行下限 %v）", elapsed, 3*perTool)
	}
}

// 写工具必须独占：它和只读调用在同一批里也不能重叠，否则 read 可能读到写前内容。
func TestEinoParallelWriteExcludesReads(t *testing.T) {
	const perTool = 120 * time.Millisecond
	model := einoMockModel(t, []llm.MockTurn{
		{ToolCalls: []llm.ToolCall{
			{ID: "w1", Name: "write"}, {ID: "r1", Name: "read"},
		}},
		{Text: "写入并读回。"},
	})
	write := sleepTool("write", perTool)
	read := sleepTool("read", perTool)
	start := time.Now()
	err := Run(context.Background(), Config{
		Model: "mock-echo", EinoModel: model, Tools: []Tool{write, read},
		Parallel: true, ParallelMax: 2,
		ParallelTools: map[string]bool{"read": true},
	}, []llm.Message{{Role: llm.RoleUser, Content: "写入并读回"}}, func(Event) {})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if elapsed < 2*perTool-20*time.Millisecond {
		t.Fatalf("写工具与只读调用不应重叠：耗时 %v，串行下限 %v", elapsed, 2*perTool)
	}
}

// Eino 的 MessageRewriter 每轮都要注入最新计划；旧的瞬态计划块必须被剔除，
// 否则长任务里会一份份堆进上下文。
func TestEinoRewriteInjectsLatestPlanOnly(t *testing.T) {
	ResetPlans()
	defer ResetPlans()
	if _, err := UpdatePlan("sess-plan", Plan{Steps: []PlanStep{
		{Step: "读代码", Status: PlanCompleted},
		{Step: "改实现", Status: PlanInProgress},
	}}); err != nil {
		t.Fatal(err)
	}
	state := &einoRunState{ex: &Exec{SessionID: "sess-plan"}, images: map[string]*ImagePart{}}
	in := []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.System, Content: planBlockPrefix + "\n旧计划：不该出现"},
	}
	out := state.rewrite(context.Background(), in)

	blocks := 0
	joined := ""
	for _, m := range out {
		if m == nil {
			continue
		}
		joined += m.Content
		if planInjected(m.Content) {
			blocks++
			if !strings.Contains(m.Content, "改实现") {
				t.Fatalf("注入的应是新计划：%q", m.Content)
			}
		}
	}
	if blocks != 1 {
		t.Fatalf("计划块应恰好一份，实际 %d；输出：%s", blocks, joined)
	}
	if strings.Contains(joined, "旧计划：不该出现") {
		t.Fatal("旧的瞬态计划块必须被剔除")
	}
}
