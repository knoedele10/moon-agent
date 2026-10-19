package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/llm"
)

// ---------------------------------------------------------------------------
// 假模型：一个脚本化的 StreamFunc。整条循环因此可以在**不依赖真实 llm 后端**
// 的情况下确定性测试（契约 §9 验收要求）。
// ---------------------------------------------------------------------------

type scriptStep struct {
	text  string
	calls []llm.ToolCall
	err   error
}

type fakeModel struct {
	mu    sync.Mutex
	steps []scriptStep
	i     int
	seen  [][]llm.Message
	reqs  []llm.ChatRequest
	n     int
}

func (f *fakeModel) stream(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.seen = append(f.seen, append([]llm.Message(nil), req.Messages...))
	f.reqs = append(f.reqs, req)
	if f.i >= len(f.steps) {
		return nil, fmt.Errorf("假模型脚本已用尽（第 %d 次调用）", f.n)
	}
	st := f.steps[f.i]
	f.i++

	ch := make(chan llm.Event, len(st.calls)+2)
	if st.err != nil {
		ch <- llm.Event{Type: llm.EventError, Err: st.err}
		close(ch)
		return ch, nil
	}
	if st.text != "" {
		ch <- llm.Event{Type: llm.EventDelta, Content: st.text}
	}
	for i := range st.calls {
		c := st.calls[i]
		ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &c}
	}
	ch <- llm.Event{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// messagesAt 返回第 round 次模型调用（0 基）看到的完整消息列表。
func (f *fakeModel) messagesAt(round int) []llm.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if round < 0 || round >= len(f.seen) {
		return nil
	}
	return f.seen[round]
}

func (f *fakeModel) requestAt(round int) llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if round < 0 || round >= len(f.reqs) {
		return llm.ChatRequest{}
	}
	return f.reqs[round]
}

// ---------------------------------------------------------------------------
// 假工具
// ---------------------------------------------------------------------------

type fakeTool struct {
	name   string
	params map[string]any
	exec   func(ctx context.Context, args json.RawMessage, ex *Exec) (Result, error)

	mu   sync.Mutex
	n    int
	args []string
}

