package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	einoschema "github.com/cloudwego/eino/schema"
)

// 本文件是 task-36 的自测：llm 包工具调用支持。
// 覆盖：纯回归（Tools 为空行为不变）/ mock 工具脚本 / 分片按 Index 归并 /
//      不支持的后端 / 不可变 WithTools / 消息映射 / 负向对照（乱序、缺 Index、幽灵分片、中途报错）。

// ---------- 测试用假后端 ----------

// t36ChunkBackend 按预设分片序列回应，用来精确控制"分片到达顺序"。
type t36ChunkBackend struct {
	msgs  []*einoschema.Message
	tools []*einoschema.ToolInfo
}

var _ einomodel.ToolCallingChatModel = (*t36ChunkBackend)(nil)

func (c *t36ChunkBackend) Generate(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.Message, error) {
	return &einoschema.Message{Role: einoschema.Assistant, Content: "t36-plain"}, nil
}

func (c *t36ChunkBackend) WithTools(tools []*einoschema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	cp := *c
	cp.tools = tools
	return &cp, nil
}

func (c *t36ChunkBackend) Stream(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	reader, writer := einoschema.Pipe[*einoschema.Message](1)
	go func() {
		defer writer.Close()
		for _, m := range c.msgs {
			if writer.Send(m, nil) {
				return
			}
		}
	}()
	return reader, nil
}

// t36PlainModel 只实现 BaseChatModel（没有 WithTools），用来测 ErrToolsUnsupported。
type t36PlainModel struct{}

var _ einomodel.BaseChatModel = (*t36PlainModel)(nil)

func (t36PlainModel) Generate(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.Message, error) {
	return &einoschema.Message{Role: einoschema.Assistant, Content: "plain"}, nil
}

func (t36PlainModel) Stream(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	reader, writer := einoschema.Pipe[*einoschema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(&einoschema.Message{Role: einoschema.Assistant, Content: "plain"}, nil)
	}()
	return reader, nil
}

// t36Int 取 int 的地址（eino 的 ToolCall.Index 是指针）。
func t36Int(i int) *int { return &i }

// t36TC 造一个分片。
func t36TC(index *int, id, name, args string) einoschema.ToolCall {
	return einoschema.ToolCall{Index: index, ID: id, Type: "function",
		Function: einoschema.FunctionCall{Name: name, Arguments: args}}
}

// t36Drain 收完整个事件流；超时直接失败，避免测试挂死。
func t36Drain(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("等待事件超时，已收到 %d 个事件", len(out))
			return out
		}
	}
}

// t36Chunks 把事件流拆成 (文本, 工具调用, 末尾事件类型)。
func t36Chunks(evs []Event) (string, []ToolCall, EventType) {
	var text strings.Builder
	var calls []ToolCall
	last := EventType("")
	for _, ev := range evs {
		switch ev.Type {
		case EventDelta:
			text.WriteString(ev.Content)
		case EventToolCall:
			if ev.ToolCall == nil {
				calls = append(calls, ToolCall{})
			} else {
				calls = append(calls, *ev.ToolCall)
			}
		}
		last = ev.Type
	}
	return text.String(), calls, last
}

// t36ReadTool 是一个两参数的 read 工具定义（含嵌套/必填，模拟 fs 工具的形状）。
func t36ReadTool() ToolDef {
	return ToolDef{
		Name:        `read`,
		Description: `读取文件`,
		Parameters: map[string]any{
			`type`: `object`,
			`properties`: map[string]any{
				`file_path`: map[string]any{`type`: `string`},
				`limit`:     map[string]any{`type`: `integer`},
			},
			`required`: []any{`file_path`},
		},
	}
}

// ---------- 0. 负向对照：Tools 为空时，上游就算回了 tool_calls 也不发事件 ----------
//
// 契约 §2：req.Tools 为空 -> 走原路径，**一字节行为不变**。
// 用一个"故意违规"的后端来证明这条门真的关着。

