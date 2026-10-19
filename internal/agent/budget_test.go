package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"blog-my/internal/llm"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type budgetObservation struct {
	requests   [][]llm.Message
	toolCounts []int
	reportErr  error
}

// Record actual Eino model requests, including the separate tool-free report.
type observedBudgetModel struct {
	einomodel.ToolCallingChatModel
	observation *budgetObservation
	toolCount   int
}

func (m *observedBudgetModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	bound, err := m.ToolCallingChatModel.WithTools(tools)
	return &observedBudgetModel{ToolCallingChatModel: bound, observation: m.observation, toolCount: len(tools)}, err
}

func (m *observedBudgetModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	m.observation.requests = append(m.observation.requests, schemaMessagesToLLM(input))
	m.observation.toolCounts = append(m.observation.toolCounts, m.toolCount)
	if m.toolCount == 0 {
		if m.observation.reportErr != nil {
			return nil, m.observation.reportErr
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "目前已读取文件，剩余任务尚未完成。"}}), nil
	}
	return m.ToolCallingChatModel.Stream(ctx, input, opts...)
}

func runBudgetScenario(t *testing.T, engine string, cfg Config, steps []scriptStep, reportErr error) ([]Event, error, *budgetObservation) {
	t.Helper()
	history := []llm.Message{{Role: llm.RoleUser, Content: "检查文件直到完成"}}
	o := &budgetObservation{reportErr: reportErr}
	if engine == "legacy" {
		m := &fakeModel{steps: append(append([]scriptStep(nil), steps...), scriptStep{text: "目前已读取文件，剩余任务尚未完成。", err: reportErr})}
		events, err := runScripted(t, context.Background(), m, cfg, history)
		for _, req := range m.reqs {
			o.requests = append(o.requests, req.Messages)
			o.toolCounts = append(o.toolCounts, len(req.Tools))
		}
		return events, err, o
	}
	turns := make([]llm.MockTurn, 0, len(steps))
	for _, step := range steps {
		turns = append(turns, llm.MockTurn{Text: step.text, ToolCalls: step.calls, Err: step.err})
	}
	cfg.EinoModel = &observedBudgetModel{ToolCallingChatModel: einoMockModel(t, turns), observation: o}
	var events []Event
	err := Run(context.Background(), cfg, history, func(ev Event) { events = append(events, ev) })
	return events, err, o
}

func budgetRead(id string) scriptStep {
	return scriptStep{calls: oneCall(id, "read", `{"file_path":"`+id+`.txt"}`)}
}

func TestBudgetAutomaticallyReplansAndCompletes(t *testing.T) {
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			tool := &fakeTool{name: "read"}
			events, err, observation := runBudgetScenario(t, engine, Config{Model: "mock-echo", Tools: []Tool{tool},
				MaxIterations: 1, MaxToolCalls: 1, LimitRecoveryRounds: 2}, []scriptStep{budgetRead("a"), budgetRead("b"), {text: "检查完成。"}}, nil)
			if err != nil || tool.count() != 2 || countKind(events, EvDone) != 1 || countKind(events, EvError) != 0 {
				t.Fatalf("run: err=%v, tools=%d, events=%v", err, tool.count(), kindSeq(events))
			}
			if len(observation.requests) != 3 {
				t.Fatalf("model calls=%d", len(observation.requests))
			}
			var replan bool
			for _, msgs := range observation.requests {
				for _, msg := range msgs {
					if strings.Contains(msg.Content, "自动续行与重新规划") {
						replan = true
					}
				}
			}
			if !replan {
				t.Fatal("the model never received a revised planning instruction")
			}
		})
	}
}