func (t *fakeTool) Name() string        { return t.name }
func (t *fakeTool) Description() string { return "fake tool " + t.name }
func (t *fakeTool) Parameters() map[string]any {
	if t.params != nil {
		return t.params
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *fakeTool) Execute(ctx context.Context, args json.RawMessage, ex *Exec) (Result, error) {
	t.mu.Lock()
	t.n++
	t.args = append(t.args, string(args))
	t.mu.Unlock()
	if t.exec == nil {
		return Result{Content: "ok"}, nil
	}
	return t.exec(ctx, args, ex)
}

func (t *fakeTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

func (t *fakeTool) argAt(i int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i < 0 || i >= len(t.args) {
		return ""
	}
	return t.args[i]
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func runScripted(t *testing.T, ctx context.Context, m *fakeModel, cfg Config, history []llm.Message) ([]Event, error) {
	t.Helper()
	SetStreamer(m.stream)
	t.Cleanup(func() { SetStreamer(nil) })
	var evs []Event
	err := Run(ctx, cfg, history, func(ev Event) { evs = append(evs, ev) })
	return evs, err
}

func kindSeq(evs []Event) []EventKind {
	out := make([]EventKind, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func countKind(evs []Event, k EventKind) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == k {
			n++
		}
	}
	return n
}

func onlyResult(t *testing.T, evs []Event) Event {
	t.Helper()
	var got []Event
	for _, ev := range evs {
		if ev.Kind == EvToolResult {
			got = append(got, ev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("期望恰好 1 个 tool_result，实际 %d 个：%v", len(got), kindSeq(evs))
	}
	return got[0]
}

func textOf(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString("|")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func oneCall(id, name, args string) []llm.ToolCall {
	return []llm.ToolCall{{ID: id, Name: name, Arguments: args}}
}

// ---------------------------------------------------------------------------
// 用例 1：零工具调用，直接结束
// ---------------------------------------------------------------------------

func TestRun_NoToolCalls_EndsDirectly(t *testing.T) {
	tool := &fakeTool{name: "read"}
	m := &fakeModel{steps: []scriptStep{{text: "不需要工具，我直接回答。"}}}
	cfg := Config{Model: "fake", Tools: []Tool{tool}}

	evs, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "你好"}})
	if err != nil {
		t.Fatalf("纯文本轮不应报错，得到 %v", err)
	}
	if got := fmt.Sprint(kindSeq(evs)); got != "[delta done]" {
		t.Fatalf("事件序列应为 [delta done]，实际 %s", got)
	}
	if m.callCount() != 1 {
		t.Fatalf("应只调用模型 1 次，实际 %d 次", m.callCount())
	}
	if tool.count() != 0 {
		t.Fatalf("不该执行任何工具，实际执行 %d 次", tool.count())
	}
	if evs[1].Content != "不需要工具，我直接回答。" {
		t.Fatalf("EvDone.Content 应带上最终文本，实际 %q", evs[1].Content)
	}
	// 每轮都要把工具定义发给模型（语义 1）。
	defs := m.requestAt(0).Tools
	if len(defs) != 1 || defs[0].Name != "read" {
		t.Fatalf("请求里应带 1 个工具定义 read，实际 %+v", defs)
	}
	if defs[0].Parameters == nil {
		t.Fatalf("工具定义的 Parameters 不能为 nil")
	}
}

func TestRun_RetriesBusyModelAfterToolResultWithoutRepeatingTool(t *testing.T) {
	tool := &fakeTool{name: "read", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
		return Result{Content: "file content"}, nil
	}}
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "read", `{}`)},
		{err: errors.New("503 Service Unavailable: Service is too busy")},
		{text: "读取完成。"},
	}}

	evs, err := runScripted(t, context.Background(), m, Config{Model: "fake", Tools: []Tool{tool}}, []llm.Message{{Role: llm.RoleUser, Content: "读取文件"}})
	if err != nil {
		t.Fatalf("503 空响应应自动重试并恢复，得到 %v", err)
	}
	if m.callCount() != 3 {
		t.Fatalf("模型应调用 3 次（工具请求、503、重试），实际 %d", m.callCount())
	}
	if tool.count() != 1 {
		t.Fatalf("模型重试不得重复执行工具，实际执行 %d 次", tool.count())
	}
	if countKind(evs, EvError) != 0 || countKind(evs, EvDone) != 1 {
		t.Fatalf("恢复后事件序列异常：%v", kindSeq(evs))
	}
}

func TestModelRoundDoesNotRetryAfterPartialOutput(t *testing.T) {
	calls := 0
	stream := func(context.Context, string, string, llm.ChatRequest) (<-chan llm.Event, error) {
		calls++
		ch := make(chan llm.Event, 2)
		ch <- llm.Event{Type: llm.EventDelta, Content: "已经开始"}
		ch <- llm.Event{Type: llm.EventError, Err: errors.New("503 Service Unavailable")}
		close(ch)
		return ch, nil
	}
	var evs []Event
	text, _, err := modelRound(context.Background(), stream, "fake", "", llm.ChatRequest{}, func(ev Event) { evs = append(evs, ev) })
	if err == nil || text != "已经开始" {
		t.Fatalf("部分输出后的错误应原样返回，text=%q err=%v", text, err)
	}
	if calls != 1 {
		t.Fatalf("已有部分输出时不得重试，实际调用 %d 次", calls)
	}
}

// ---------------------------------------------------------------------------
// 用例 2：一次工具调用后结束（含历史回灌形态）
// ---------------------------------------------------------------------------