func TestT36ToolsEmptyIgnoresToolCalls(t *testing.T) {
	backend := &t36ChunkBackend{msgs: []*einoschema.Message{
		{Role: einoschema.Assistant, Content: `文本`},
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(0), `a`, `read`, `{"y":1}`)}},
	}}
	reg := t36Registry(t, `t36-ignore`, t36Static(backend))
	ch, err := reg.Stream(context.Background(), `t36-ignore-1`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `hi`}},
	})
	if err != nil {
		t.Fatalf("Stream 出错: %v", err)
	}
	evs := t36Drain(t, ch)
	text, calls, last := t36Chunks(evs)
	if text != `文本` || last != EventDone {
		t.Fatalf("文本/结束事件不对: text=%q last=%q", text, last)
	}
	if len(calls) != 0 {
		t.Fatalf("Tools 为空时不该发 EventToolCall（老行为必须一字节不变），got %+v", calls)
	}
	if len(evs) != 2 {
		t.Fatalf("事件序列应只有 delta + done，got %+v", evs)
	}
}

// ---------- 1. 回归：Tools 为空的 mock 行为与改造前一致 ----------

func TestT36ToolsEmptyMockRegression(t *testing.T) {
	ClearMockScript()
	reg := NewRegistry()
	reg.SetEndpoints(DefaultEndpoints())

	ch, err := reg.Stream(context.Background(), `mock-echo`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `你好`}},
	})
	if err != nil {
		t.Fatalf("Stream 出错: %v", err)
	}
	text, calls, last := t36Chunks(t36Drain(t, ch))
	want := `[mock:mock-echo] 你说的是：你好`
	if text != want {
		t.Fatalf("回显文本变了：got %q want %q", text, want)
	}
	if len(calls) != 0 {
		t.Fatalf("Tools 为空时不该有工具调用，got %+v", calls)
	}
	if last != EventDone {
		t.Fatalf("末尾事件应为 done，got %q", last)
	}

	// Generate 也要一字不差（CUA 模型代理依赖它）。
	got, err := reg.Generate(context.Background(), `mock-echo`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `你好`}},
	})
	if err != nil {
		t.Fatalf("Generate 出错: %v", err)
	}
	if got != want {
		t.Fatalf("Generate 文本变了：got %q want %q", got, want)
	}
}

// ---------- 2. mock 工具脚本：第 0 轮工具调用、第 1 轮文本 ----------

func TestT36MockScriptToolCallThenText(t *testing.T) {
	SetMockScript([]MockTurn{
		{ToolCalls: []ToolCall{{ID: `call_1`, Name: `read`, Arguments: `{"file_path":"a.txt"}`}}},
		{Text: `读到了`},
	})
	defer ClearMockScript()

	reg := NewRegistry()
	reg.SetEndpoints(DefaultEndpoints())
	req := ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `读一下 a.txt`}},
		Tools:    []ToolDef{t36ReadTool()},
	}

	ch, err := reg.Stream(context.Background(), `mock-echo`, ``, req)
	if err != nil {
		t.Fatalf("第 1 轮 Stream 出错: %v", err)
	}
	text, calls, last := t36Chunks(t36Drain(t, ch))
	if text != `` {
		t.Fatalf("工具轮不该有文本 delta，got %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("第 1 轮应恰好 1 条工具调用，got %+v", calls)
	}
	// mock 把 arguments 切成两片发；这里必须是归并后的完整 JSON。
	if calls[0].ID != `call_1` || calls[0].Name != `read` || calls[0].Arguments != `{"file_path":"a.txt"}` {
		t.Fatalf("归并结果不对: %+v", calls[0])
	}
	if last != EventDone {
		t.Fatalf("工具轮末尾应为 done，got %q", last)
	}

	// 第 2 轮：把 assistant(tool_calls) 与 tool 结果回灌，mock 应给文本。
	req.Messages = []Message{
		{Role: RoleUser, Content: `读一下 a.txt`},
		{Role: RoleAssistant, ToolCalls: calls},
		{Role: RoleTool, ToolCallID: calls[0].ID, ToolName: calls[0].Name, Content: `文件内容`},
	}
	ch2, err := reg.Stream(context.Background(), `mock-echo`, ``, req)
	if err != nil {
		t.Fatalf("第 2 轮 Stream 出错: %v", err)
	}
	text2, calls2, last2 := t36Chunks(t36Drain(t, ch2))
	if text2 != `读到了` {
		t.Fatalf("第 2 轮文本不对: got %q", text2)
	}
	if len(calls2) != 0 {
		t.Fatalf("第 2 轮不该再有工具调用: %+v", calls2)
	}
	if last2 != EventDone {
		t.Fatalf("第 2 轮末尾应为 done，got %q", last2)
	}
}