func TestBudgetHardLimitReportsWithoutReplayingTools(t *testing.T) {
	for _, engine := range []string{"legacy", "eino"} {
		for _, reason := range []string{"iterations", "tools", "report_failure"} {
			t.Run(engine+"/"+reason, func(t *testing.T) {
				tool := &fakeTool{name: "read"}
				cfg := Config{Model: "mock-echo", Tools: []Tool{tool}, MaxIterations: 1, MaxToolCalls: 8, LimitRecoveryRounds: 1}
				steps := []scriptStep{budgetRead("a"), budgetRead("b")}
				wantErr := ErrMaxIterations
				var reportErr error
				if reason == "tools" {
					cfg.MaxIterations, cfg.MaxToolCalls = 5, 1
					steps = []scriptStep{{calls: append(append(budgetRead("a").calls, budgetRead("b").calls...), budgetRead("c").calls...)}}
					wantErr = ErrMaxToolCalls
				}
				if reason == "report_failure" {
					reportErr = errors.New("report unavailable")
				}
				events, err, o := runBudgetScenario(t, engine, cfg, steps, reportErr)
				var reported *LimitReportError
				if !errors.As(err, &reported) || !errors.Is(err, wantErr) {
					t.Fatalf("incomplete outcome lost: %v", err)
				}
				if tool.count() != 2 {
					t.Fatalf("tools executed %d times, want 2", tool.count())
				}
				if countKind(events, EvError) != 0 || countKind(events, EvDone) != 0 {
					t.Fatalf("unexpected completion/error events: %v", kindSeq(events))
				}
				last := len(o.requests) - 1
				if o.toolCounts[last] != 0 {
					t.Fatal("final report still has tools")
				}
				// Every requested tool must have exactly one result, even an unexecuted
				// third tool in the same batch. This is required by real provider APIs.
				calls, results := map[string]int{}, map[string]int{}
				for _, msg := range o.requests[last] {
					for _, call := range msg.ToolCalls {
						calls[call.ID]++
					}
					if msg.Role == roleTool {
						results[msg.ToolCallID]++
					}
				}
				for id, n := range calls {
					if n != 1 || results[id] != 1 {
						t.Fatalf("unpaired call %s: calls=%d results=%d", id, n, results[id])
					}
				}
				if len(calls) < 2 {
					t.Fatal("tool history lost during final report")
				}
				var output strings.Builder
				for _, event := range events {
					if event.Kind == EvDelta {
						output.WriteString(event.Content)
					}
				}
				if !strings.Contains(output.String(), "执行上限") {
					t.Fatal("missing readable progress report")
				}
				if reportErr != nil && !strings.Contains(output.String(), "工具成功 2 次") {
					t.Fatal("missing fallback execution facts")
				}
			})
		}
	}
}

func TestBudgetContinuationRespectsCancellation(t *testing.T) {
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			n := 0
			tool := &fakeTool{name: "read", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
				n++
				if n == 2 {
					cancel()
				}
				return Result{Content: "ok"}, nil
			}}
			cfg := Config{Model: "mock-echo", Tools: []Tool{tool}, MaxIterations: 1, MaxToolCalls: 8, LimitRecoveryRounds: 2}
			var err error
			if engine == "legacy" {
				_, err = runScripted(t, ctx, &fakeModel{steps: []scriptStep{budgetRead("a"), budgetRead("b"), {text: "不应调用"}}}, cfg, nil)
			} else {
				cfg.EinoModel = einoMockModel(t, []llm.MockTurn{{ToolCalls: budgetRead("a").calls}, {ToolCalls: budgetRead("b").calls}, {Text: "不应调用"}})
				err = Run(ctx, cfg, nil, nil)
			}
			if !errors.Is(err, context.Canceled) || n != 2 {
				t.Fatalf("cancellation: err=%v tools=%d", err, n)
			}
		})
	}
}

func TestBudgetPreservesUnknownOutcomeAcrossContinuation(t *testing.T) {
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			deletion := &fakeTool{name: "delete", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
				return Result{IsError: true, Content: "timeout: deletion outcome unknown"}, nil
			}}
			move, read := &fakeTool{name: "move"}, &fakeTool{name: "read"}
			steps := []scriptStep{
				{calls: []llm.ToolCall{{ID: "delete-1", Name: "delete", Arguments: `{}`}, {ID: "move-1", Name: "move", Arguments: `{"from_path":"a"}`}, {ID: "read-1", Name: "read", Arguments: `{}`}}},
				{calls: []llm.ToolCall{{ID: "move-2", Name: "move", Arguments: `{"from_path":"b"}`}, {ID: "read-2", Name: "read", Arguments: `{}`}}},
				{text: "删除结果未知，后续移动没有执行。"},
				{text: "删除结果未知，后续移动没有执行。"},
			}
			_, err, _ := runBudgetScenario(t, engine, Config{Model: "mock-echo", Tools: []Tool{deletion, move, read}, MaxIterations: 1, LimitRecoveryRounds: 4}, steps, nil)
			if err != nil {
				t.Fatal(err)
			}
			if deletion.count() != 1 || move.count() != 0 || read.count() != 2 {
				t.Fatalf("unknown-outcome protection lost: delete=%d move=%d read=%d", deletion.count(), move.count(), read.count())
			}
		})
	}
}
