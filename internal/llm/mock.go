package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	einoschema "github.com/cloudwego/eino/schema"
)

const (
	mockChunkRunes = 3                     // 每次 delta 吐 3 个 rune
	mockChunkDelay = 30 * time.Millisecond // 块间隔，模拟打字机效果
	mockArgSplit   = 10                    // arguments 长于这个 rune 数就切成两片，复现"分片到达"
)

// MockTurn 描述 mock 后端在「第 N 轮」的确定行为，供无真实模型时确定性测试工具循环。
//
// 「轮」= 本次输入里已经携带 ToolCalls 的 assistant 消息条数：
//
//	第 1 次请求 messages=[user]                          -> round 0
//	第 2 次请求 messages=[user, assistant(tool_calls), tool] -> round 1
//
// 脚本用完（round 越界）时回落到默认回显，方便"最后一轮交给默认行为"。
// 脚本只在工具模式下生效（请求带 Tools，或实例被 WithTools 绑过工具），
// 以免污染既有的纯文本用例。
type MockTurn struct {
	// ToolCalls 非空 -> 本轮返回这些工具调用（可多条），不发文本。
	ToolCalls []ToolCall
	// Text 非空 -> 本轮返回该文本（仍按 mockChunkRunes 分块流式发出）。
	Text string
	// TextBeforeToolCalls 模拟先输出一段文字、再输出工具调用的供应商流。
	TextBeforeToolCalls bool
	// Err 非 nil -> 本轮把错误直接交给读端，用来测错误路径。
	Err error
}

var mockScriptState struct {
	sync.RWMutex
	turns []MockTurn
}

// SetMockScript 设置 mock 后端各轮的确定行为（进程级）。传 nil 等价于清空。
func SetMockScript(turns []MockTurn) {
	mockScriptState.Lock()
	defer mockScriptState.Unlock()
	mockScriptState.turns = append([]MockTurn(nil), turns...)
}

// ClearMockScript 清空脚本：mock 回到"复读最后一条 user 消息"的默认行为。
func ClearMockScript() { SetMockScript(nil) }

// MockScript 返回当前脚本的副本。
func MockScript() []MockTurn {
	mockScriptState.RLock()
	defer mockScriptState.RUnlock()
	return append([]MockTurn(nil), mockScriptState.turns...)
}

// mockChatModel 不需要任何 key，把最后一条 user 消息分块回显，
// 用于在没有真实凭证时验证整条流式链路。
//
// 设了脚本且处于工具模式时，改为按轮次返回脚本内容——工具调用也在这里造，
// 这是全队"没有真实模型也能确定性测工具循环"的测试缝。
type mockChatModel struct {
	model string
	// tools 是 WithTools 绑上来的工具集（不可变语义：返回副本，不动原实例）。
	tools []*einoschema.ToolInfo
	// reqTools 记录构造它的那次请求是否带了 Tools。
	reqTools bool
	// script 在构造时快照，流跑到一半有人改脚本也不会把本轮撕开。
	script []MockTurn
}

// 编译期确认实现了 Eino 的 BaseChatModel 与 ToolCallingChatModel。
var (
	_ einomodel.BaseChatModel        = (*mockChatModel)(nil)
	_ einomodel.ToolCallingChatModel = (*mockChatModel)(nil)
)

// NewMockBackend 构造 mock 后端。mock 端点无需 key，apiKey 忽略。
func NewMockBackend(_ context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	model := req.Model
	if model == "" && len(ep.Models) > 0 {
		model = ep.Models[0]
	}
	return &mockChatModel{
		model:    model,
		reqTools: len(req.Tools) > 0,
		script:   MockScript(),
	}, nil
}

// WithTools 返回绑好工具的新实例（不可变）：原实例的 tools 仍为空。
// 语义与 Eino 各真实后端（openai/claude/gemini/ollama/ark）的 WithTools 一致。
func (m *mockChatModel) WithTools(tools []*einoschema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	cp := *m
	cp.tools = append([]*einoschema.ToolInfo(nil), tools...)
	return &cp, nil
}

// toolsBound 表示当前实例处于工具模式（脚本生效的前提）。
func (m *mockChatModel) toolsBound() bool { return m.reqTools || len(m.tools) > 0 }