// ---------- 3. 负向对照：乱序 Index ----------

func TestT36MergeOutOfOrderIndex(t *testing.T) {
	backend := &t36ChunkBackend{msgs: []*einoschema.Message{
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(1), `b`, `write`, `{"x":`)}},
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(0), `a`, `read`, `{"y":`)}},
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(1), ``, ``, `1}`)}},
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(0), ``, ``, `2}`)}},
	}}
	reg := t36Registry(t, `t36-frag`, func(context.Context, Endpoint, string, ChatRequest) (einomodel.BaseChatModel, error) {
		return backend, nil
	})

	ch, err := reg.Stream(context.Background(), `t36-frag-1`, ``, ChatRequest{Messages: []Message{{Role: RoleUser, Content: `x`}}, Tools: []ToolDef{t36ReadTool()}})
	if err != nil {
		t.Fatalf("Stream 出错: %v", err)
	}
	_, calls, last := t36Chunks(t36Drain(t, ch))
	if last != EventDone {
		t.Fatalf("末尾应为 done，got %q", last)
	}
	if len(calls) != 2 {
		t.Fatalf("应归并成 2 条调用，got %d: %+v", len(calls), calls)
	}
	// 乱序到达（先 1 后 0），发出的顺序必须按 Index 升序 → 0 在前。
	if calls[0].ID != `a` || calls[0].Arguments != `{"y":2}` {
		t.Fatalf("Index 0 归并不对: %+v", calls[0])
	}
	if calls[1].ID != `b` || calls[1].Arguments != `{"x":1}` {
		t.Fatalf("Index 1 归并不对: %+v", calls[1])
	}
}

// ---------- 4. 负向对照：缺 Index ----------

func TestT36MergeMissingIndex(t *testing.T) {
	t.Run(`独立完整调用按到达序`, func(t *testing.T) {
		backend := &t36ChunkBackend{msgs: []*einoschema.Message{
			{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(nil, `a`, `read`, `{"y":1}`)}},
			{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(nil, `b`, `write`, `{"x":2}`)}},
		}}
		reg := t36Registry(t, `t36-frag`, t36Static(backend))
		ch, err := reg.Stream(context.Background(), `t36-frag-1`, ``, ChatRequest{Tools: []ToolDef{t36ReadTool()}})
		if err != nil {
			t.Fatalf("Stream 出错: %v", err)
		}
		_, calls, _ := t36Chunks(t36Drain(t, ch))
		if len(calls) != 2 || calls[0].ID != `a` || calls[1].ID != `b` {
			t.Fatalf("无 Index 的独立调用应按到达顺序原样发出，got %+v", calls)
		}
	})

	t.Run(`裸续片拼到最近一条`, func(t *testing.T) {
		backend := &t36ChunkBackend{msgs: []*einoschema.Message{
			{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(nil, `a`, `read`, `{"y":`)}},
			{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(nil, ``, ``, `1}`)}},
		}}
		reg := t36Registry(t, `t36-frag`, t36Static(backend))
		ch, err := reg.Stream(context.Background(), `t36-frag-1`, ``, ChatRequest{Tools: []ToolDef{t36ReadTool()}})
		if err != nil {
			t.Fatalf("Stream 出错: %v", err)
		}
		_, calls, _ := t36Chunks(t36Drain(t, ch))
		if len(calls) != 1 || calls[0].Arguments != `{"y":1}` {
			t.Fatalf("裸续片应拼到最近一条上，got %+v", calls)
		}
	})
}

