package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"blog-my/internal/llm"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Both engines consume the same sequential script. Unlike a round-count-based
// mock this also detects extra model calls caused by completion reminders.
type safetyScriptModel struct{ script *fakeModel }

func (m *safetyScriptModel) WithTools([]*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return m, nil
}

func (m *safetyScriptModel) Generate(ctx context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	ch, err := m.script.stream(ctx, "safety-test", "", llm.ChatRequest{Messages: schemaMessagesToLLM(input)})
	if err != nil {
		return nil, err
	}
	text, calls, err := drain(ctx, ch, nil)
	if err != nil {
		return nil, err
	}
	return llm.ToSchemaMessages([]llm.Message{{Role: llm.RoleAssistant, Content: text, ToolCalls: calls}})[0], nil
}

func (m *safetyScriptModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func runSafetyScript(t *testing.T, engine string, steps []scriptStep, tools ...Tool) ([]Event, *fakeModel) {
	t.Helper()
	m := &fakeModel{steps: steps}
	cfg := Config{Model: "safety-test", Tools: tools, MaxIterations: 12}
	if engine == "eino" {
		cfg.EinoModel = &safetyScriptModel{script: m}
	}
	events, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "完成任务"}})
	if err != nil {
		t.Fatal(err)
	}
	if countKind(events, EvDone) != 1 || countKind(events, EvError) != 0 {
		t.Fatalf("unexpected completion: %+v", events)
	}
	if got := m.callCount(); got != len(steps) {
		t.Fatalf("model calls = %d, want %d", got, len(steps))
	}
	return events, m
}

func TestToolSafetyAcrossEngines(t *testing.T) {
	call := func(name, id string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: name, Arguments: `{}`}
	}
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("read retries once", func(t *testing.T) {
				for _, recover := range []bool{false, true} {
					t.Run(fmt.Sprint(recover), func(t *testing.T) {
						attempts := 0
						read := &fakeTool{name: "read", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
							attempts++
							if attempts == 1 || !recover {
								return Result{}, fmt.Errorf("connection reset")
							}
							return Result{Content: "verified"}, nil
						}}
						events, _ := runSafetyScript(t, engine, []scriptStep{
							{calls: []llm.ToolCall{call("read", "read-1")}}, {text: "done"},
						}, read)
						if read.count() != 2 || onlyResult(t, events).OK != recover {
							t.Fatalf("read attempts=%d, events=%+v", read.count(), events)
						}
					})
				}
			})
			t.Run("uncertain command is never replayed", func(t *testing.T) {
				command := &fakeTool{name: "run_command", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
					return Result{}, fmt.Errorf("timeout after request sent")
				}}
				different := call("run_command", "different")
				different.Arguments = `{"program":"other"}`
				events, _ := runSafetyScript(t, engine, []scriptStep{
					{calls: []llm.ToolCall{call("run_command", "first"), different}},
					{calls: []llm.ToolCall{call("run_command", "replay")}},
					{text: "done"}, {text: "result unknown"},
				}, command)
				if command.count() != 1 {
					t.Fatalf("non-idempotent command ran %d times", command.count())
				}
				blocked := 0
				for _, ev := range events {
					if ev.Kind == EvToolResult && ev.Display["code"] == "NOT_EXECUTED_AFTER_UNCERTAIN" {
						blocked++
					}
				}
				if blocked != 2 {
					t.Fatalf("blocked=%d, want 2; events=%+v", blocked, events)
				}
			})
			t.Run("failed write has no verification obligation", func(t *testing.T) {
				write := &fakeTool{name: "write", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
					return Result{}, fmt.Errorf("permission denied")
				}}
				runSafetyScript(t, engine, []scriptStep{
					{calls: []llm.ToolCall{call("write", "write")}}, {text: "write failed"},
				}, write)
				if write.count() != 1 {
					t.Fatalf("write attempts=%d, want 1", write.count())
				}
			})
			t.Run("successful write then verification", func(t *testing.T) {
				write, read := &fakeTool{name: "write"}, &fakeTool{name: "read"}
				runSafetyScript(t, engine, []scriptStep{
					{calls: []llm.ToolCall{call("write", "write"), call("read", "verify")}}, {text: "verified"},
				}, write, read)
				if write.count() != 1 || read.count() != 1 {
					t.Fatalf("write=%d, verify=%d", write.count(), read.count())
				}
			})
			t.Run("unverified write requests verification", func(t *testing.T) {
				write, read := &fakeTool{name: "write"}, &fakeTool{name: "read"}
				_, model := runSafetyScript(t, engine, []scriptStep{
					{calls: []llm.ToolCall{call("write", "write")}}, {text: "done"},
					{calls: []llm.ToolCall{call("read", "verify")}}, {text: "verified"},
				}, write, read)
				found := false
				for _, msg := range model.messagesAt(2) {
					if msg.Role == llm.RoleSystem && msg.Content == verificationFeedback() {
						found = true
					}
				}
				if !found || write.count() != 1 || read.count() != 1 {
					t.Fatalf("verification reminder=%v, write=%d, read=%d", found, write.count(), read.count())
				}
			})
		})
	}
}