// turn 按已完成的工具轮数取脚本；没有脚本或不在工具模式时返回 nil（走默认回显）。
func (m *mockChatModel) turn(input []*einoschema.Message) *MockTurn {
	if len(m.script) == 0 || !m.toolsBound() {
		return nil
	}
	round := 0
	for _, msg := range input {
		if msg.Role == einoschema.Assistant && len(msg.ToolCalls) > 0 {
			round++
		}
	}
	if round >= len(m.script) {
		return nil
	}
	t := m.script[round]
	return &t
}

func (m *mockChatModel) Generate(_ context.Context, input []*einoschema.Message, _ ...einomodel.Option) (*einoschema.Message, error) {
	if t := m.turn(input); t != nil {
		if t.Err != nil {
			return nil, t.Err
		}
		return &einoschema.Message{
			Role:      einoschema.Assistant,
			Content:   t.Text,
			ToolCalls: mockSchemaToolCalls(t.ToolCalls),
		}, nil
	}
	return &einoschema.Message{Role: einoschema.Assistant, Content: m.echo(input)}, nil
}

// Stream 用 schema.Pipe 构造 StreamReader：起 goroutine 往 writer 分块写，
// 读端由调用方关闭；ctx 取消或读端提前关闭时 goroutine 立即退出，不泄漏。
func (m *mockChatModel) Stream(ctx context.Context, input []*einoschema.Message, _ ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	reader, writer := einoschema.Pipe[*einoschema.Message](1)
	turn := m.turn(input)

	go func() {
		defer writer.Close()

		if turn != nil {
			if turn.Err != nil {
				writer.Send(nil, turn.Err)
				return
			}
			if turn.TextBeforeToolCalls && turn.Text != "" {
				m.sendText(ctx, writer, turn.Text)
			}
			// 工具调用按 Index 分两片发出：正好用来验证上层"按 Index 归并"。
			for i := range turn.ToolCalls {
				idx := i
				tc := turn.ToolCalls[i]
				head, tail := splitMockArgs(tc.Arguments)
				if writer.Send(&einoschema.Message{
					Role: einoschema.Assistant,
					ToolCalls: []einoschema.ToolCall{{
						Index: &idx,
						ID:    tc.ID,
						Type:  "function",
						Function: einoschema.FunctionCall{
							Name:      tc.Name,
							Arguments: head,
						},
					}},
				}, nil) {
					return
				}
				if tail == "" {
					continue
				}
				if writer.Send(&einoschema.Message{
					Role: einoschema.Assistant,
					ToolCalls: []einoschema.ToolCall{{
						Index: &idx,
						Function: einoschema.FunctionCall{
							Arguments: tail,
						},
					}},
				}, nil) {
					return
				}
			}
			if turn.Text != "" && !turn.TextBeforeToolCalls {
				m.sendText(ctx, writer, turn.Text)
			}
			return
		}

		m.sendText(ctx, writer, m.echo(input))
	}()
	return reader, nil
}

// sendText 按 mockChunkRunes 分块把文本写进 writer，块间 sleep 模拟打字机。
func (m *mockChatModel) sendText(ctx context.Context, writer *einoschema.StreamWriter[*einoschema.Message], text string) {
	runes := []rune(text)
	for start := 0; start < len(runes); start += mockChunkRunes {
		end := start + mockChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		// 读端已关闭时 Send 返回 true，立即退出。
		if writer.Send(&einoschema.Message{
			Role:    einoschema.Assistant,
			Content: string(runes[start:end]),
		}, nil) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(mockChunkDelay):
		}
	}
}

// splitMockArgs 把一段 arguments 切成两片；太短就不切（第二片为空）。
func splitMockArgs(args string) (string, string) {
	r := []rune(args)
	if len(r) < mockArgSplit {
		return args, ""
	}
	mid := len(r) / 2
	return string(r[:mid]), string(r[mid:])
}

// mockSchemaToolCalls 把本包的 ToolCall 转成 eino 的完整 tool_call（非流式用，不带 Index）。
func mockSchemaToolCalls(calls []ToolCall) []einoschema.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]einoschema.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, einoschema.ToolCall{
			ID:   c.ID,
			Type: "function",
			Function: einoschema.FunctionCall{
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		})
	}
	return out
}

// echo 取最后一条 user 消息拼出回显文案。
func (m *mockChatModel) echo(input []*einoschema.Message) string {
	var lastUser string
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == RoleUser {
			lastUser = input[i].Content
			break
		}
	}
	model := m.model
	if model == "" {
		model = "mock"
	}
	return fmt.Sprintf("[mock:%s] 你说的是：%s", model, lastUser)
}
