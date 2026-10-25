// Package llm 基于 Eino（CloudWeGo 的 LLM 应用框架）统一各家大模型的调用方式。
//
// 分层设计：
//
//	Endpoint —— "一个可调用的服务端点"，带 BaseURL、默认 key 的环境变量名、模型清单。
//	            例如 deepseek、openai、kimi、qwen、zhipu、anthropic、gemini、ernie、ark、ollama、mock。
//	backend  —— Endpoint.Backend 决定由哪个 Eino ChatModel 组件发起请求：
//	            openai 组件（覆盖 DeepSeek / OpenAI / Kimi / 通义 / 智谱）、
//	            claude 组件、gemini 组件、ark（火山方舟，也兼容文心转换端点）、ollama、mock。
//
// 端点与后端分离，一个后端可以服务多个端点，同原先"端点/适配器"的分工一致。
package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	einoschema "github.com/cloudwego/eino/schema"
)

// Role 与 eino schema.RoleType 无缝对应。
type Role = string

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool 是工具结果的角色：携带 ToolCallID 把结果对回 assistant 的那次调用。
	RoleTool = "tool"
)

// Image 是一张随消息发给模型的图片。
// 本地 Ollama 只接受 base64 内联图片、不接受 URL，所以这里直接带原始字节。
type Image struct {
	MIME string
	Data []byte
}

// Message 是一条对话消息（保留项目自有的轻量结构，映射到 eino schema.Message）。
// Images 非空时按多模态消息发送：先文字、后图片。
type Message struct {
	Role    string  `json:"role"`
	Content string  `json:"content"`
	Images  []Image `json:"-"`

	// 以下三个字段是工具调用的纯追加扩展（契约 §2）。全为空时映射结果与改造前
	// 一字节不差（eino 侧都是 omitempty）。
	// ToolCalls 只在 role=assistant 上携带：模型决定要调哪几个工具。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 在 role=tool 时必填：对应 assistant 那条 tool_calls[i].id。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName 可选，日志与前端用它辨认结果来自哪个工具。
	ToolName string `json:"tool_name,omitempty"`
}

// ChatRequest 是一次对话请求。本项目只做流式。
type ChatRequest struct {
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int
	// Tools 是暴露给模型的工具清单（契约 §2）。为空时行为与改造前完全一致。
	Tools []ToolDef
}

type EventType string

const (
	EventDelta EventType = "delta" // 增量文本
	EventDone  EventType = "done"  // 正常结束
	EventError EventType = "error" // 出错，Err 非空
	// EventToolCall 携带一条**已按 Index 归并完整**的工具调用（契约 §2）。
	// 一轮里可以有多条；它们统一在流结束（EOF）时发出，不转发半截分片。
	EventToolCall EventType = "tool_call"
)

// Event 是流式响应的统一事件。
type Event struct {
	Type    EventType
	Content string
	// ToolCall 仅在 EventToolCall 时非空。
	ToolCall *ToolCall
	Err      error
	// Usage 只在 EventDone 上携带这一轮的 token 账；供应商没返回时为 nil。
	Usage *Usage
}

var (
	ErrNoBackend      = errors.New("llm: 没有匹配的模型后端")
	ErrNoEndpoint     = errors.New("llm: 端点不存在")
	ErrMissingKey     = errors.New("llm: 缺少 API Key")
	ErrModelNotSpecif = errors.New("llm: 未指定模型")
)

// Endpoint 是一个可调用的模型服务端点。
type Endpoint struct {
	ID      string   `json:"id"`       // 唯一标识，如 "deepseek"
	Label   string   `json:"label"`    // 显示名，如 "DeepSeek"
	Backend string   `json:"backend"`  // Eino 模型后端：openai | claude | gemini | ark | ollama | mock
	BaseURL string   `json:"base_url"` // API 基地址；mock / ollama 默认为空
	EnvKey  string   `json:"-"`        // 服务端默认 key 的环境变量名
	Models  []string `json:"models"`   // 可用模型 ID
}

// Registry 保存 endpoint 清单，并按 (endpoint, model, apiKey) 维度缓存 Eino ChatModel 实例。
type Registry struct {
	endpoints []Endpoint
	factories map[string]backendFactory
}