// ---------- 5. 负向对照：幽灵分片 / 流中途报错 ----------

func TestT36MergeNegativeControls(t *testing.T) {
	t.Run(`全空分片不产生幽灵调用`, func(t *testing.T) {
		acc := &toolCallAccumulator{}
		acc.add(toolCallChunk{})
		acc.add(toolCallChunk{Index: t36Int(0)})
		if got := acc.merged(); got != nil {
			t.Fatalf("全空分片应被丢弃，got %+v", got)
		}
	})

	t.Run(`流中途报错不发半截工具调用`, func(t *testing.T) {
		boom := errors.New(`t36-boom`)
		reader, writer := einoschema.Pipe[*einoschema.Message](1)
		go func() {
			defer writer.Close()
			writer.Send(&einoschema.Message{Role: einoschema.Assistant,
				ToolCalls: []einoschema.ToolCall{t36TC(t36Int(0), `a`, `read`, `{"y":`)}}, nil)
			writer.Send(nil, boom)
		}()
		backend := &t36ErrBackend{reader: reader}
		reg := t36Registry(t, `t36-err`, t36Static(backend))
		ch, err := reg.Stream(context.Background(), `t36-err-1`, ``, ChatRequest{Tools: []ToolDef{t36ReadTool()}})
		if err != nil {
			t.Fatalf("Stream 出错: %v", err)
		}
		evs := t36Drain(t, ch)
		for _, ev := range evs {
			if ev.Type == EventToolCall {
				t.Fatalf("报错时不该发出工具调用: %+v", ev)
			}
		}
		if len(evs) == 0 || evs[len(evs)-1].Type != EventError || !errors.Is(evs[len(evs)-1].Err, boom) {
			t.Fatalf("末尾应为一个 EventError(boom)，got %+v", evs)
		}
	})
}

// t36ErrBackend 直接返回预先准备好的（会报错的）reader。
type t36ErrBackend struct {
	reader *einoschema.StreamReader[*einoschema.Message]
	tools  []*einoschema.ToolInfo
}

var _ einomodel.ToolCallingChatModel = (*t36ErrBackend)(nil)

func (b *t36ErrBackend) Generate(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.Message, error) {
	return nil, errors.New(`t36-err-backend`)
}

func (b *t36ErrBackend) WithTools(tools []*einoschema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	cp := *b
	cp.tools = tools
	return &cp, nil
}

func (b *t36ErrBackend) Stream(context.Context, []*einoschema.Message, ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	return b.reader, nil
}

// ---------- 6. 不支持工具的后端 ----------

func TestT36ToolsUnsupported(t *testing.T) {
	reg := t36Registry(t, `t36-plain`, t36Static(&t36PlainModel{}))
	ctx := context.Background()

	// 带 Tools → ErrToolsUnsupported，且不返回 channel。
	ch, err := reg.Stream(ctx, `t36-plain-1`, ``, ChatRequest{Tools: []ToolDef{t36ReadTool()}})
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("应返回 ErrToolsUnsupported，got err=%v ch=%v", err, ch)
	}
	if ch != nil {
		t.Fatalf("出错时不该返回事件通道")
	}

	// 不带 Tools → 走老路径，正常出事件（回归）。
	ch2, err := reg.Stream(ctx, `t36-plain-1`, ``, ChatRequest{Messages: []Message{{Role: RoleUser, Content: `hi`}}})
	if err != nil {
		t.Fatalf("Tools 为空时不该出错: %v", err)
	}
	text, _, last := t36Chunks(t36Drain(t, ch2))
	if text != `plain` || last != EventDone {
		t.Fatalf("老路径行为变了: text=%q last=%q", text, last)
	}

	// Generate 带 Tools 同样返回 ErrToolsUnsupported。
	if _, err := reg.Generate(ctx, `t36-plain-1`, ``, ChatRequest{Tools: []ToolDef{t36ReadTool()}}); !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("Generate 应返回 ErrToolsUnsupported，got %v", err)
	}
	// Generate 不带 Tools 行为不变。
	got, err := reg.Generate(ctx, `t36-plain-1`, ``, ChatRequest{Messages: []Message{{Role: RoleUser, Content: `hi`}}})
	if err != nil || got != `plain` {
		t.Fatalf("Generate 老路径行为变了: got=%q err=%v", got, err)
	}
}