func TestRun_OneToolCall_ThenDone(t *testing.T) {
	tool := &fakeTool{
		name: "read",
		exec: func(_ context.Context, args json.RawMessage, ex *Exec) (Result, error) {
			if ex == nil || ex.Root != `E:\root` {
				return Result{}, fmt.Errorf("Exec.Root 没有透传到工具：%+v", ex)
			}
			return Result{
				Content: "1	hello",
				Display: map[string]any{"summary": "读取 a.txt（1 行）"},
			}, nil
		},
	}
	m := &fakeModel{steps: []scriptStep{
		{text: "先读文件。", calls: oneCall("call_1", "read", `{"file_path":"a.txt"}`)},
		{text: "读完了，内容是 hello。"},
	}}
	cfg := Config{Model: "fake", Tools: []Tool{tool}, Exec: &Exec{SessionID: "s1", Root: `E:\root`}}

	evs, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "读 a.txt"}})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got := fmt.Sprint(kindSeq(evs)); got != "[delta tool_call tool_result delta done]" {
		t.Fatalf("事件序列不符：%s", got)
	}
	res := onlyResult(t, evs)
	if !res.OK || res.ToolName != "read" || res.ToolID != "call_1" || res.Summary != "读取 a.txt（1 行）" {
		t.Fatalf("tool_result 字段不符：%+v", res)
	}
	if res.Detail != "1	hello" {
		t.Fatalf("tool_result.Detail 应为工具正文，实际 %q", res.Detail)
	}
	if tool.count() != 1 || tool.argAt(0) != `{"file_path":"a.txt"}` {
		t.Fatalf("工具应被调用 1 次且参数原样透传，实际 %d 次 / %q", tool.count(), tool.argAt(0))
	}

	// 第二次模型调用看到的最后两条：assistant(tool_calls) + role=tool。
	msgs := m.messagesAt(1)
	if len(msgs) < 3 {
		t.Fatalf("第二轮消息太少：%v", textOf(msgs))
	}
	assistant := msgs[len(msgs)-2]
	toolMsg := msgs[len(msgs)-1]
	if assistant.Role != llm.RoleAssistant || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant 消息应带 tool_calls：%+v", assistant)
	}
	if assistant.Content != "先读文件。" {
		t.Fatalf("assistant 消息应保留该轮文本，实际 %q", assistant.Content)
	}
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "1	hello" {
		t.Fatalf("tool 消息不符：%+v", toolMsg)
	}
	if toolMsg.ToolName != "read" {
		t.Fatalf("tool 消息应带工具名，实际 %q", toolMsg.ToolName)
	}
}

// ---------------------------------------------------------------------------
// 用例 3（核心）：工具报错不回滚——错误回灌，模型纠正后继续
// 负向对照针对的就是这个用例：把「回灌」改成「直接终止」，它必须转红。
// ---------------------------------------------------------------------------

func TestRun_ToolErrorIsFedBackAndModelRecovers(t *testing.T) {
	tool := &fakeTool{
		name: "edit",
		exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
			return Result{}, errors.New(`FS_NOT_OBSERVED: cannot modify "a.txt": file has not been read — read the file, then retry`)
		},
	}
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "edit", `{"file_path":"a.txt"}`)},
		{text: "我明白了，先读再改。"},
	}}
	cfg := Config{Model: "fake", Tools: []Tool{tool}}

	evs, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "改一下 a.txt"}})
	if err != nil {
		t.Fatalf("工具报错**不该**终止循环（契约 §3.5），却得到错误：%v", err)
	}
	if countKind(evs, EvError) != 0 {
		t.Fatalf("不该发任何 EvError，实际 %v", kindSeq(evs))
	}
	if got := fmt.Sprint(kindSeq(evs)); got != "[tool_call tool_result delta done]" {
		t.Fatalf("事件序列不符：%s", got)
	}
	res := onlyResult(t, evs)
	if res.OK {
		t.Fatalf("工具报错的 tool_result.OK 应为 false：%+v", res)
	}
	if !strings.Contains(res.Detail, "FS_NOT_OBSERVED") {
		t.Fatalf("错误原因应回灌给前端/模型，实际 %q", res.Detail)
	}
	if !strings.Contains(res.Summary, "失败") {
		t.Fatalf("失败摘要应能一眼看出失败，实际 %q", res.Summary)
	}

	// 关键断言：模型在第二轮**真的看到了** is_error 的 tool 结果。
	msgs := m.messagesAt(1)
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || last.ToolCallID != "c1" {
		t.Fatalf("第二轮最后一条应是 c1 的 tool 结果：%+v", last)
	}
	if !strings.Contains(last.Content, "FS_NOT_OBSERVED") {
		t.Fatalf("回灌内容应含错误原因，实际 %q", last.Content)
	}
	if m.callCount() != 2 {
		t.Fatalf("模型应被再叫一次（给它纠正机会），实际 %d 次", m.callCount())
	}
}

