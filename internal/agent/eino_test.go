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