// backendFactory 按端点与请求参数构造一个 Eino ChatModel。
// 返回的实例只覆盖"连到哪个服务"，模型名等逐请求参数走调用方 Option。
type backendFactory func(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (model.BaseChatModel, error)

func NewRegistry() *Registry {
	return &Registry{
		factories: map[string]backendFactory{
			"ollama": NewOllamaBackend,
			"mock":   NewMockBackend,
		},
	}
}

// Register 注册一个 Eino 模型后端，以 backend 名为键。
func (r *Registry) Register(backend string, f backendFactory) {
	r.factories[backend] = f
}

// SetEndpoints 覆盖端点清单。
func (r *Registry) SetEndpoints(eps []Endpoint) {
	r.endpoints = eps
}

// Endpoints 返回全部端点。
func (r *Registry) Endpoints() []Endpoint {
	return r.endpoints
}

// Endpoint 按 ID 查找端点。
func (r *Registry) Endpoint(id string) (Endpoint, bool) {
	for _, ep := range r.endpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return Endpoint{}, false
}

// FindByModel 按模型 ID 反查端点。模型 ID 全局唯一，故第一个匹配即结果。
func (r *Registry) FindByModel(model string) (Endpoint, bool) {
	for _, ep := range r.endpoints {
		for _, m := range ep.Models {
			if m == model {
				return ep, true
			}
		}
	}
	return Endpoint{}, false
}

// FindBackend 按后端名查找工厂。
func (r *Registry) FindBackend(backend string) (backendFactory, bool) {
	f, ok := r.factories[backend]
	return f, ok
}

// NewToolCallingModel builds the Eino component used by the ReAct runtime.
// ReAct binds the per-run tool list itself, so this returns an unbound model.
func (r *Registry) NewToolCallingModel(ctx context.Context, modelID, apiKey string, req ChatRequest) (model.ToolCallingChatModel, error) {
	ep, ok := r.FindByModel(modelID)
	if !ok {
		return nil, ErrNoEndpoint
	}
	f, ok := r.FindBackend(ep.Backend)
	if !ok {
		return nil, ErrNoBackend
	}
	req.Model = modelID
	cm, err := f(ctx, ep, apiKey, req)
	if err != nil {
		return nil, err
	}
	tc, ok := cm.(model.ToolCallingChatModel)
	if !ok {
		return nil, ErrToolsUnsupported
	}
	return maybeCache(ep.Backend, tc), nil
}

// toSchemaMessages 把本包的轻量消息映射为 eino schema.Message。
//
// 带图片的消息会组装成 UserInputMultiContent —— Ollama 后端只在 multi-content
// 下才认图片。此时 Content 必须留空：go-openai 的 ChatCompletionMessage 不允许
// 同时设置 Content 与 MultiContent（openai 后端会把两者一起带上，MarshalJSON 直接报错），
// 文字改由 multi-content 的第一个 text part 承载；纯文本消息行为一字未变。
func toSchemaMessages(msgs []Message) []*einoschema.Message {
	out := make([]*einoschema.Message, 0, len(msgs))
	for _, m := range msgs {
		sm := &einoschema.Message{
			Role:       einoschema.RoleType(m.Role),
			ToolCallID: m.ToolCallID,
			ToolName:   m.ToolName,
		}
		if len(m.ToolCalls) > 0 {
			sm.ToolCalls = make([]einoschema.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				sm.ToolCalls = append(sm.ToolCalls, einoschema.ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: einoschema.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		if len(m.Images) == 0 {
			sm.Content = m.Content
			out = append(out, sm)
			continue
		}
		// 有图片时必须走 multi-content，且 Content 必须留空：
		// go-openai 的 ChatCompletionMessage 不允许同时设置 Content 与 MultiContent
		// （MarshalJSON 直接报错），而 openai 后端会把两者一起带上；
		// 文字改由第一个 part 承载，ollama 后端本来也只从 parts 里取文字。
		parts := make([]einoschema.MessageInputPart, 0, len(m.Images)+1)
		if m.Content != "" {
			parts = append(parts, einoschema.MessageInputPart{
				Type: einoschema.ChatMessagePartTypeText,
				Text: m.Content,
			})
		}
		for _, img := range m.Images {
			b64 := base64.StdEncoding.EncodeToString(img.Data)
			parts = append(parts, einoschema.MessageInputPart{
				Type: einoschema.ChatMessagePartTypeImageURL,
				Image: &einoschema.MessageInputImage{
					MessagePartCommon: einoschema.MessagePartCommon{
						Base64Data: &b64,
						MIMEType:   img.MIME,
					},
				},
			})
		}
		sm.UserInputMultiContent = parts
		out = append(out, sm)
	}
	return out
}

// ToSchemaMessages exposes the same conversion to the Eino ReAct runtime.
func ToSchemaMessages(msgs []Message) []*einoschema.Message { return toSchemaMessages(msgs) }

// Generate 是调用方的非流式入口：与 Stream 同源（同一端点、同一后端、同一套消息映射），
// 区别只在走 eino 的 Generate 而不是 Stream，直接拿完整回复文本。
//
// CUA 的模型代理按契约 §3 是「请求非流式 + 响应非流式」，用这条路径最直接：
// 上游不必支持 SSE（Stream 会强制向上游要 SSE，普通返回 JSON 的服务会读不出内容）。
func (r *Registry) Generate(ctx context.Context, modelID, apiKey string, req ChatRequest) (string, error) {
	ep, ok := r.FindByModel(modelID)
	if !ok {
		return "", ErrNoEndpoint
	}
	f, ok := r.FindBackend(ep.Backend)
	if !ok {
		return "", ErrNoBackend
	}
	req.Model = modelID

	cm, err := f(ctx, ep, apiKey, req)
	if err != nil {
		return "", err
	}
	// req.Tools 为空 → cm 原样返回，行为与改造前完全一致（CUA 依赖这条路径）。
	// req.Tools 非空 → 也走 WithTools（后端不支持则 ErrToolsUnsupported），
	// 但本函数**只返回文本 Content**：它没有携带 tool_calls 的返回位。
	// 需要工具调用请走 Stream —— 工具调用只从 Stream 出。
	cm, err = withTools(cm, req)
	if err != nil {
		return "", err
	}
	msg, err := cm.Generate(ctx, toSchemaMessages(req.Messages), CacheOptions(ep.Backend)...)
	if err != nil {
		return "", err
	}
	if msg == nil {
		return "", nil
	}
	return msg.Content, nil
}

// sendEvent 在 ctx 取消时放弃投递，避免调用方停止读取后 goroutine 永久阻塞。
func sendEvent(ctx context.Context, ch chan<- Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// Stream 是调用方的统一入口：按模型 ID 找到端点、选后端工厂、开流并桥接为统一 Event。
// apiKey 为空时表示"没有可用 key"，由后端决定是否报错。
func (r *Registry) Stream(ctx context.Context, modelID, apiKey string, req ChatRequest) (<-chan Event, error) {
	ep, ok := r.FindByModel(modelID)
	if !ok {
		return nil, ErrNoEndpoint
	}
	f, ok := r.FindBackend(ep.Backend)
	if !ok {
		return nil, ErrNoBackend
	}
	req.Model = modelID

	cm, err := f(ctx, ep, apiKey, req)
	if err != nil {
		return nil, err
	}
	// req.Tools 为空时 cm 原样返回（老路径一字节不变）；非空时得到绑好工具的新实例。
	cm, err = withTools(cm, req)
	if err != nil {
		return nil, err
	}

	reader, err := cm.Stream(ctx, toSchemaMessages(req.Messages), CacheOptions(ep.Backend)...)
	if err != nil {
		return nil, err
	}

	// 只有调用方声明了工具才归集 tool_calls 分片。
	//
	// 契约 §2 要求「req.Tools 为空 → 走原路径，一字节行为不变」，所以这条分支是硬门：
	// 即使某个上游在没有声明工具时仍回了 tool_calls（协议违规），也不改变 Tools 为空
	// 时的事件序列——那是 CUA 与 /api/chat 现在依赖的行为。
	collectTools := len(req.Tools) > 0

	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer reader.Close()
		acc := &toolCallAccumulator{}
		var round Usage
		for {
			msg, err := reader.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					// 工具调用统一在这里整条发出：分片本身对上层没有意义，
					// 转发半截只会让 agent 循环重复执行同一个工具。
					for _, call := range acc.merged() {
						c := call
						if !sendEvent(ctx, ch, Event{Type: EventToolCall, ToolCall: &c}) {
							return
						}
					}
					done := Event{Type: EventDone}
					if !round.IsZero() {
						u := round
						done.Usage = &u
					}
					sendEvent(ctx, ch, done)
				} else {
					// 中途出错：只发 EventError，不发已经攒了半截的工具调用。
					sendEvent(ctx, ch, Event{Type: EventError, Err: err})
				}
				return
			}
			round = MaxUsage(round, UsageFromMessage(msg))
			if msg.Content != "" {
				if !sendEvent(ctx, ch, Event{Type: EventDelta, Content: msg.Content}) {
					return
				}
			}
			if !collectTools {
				continue
			}
			for _, tc := range msg.ToolCalls {
				acc.add(toolCallChunkFromSchema(tc))
			}
		}
	}()
	return ch, nil
}