// ---------------------------------------------------------------------------
// 用例 4/5：其它「工具侧失败」也走同一条回灌路径
// ---------------------------------------------------------------------------

func TestRun_UnknownToolIsFedBack(t *testing.T) {
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "nope", `{}`)},
		{text: "那我换个做法。"},
	}}
	evs, err := runScripted(t, context.Background(), m, Config{Model: "fake", Tools: []Tool{&fakeTool{name: "read"}}},
		[]llm.Message{{Role: llm.RoleUser, Content: "x"}})
	if err != nil {
		t.Fatalf("未知工具应回灌而不是终止：%v", err)
	}
	res := onlyResult(t, evs)
	if res.OK || !strings.Contains(res.Detail, "未知工具") {
		t.Fatalf("未知工具应作为 is_error 回灌：%+v", res)
	}
	if !strings.Contains(textOf(m.messagesAt(1)), "未知工具") {
		t.Fatalf("模型应看到未知工具的错误：%v", textOf(m.messagesAt(1)))
	}
}

func TestRun_BadArgsJSONIsFedBack(t *testing.T) {
	tool := &fakeTool{name: "read"}
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "read", `{oops`)},
		{text: "我重新给合法参数。"},
	}}
	evs, err := runScripted(t, context.Background(), m, Config{Model: "fake", Tools: []Tool{tool}},
		[]llm.Message{{Role: llm.RoleUser, Content: "x"}})
	if err != nil {
		t.Fatalf("非法 JSON 参数应回灌而不是终止：%v", err)
	}
	if tool.count() != 0 {
		t.Fatalf("参数非法时不该真的执行工具，实际执行 %d 次", tool.count())
	}
	res := onlyResult(t, evs)
	if res.OK || !strings.Contains(res.Detail, "不是合法 JSON") {
		t.Fatalf("应回灌参数错误：%+v", res)
	}
}

func TestRun_ToolPanicIsFedBack(t *testing.T) {
	tool := &fakeTool{
		name: "boom",
		exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
			panic("工具内部炸了")
		},
	}
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "boom", `{}`)},
		{text: "好的，我不用这个工具了。"},
	}}
	evs, err := runScripted(t, context.Background(), m, Config{Model: "fake", Tools: []Tool{tool}},
		[]llm.Message{{Role: llm.RoleUser, Content: "x"}})
	if err != nil {
		t.Fatalf("工具 panic 不该打挂整条循环：%v", err)
	}
	res := onlyResult(t, evs)
	if res.OK || !strings.Contains(res.Detail, "内部错误") {
		t.Fatalf("panic 应折成 is_error：%+v", res)
	}
}

// ---------------------------------------------------------------------------
// 用例 6：触顶——轮数
// ---------------------------------------------------------------------------

func TestRun_MaxIterations(t *testing.T) {
	tool := &fakeTool{name: "read"}
	steps := make([]scriptStep, 0, 10)
	for i := 0; i < 10; i++ {
		steps = append(steps, scriptStep{calls: oneCall(fmt.Sprintf("c%d", i), "read", `{}`)})
	}
	m := &fakeModel{steps: steps}
	cfg := Config{Model: "fake", Tools: []Tool{tool}, MaxIterations: 3}

	evs, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "x"}})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("应返回 ErrMaxIterations，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "触顶") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("错误文案必须说明触顶原因与上限，实际 %q", err.Error())
	}
	if countKind(evs, EvError) != 1 || evs[len(evs)-1].Kind != EvError {
		t.Fatalf("触顶必须发 EvError 且作为最后一个事件：%v", kindSeq(evs))
	}
	if !errors.Is(evs[len(evs)-1].Err, ErrMaxIterations) {
		t.Fatalf("EvError.Err 应是触顶错误，实际 %v", evs[len(evs)-1].Err)
	}
	if m.callCount() != 3 {
		t.Fatalf("模型应只被调用 3 轮，实际 %d", m.callCount())
	}
	if countKind(evs, EvDone) != 0 {
		t.Fatalf("触顶不该发 EvDone：%v", kindSeq(evs))
	}
}