// ---------- 7. WithTools 不可变（并发安全的前提） ----------

func TestT36WithToolsImmutable(t *testing.T) {
	base := &mockChatModel{model: `m`, reqTools: true}

	a, err := base.WithTools([]*einoschema.ToolInfo{{Name: `a`}})
	if err != nil {
		t.Fatalf("WithTools(a): %v", err)
	}
	b, err := base.WithTools([]*einoschema.ToolInfo{{Name: `b`}})
	if err != nil {
		t.Fatalf("WithTools(b): %v", err)
	}

	if len(base.tools) != 0 {
		t.Fatalf("WithTools 改动了原实例：base.tools=%+v", base.tools)
	}
	if a == b {
		t.Fatalf("两次 WithTools 返回了同一个实例（不可变语义被破坏）")
	}
	ma, ok := a.(*mockChatModel)
	if !ok || len(ma.tools) != 1 || ma.tools[0].Name != `a` {
		t.Fatalf("第一个副本的工具集不对: %+v", ma.tools)
	}
	mb, ok := b.(*mockChatModel)
	if !ok || len(mb.tools) != 1 || mb.tools[0].Name != `b` {
		t.Fatalf("第二个副本的工具集不对: %+v", mb.tools)
	}
	// 在副本上再绑一次，不得回写副本自己。
	if _, err := ma.WithTools([]*einoschema.ToolInfo{{Name: `c`}}); err != nil {
		t.Fatalf("副本再绑: %v", err)
	}
	if len(ma.tools) != 1 || ma.tools[0].Name != `a` {
		t.Fatalf("副本被就地改写: %+v", ma.tools)
	}
}

// ---------- 8. ToolDef → ToolInfo 映射 ----------

func TestT36ToToolInfos(t *testing.T) {
	infos, err := toToolInfos([]ToolDef{t36ReadTool(), {Name: `noargs`, Description: `无参数`}})
	if err != nil {
		t.Fatalf("toToolInfos: %v", err)
	}
	if len(infos) != 2 || infos[0].Name != `read` || infos[0].Desc != `读取文件` {
		t.Fatalf("映射结果不对: %+v", infos[0])
	}
	if infos[1].ParamsOneOf != nil {
		t.Fatalf("无参数工具的 ParamsOneOf 应为 nil")
	}
	spec, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	for _, want := range []string{`file_path`, `limit`, `required`, `object`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("JSON Schema 丢了 %q: %s", want, raw)
		}
	}

	// 负向：缺 name。
	if _, err := toToolInfos([]ToolDef{{Description: `x`}}); !errors.Is(err, ErrToolDefInvalid) {
		t.Fatalf("缺 name 应返回 ErrToolDefInvalid，got %v", err)
	}
	// 负向：parameters 无法序列化（channel 不是 JSON 值）。
	if _, err := toToolInfos([]ToolDef{{Name: `bad`, Parameters: map[string]any{`p`: make(chan int)}}}); !errors.Is(err, ErrToolDefInvalid) {
		t.Fatalf("非法 parameters 应返回 ErrToolDefInvalid，got %v", err)
	}
}