// ---------------------------------------------------------------------------
// 用例 7：触顶——工具调用次数
// ---------------------------------------------------------------------------

func TestRun_MaxToolCalls(t *testing.T) {
	tool := &fakeTool{name: "read"}
	steps := make([]scriptStep, 0, 6)
	for i := 0; i < 6; i++ {
		steps = append(steps, scriptStep{calls: []llm.ToolCall{
			{ID: fmt.Sprintf("a%d", i), Name: "read", Arguments: `{}`},
			{ID: fmt.Sprintf("b%d", i), Name: "read", Arguments: `{}`},
		}})
	}
	m := &fakeModel{steps: steps}
	cfg := Config{Model: "fake", Tools: []Tool{tool}, MaxIterations: 6, MaxToolCalls: 2}

	evs, err := runScripted(t, context.Background(), m, cfg, []llm.Message{{Role: llm.RoleUser, Content: "x"}})
	if !errors.Is(err, ErrMaxToolCalls) {
		t.Fatalf("应返回 ErrMaxToolCalls，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "触顶") {
		t.Fatalf("错误文案必须说明触顶原因，实际 %q", err.Error())
	}
	if tool.count() != 2 {
		t.Fatalf("上限 2 → 只应执行 2 次，实际 %d 次", tool.count())
	}
	if got := countKind(evs, EvToolResult); got != 2 {
		t.Fatalf("应发 2 个 tool_result，实际 %d", got)
	}
	if evs[len(evs)-1].Kind != EvError {
		t.Fatalf("触顶应以 EvError 收尾：%v", kindSeq(evs))
	}
}

// ---------------------------------------------------------------------------
// 用例 8：ctx 取消（调用前 / 流中途）
// ---------------------------------------------------------------------------

func TestRun_ContextCanceledBeforeFirstRound(t *testing.T) {
	tool := &fakeTool{name: "read"}
	m := &fakeModel{steps: []scriptStep{{text: "不该被调用"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	evs, err := runScripted(t, ctx, m, Config{Model: "fake", Tools: []Tool{tool}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，实际 %v", err)
	}
	if countKind(evs, EvError) != 1 {
		t.Fatalf("取消应发 EvError：%v", kindSeq(evs))
	}
	if m.callCount() != 0 {
		t.Fatalf("已取消时不该再调模型，实际 %d 次", m.callCount())
	}
}

func TestRun_ContextCanceledDuringStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	SetStreamer(func(ctx context.Context, _ string, _ string, _ llm.ChatRequest) (<-chan llm.Event, error) {
		ch := make(chan llm.Event) // 永不发送：模拟上游卡住
		close(started)
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	})
	t.Cleanup(func() { SetStreamer(nil) })

	go func() {
		<-started
		cancel()
	}()

	var evs []Event
	err := Run(ctx, Config{Model: "fake"}, []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		func(ev Event) { evs = append(evs, ev) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("流中途取消应返回 context.Canceled，实际 %v", err)
	}
	if countKind(evs, EvError) != 1 {
		t.Fatalf("取消应发 EvError：%v", kindSeq(evs))
	}
}

// ---------------------------------------------------------------------------
// 用例 9：Result.Image 作为一条 user 消息追加
// ---------------------------------------------------------------------------

func TestRun_ImageResultBecomesUserMessage(t *testing.T) {
	tool := &fakeTool{
		name: "read_image",
		exec: func(_ context.Context, _ json.RawMessage, _ *Exec) (Result, error) {
			return Result{
				Content: "已读取 image.png（2x2）",
				Image:   &ImagePart{MIME: "image/png", Data: []byte{0x89, 0x50, 0x4E, 0x47}},
			}, nil
		},
	}
	m := &fakeModel{steps: []scriptStep{
		{calls: oneCall("c1", "read_image", `{"file_path":"image.png"}`)},
		{text: "我看到图了。"},
	}}
	evs, err := runScripted(t, context.Background(), m, Config{Model: "fake", Tools: []Tool{tool}},
		[]llm.Message{{Role: llm.RoleUser, Content: "看看这张图"}})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if countKind(evs, EvDone) != 1 {
		t.Fatalf("应正常结束：%v", kindSeq(evs))
	}
	msgs := m.messagesAt(1)
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("图片必须作为 user 消息追加（tool 消息带不了图），最后一条是 %q：%v", last.Role, textOf(msgs))
	}
	if len(last.Images) != 1 || last.Images[0].MIME != "image/png" || len(last.Images[0].Data) != 4 {
		t.Fatalf("图片内容不符：%+v", last.Images)
	}
	if strings.TrimSpace(last.Content) == "" {
		t.Fatalf("图片消息应附一句短说明（cua-code 建议，纯图片消息的真机行为待验证）")
	}
	// tool 文本结果仍然要有一条。
	if msgs[len(msgs)-2].Role != "tool" {
		t.Fatalf("tool 结果消息不应被图片挤掉：%v", textOf(msgs))
	}
}

// ---------------------------------------------------------------------------
// 用例 10/11：system prompt 注入、未装 Streamer
// ---------------------------------------------------------------------------

func TestRun_SystemPromptInjectedExactlyOnce(t *testing.T) {
	m := &fakeModel{steps: []scriptStep{{text: "ok"}, {text: "ok"}}}
	SetStreamer(m.stream)
	t.Cleanup(func() { SetStreamer(nil) })

	// history 里没有 system → 前插一条。
	if err := Run(context.Background(), Config{Model: "f", SystemPrompt: "你是助手"}, []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	msgs := m.messagesAt(0)
	if len(msgs) != 2 || msgs[0].Role != llm.RoleSystem || msgs[0].Content != "你是助手" {
		t.Fatalf("应前插一条 system：%+v", msgs)
	}
	// 原 history 不被改动（参数是值拷贝，但底层数组共享，这里断言调用方切片没被写）。
	orig := []llm.Message{{Role: llm.RoleSystem, Content: "旧的"}, {Role: llm.RoleUser, Content: "hi"}}
	if err := Run(context.Background(), Config{Model: "f", SystemPrompt: "新的"}, orig, nil); err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	msgs2 := m.messagesAt(1)
	if len(msgs2) != 2 || msgs2[0].Content != "新的" {
		t.Fatalf("history 首条已是 system 时应替换而不是再插一条：%+v", msgs2)
	}
	if orig[0].Content != "旧的" {
		t.Fatalf("调用方的 history 不能被就地修改，实际 %q", orig[0].Content)
	}
}

func TestRun_NoStreamer(t *testing.T) {
	SetStreamer(nil)
	var evs []Event
	err := Run(context.Background(), Config{Model: "f"}, nil, func(ev Event) { evs = append(evs, ev) })
	if !errors.Is(err, ErrNoStreamer) {
		t.Fatalf("应返回 ErrNoStreamer，实际 %v", err)
	}
	if countKind(evs, EvError) != 1 {
		t.Fatalf("应发 EvError：%v", kindSeq(evs))
	}
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

func TestRegistry_RegisterLookupDefs(t *testing.T) {
	a := &fakeTool{name: "read"}
	b := &fakeTool{name: "write"}
	reg := NewRegistry(a, b)
	if reg.Len() != 2 {
		t.Fatalf("应有 2 个工具，实际 %d", reg.Len())
	}
	if _, ok := reg.Lookup("read"); !ok {
		t.Fatalf("read 应可查到")
	}
	if _, ok := reg.Lookup("missing"); ok {
		t.Fatalf("missing 不该查到")
	}
	if err := reg.Register(a); err == nil {
		t.Fatalf("重名注册应报错")
	}
	if err := reg.Register(&fakeTool{name: "  "}); err == nil {
		t.Fatalf("空名字应报错")
	}
	defs := reg.Defs()
	if len(defs) != 2 || defs[0].Name != "read" || defs[1].Name != "write" {
		t.Fatalf("Defs 应保持登记顺序：%+v", defs)
	}
}