// ---------- 9. 消息映射（含 cua-arch 问的"纯图片 user 消息"） ----------

func TestT36ToSchemaMessages(t *testing.T) {
	// 回归：纯文本消息的 JSON 形状必须与改造前一模一样。
	plain := toSchemaMessages([]Message{{Role: RoleUser, Content: `hi`}})
	if len(plain) != 1 {
		t.Fatalf("消息条数不对: %d", len(plain))
	}
	raw, err := json.Marshal(plain[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"role":"user","content":"hi"}` {
		t.Fatalf("纯文本消息 JSON 变了: %s", raw)
	}

	// assistant 携带 tool_calls。
	got := toSchemaMessages([]Message{{Role: RoleAssistant, ToolCalls: []ToolCall{
		{ID: `call_1`, Name: `read`, Arguments: `{"file_path":"a.txt"}`},
	}}})
	if len(got[0].ToolCalls) != 1 {
		t.Fatalf("assistant 的 tool_calls 没映射过去: %+v", got[0])
	}
	tc := got[0].ToolCalls[0]
	if tc.ID != `call_1` || tc.Type != `function` || tc.Function.Name != `read` || tc.Function.Arguments != `{"file_path":"a.txt"}` {
		t.Fatalf("tool_call 映射不对: %+v", tc)
	}

	// role=tool 的结果消息。
	toolMsg := toSchemaMessages([]Message{{Role: RoleTool, ToolCallID: `call_1`, ToolName: `read`, Content: `内容`}})[0]
	if toolMsg.Role != einoschema.Tool || toolMsg.ToolCallID != `call_1` || toolMsg.ToolName != `read` || toolMsg.Content != `内容` {
		t.Fatalf("tool 结果消息映射不对: %+v", toolMsg)
	}

	// 纯图片 user 消息：Content 必须为空，且只产出 1 个 image part（不会造出空 text part）。
	img := toSchemaMessages([]Message{{Role: RoleUser, Images: []Image{{MIME: `image/png`, Data: []byte{1, 2, 3}}}}})[0]
	if img.Content != `` {
		t.Fatalf("图片消息的 Content 应为空，got %q", img.Content)
	}
	if len(img.UserInputMultiContent) != 1 {
		t.Fatalf("纯图片消息应只有 1 个 part，got %d: %+v", len(img.UserInputMultiContent), img.UserInputMultiContent)
	}
	if img.UserInputMultiContent[0].Type != einoschema.ChatMessagePartTypeImageURL {
		t.Fatalf("part 类型应为 image_url，got %q", img.UserInputMultiContent[0].Type)
	}
	// 文字 + 图片：先 text 后 image。
	both := toSchemaMessages([]Message{{Role: RoleUser, Content: `看图`, Images: []Image{{MIME: `image/png`, Data: []byte{9}}}}})[0]
	if both.Content != `` || len(both.UserInputMultiContent) != 2 || both.UserInputMultiContent[0].Type != einoschema.ChatMessagePartTypeText {
		t.Fatalf("文字+图片映射不对: %+v", both)
	}
}

// ---------- 10. 常量与 mock 边界 ----------

func TestT36EventToolCallConstant(t *testing.T) {
	if EventToolCall != `tool_call` {
		t.Fatalf("EventToolCall 应为 tool_call，got %q", EventToolCall)
	}
	if EventToolCall == EventDelta || EventToolCall == EventDone || EventToolCall == EventError {
		t.Fatalf("EventToolCall 与既有常量撞了")
	}
	if RoleTool != `tool` {
		t.Fatalf("RoleTool 应为 tool，got %q", RoleTool)
	}
}

func TestT36MockScriptBoundaries(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()
	reg.SetEndpoints(DefaultEndpoints())

	// 脚本只有 1 轮，第 2 轮回落默认回显（脚本用完不 panic）。
	SetMockScript([]MockTurn{{ToolCalls: []ToolCall{{ID: `c1`, Name: `read`, Arguments: `{}`}}}})
	defer ClearMockScript()
	req := ChatRequest{
		Messages: []Message{
			{Role: RoleUser, Content: `u`},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: `c1`, Name: `read`, Arguments: `{}`}}},
			{Role: RoleTool, ToolCallID: `c1`, Content: `r`},
		},
		Tools: []ToolDef{t36ReadTool()},
	}
	ch, err := reg.Stream(ctx, `mock-echo`, ``, req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, calls, _ := t36Chunks(t36Drain(t, ch))
	if !strings.Contains(text, `[mock:mock-echo]`) || len(calls) != 0 {
		t.Fatalf("脚本用完应回落回显，got text=%q calls=%+v", text, calls)
	}

	// 带 Tools 但没设脚本：mock 只回文本、不造工具调用（"模型选择不用工具"这条路）。
	ClearMockScript()
	ch2, err := reg.Stream(ctx, `mock-echo`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `你好`}},
		Tools:    []ToolDef{t36ReadTool()},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, calls2, _ := t36Chunks(t36Drain(t, ch2)); len(calls2) != 0 {
		t.Fatalf("没有脚本时不该造工具调用: %+v", calls2)
	}

	// 脚本里的 Err 轮：直接变成 EventError。
	SetMockScript([]MockTurn{{Err: errors.New(`t36-script-err`)}})
	ch3, err := reg.Stream(ctx, `mock-echo`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `u`}}, Tools: []ToolDef{t36ReadTool()},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := t36Drain(t, ch3)
	if len(evs) != 1 || evs[0].Type != EventError || evs[0].Err == nil {
		t.Fatalf("脚本错误轮应只发一个 EventError，got %+v", evs)
	}
}

// ---------- 11. 全链路：Tools 非空经 Registry 走到真正绑好的实例 ----------

func TestT36StreamWithToolsBindsThroughRegistry(t *testing.T) {
	inner := &t36ChunkBackend{msgs: []*einoschema.Message{
		{Role: einoschema.Assistant, ToolCalls: []einoschema.ToolCall{t36TC(t36Int(0), `a`, `read`, `{"y":1}`)}},
	}}
	var seen einomodel.BaseChatModel
	reg := t36Registry(t, `t36-bind`, func(_ context.Context, _ Endpoint, _ string, req ChatRequest) (einomodel.BaseChatModel, error) {
		seen = inner
		return inner, nil
	})
	ch, err := reg.Stream(context.Background(), `t36-bind-1`, ``, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: `x`}},
		Tools:    []ToolDef{t36ReadTool()},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, calls, last := t36Chunks(t36Drain(t, ch))
	if len(calls) != 1 || calls[0].ID != `a` {
		t.Fatalf("绑定后工具调用不对: %+v", calls)
	}
	if last != EventDone {
		t.Fatalf("末尾应为 done，got %q", last)
	}
	// withTools 必须走 WithTools（副本），不能就地改工厂给的实例。
	if len(seen.(*t36ChunkBackend).tools) != 0 {
		t.Fatalf("原实例被就地绑定（应返回副本）: %+v", seen.(*t36ChunkBackend).tools)
	}
}

// ---------- 测试工具 ----------

// t36Registry 造一个只含一个端点的 Registry。
func t36Registry(t *testing.T, backend string, f func(context.Context, Endpoint, string, ChatRequest) (einomodel.BaseChatModel, error)) *Registry {
	t.Helper()
	reg := NewRegistry()
	reg.SetEndpoints([]Endpoint{{
		ID: "ep-" + backend, Label: "t36", Backend: backend,
		Models: []string{backend + "-1"},
	}})
	reg.Register(backend, f)
	return reg
}

// t36Static 把固定实例包成工厂。
func t36Static(cm einomodel.BaseChatModel) func(context.Context, Endpoint, string, ChatRequest) (einomodel.BaseChatModel, error) {
	return func(context.Context, Endpoint, string, ChatRequest) (einomodel.BaseChatModel, error) {
		return cm, nil
	}
}

// ---------- 12. 真实后端（openai/claude/gemini/ollama/mock）确实能被 WithTools 绑定 ----------
//
// 把「Eino 各后端都实现了 ToolCallingChatModel」从"读源码的结论"变成仓内可复跑的断言。
// 这里只做**构造**，不发任何网络请求（构造器不联网），所以离线可跑。

func TestT36RealBackendsSupportWithTools(t *testing.T) {
	ctx := context.Background()
	req := ChatRequest{Model: `t36-model`, Tools: []ToolDef{t36ReadTool()}}
	cases := []struct {
		name string
		f    backendFactory
		ep   Endpoint
	}{
		{`openai`, NewOpenAIBackend, Endpoint{ID: `openai`, Backend: `openai`, BaseURL: `https://api.deepseek.com/v1`}},
		{`claude`, NewClaudeBackend, Endpoint{ID: `anthropic`, Backend: `claude`, BaseURL: `https://api.anthropic.com`}},
		{`gemini`, NewGeminiBackend, Endpoint{ID: `gemini`, Backend: `gemini`}},
		{`ollama`, NewOllamaBackend, Endpoint{ID: `ollama`, Backend: `ollama`, BaseURL: `http://localhost:11434`}},
		{`mock`, NewMockBackend, Endpoint{ID: `mock`, Backend: `mock`, Models: []string{`mock-echo`}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, err := c.f(ctx, c.ep, `t36-fake-key`, req)
			if err != nil {
				t.Fatalf("构造 %s 后端失败: %v", c.name, err)
			}
			if _, ok := base.(einomodel.ToolCallingChatModel); !ok {
				t.Fatalf("%s 后端没实现 model.ToolCallingChatModel，req.Tools 会退化成 ErrToolsUnsupported", c.name)
			}
			bound, err := withTools(base, req)
			if err != nil {
				t.Fatalf("withTools(%s) 出错: %v", c.name, err)
			}
			if bound == nil {
				t.Fatalf("withTools(%s) 返回 nil", c.name)
			}
			// 反向对照：不带 Tools 时必须原样返回同一个实例（老路径零行为变化）。
			same, err := withTools(base, ChatRequest{Model: req.Model})
			if err != nil {
				t.Fatalf("无 Tools 时 withTools 出错: %v", err)
			}
			if same != base {
				t.Fatalf("无 Tools 时应原样返回同一实例，不得走 WithTools 分支")
			}
		})
	}
}

// ---------- 13. 负向对照：禁止使用已废弃的 BindTools ----------

// Lead 的红线：BindTools 就地改写实例、并发下互相覆盖，必须用 WithTools。
// 这里做成源码级守卫，防止以后有人"顺手"改回去。

func TestT36NoDeprecatedBindTools(t *testing.T) {
	entries, err := os.ReadDir(`.`)
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}
	// 先自证检查器有效（对检查器本身的负向对照）：
	if !t36HasForbiddenCall(`x.BindTools(tools)`) {
		t.Fatalf("检查器失效：合成的 BindTools 调用没被识别")
	}
	if t36HasForbiddenCall(`// 注释里提到 BindTools 不算调用`) {
		t.Fatalf("检查器误报：注释不该算调用")
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, `.go`) || strings.HasSuffix(name, `_test.go`) {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		checked++
		if t36HasForbiddenCall(string(raw)) {
			t.Fatalf("%s 出现已废弃的 BindTools 调用；必须用 ToolCallingChatModel.WithTools", name)
		}
	}
	if checked == 0 {
		t.Fatalf("没检查到任何源文件，守卫形同虚设")
	}
}

// t36HasForbiddenCall 判断源码里是否出现 BindTools 的**方法调用**（带点与左括号）。
func t36HasForbiddenCall(src string) bool {
	return strings.Contains(src, `.BindTools(`)
}
