package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"blog-my/internal/llm"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

var errEinoNeedsInput = errors.New("agent: 等待用户输入")

const executionStatePrefix = "【本轮实际执行状态】"

// einoRunState holds the project's execution rules around Eino's ReAct graph.
// Eino decides when to call the model or tools; the adapters retain the
// existing permission, retry, event and completion contracts.
type einoRunState struct {
	runToolState
	mu     sync.Mutex
	emit   func(Event)
	reg    *Registry
	ex     *Exec
	stream StreamFunc
	cfg    Config
	budget *runBudget

	maxCalls        int
	executed        int
	needInput       string
	images          map[string]*ImagePart
	lastHistory     []*schema.Message
	modelText       strings.Builder
	toolPreface     strings.Builder
	checkedToolCall bool
}

func (s *einoRunState) send(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	emitEvent(s.emit, ev)
}

func (s *einoRunState) resetOutputPass() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelText.Reset()
	s.toolPreface.Reset()
	s.checkedToolCall = false
}

func (s *einoRunState) resetModelText() {
	s.mu.Lock()
	s.modelText.Reset()
	s.mu.Unlock()
}

func (s *einoRunState) recordModelText(content string) {
	s.mu.Lock()
	s.modelText.WriteString(content)
	s.mu.Unlock()
}

func (s *einoRunState) markCheckedToolCall() {
	s.mu.Lock()
	s.checkedToolCall = true
	s.toolPreface.WriteString(s.modelText.String())
	s.mu.Unlock()
}

func (s *einoRunState) checkedOutput() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolPreface.String(), s.checkedToolCall
}

type einoToolAdapter struct {
	state *einoRunState
	inner Tool
	info  *schema.ToolInfo
}

var _ tool.InvokableTool = (*einoToolAdapter)(nil)

func (t *einoToolAdapter) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *einoToolAdapter) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	return t.state.call(ctx, t.inner.Name(), args)
}

func (s *einoRunState) call(ctx context.Context, name, args string) (string, error) {
	call := llm.ToolCall{ID: compose.GetToolCallID(ctx), Name: name, Arguments: args}
	s.send(Event{Kind: EvToolCall, ToolCall: &call})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.cfg.LimitRecoveryRounds <= 0 && s.executed >= s.maxCalls {
		return "", fmt.Errorf("%w：本次运行已执行 %d 次工具调用（上限 %d）", ErrMaxToolCalls, s.executed, s.maxCalls)
	}
	var budgetErr error
	if s.budget != nil && s.budget.rounds > 0 {
		budgetErr = s.budget.reserveTool()
	}
	if budgetErr == nil {
		s.executed++
	}

	res := s.runToolState.execute(ctx, s.reg, call, s.ex, budgetErr)
	if s.budget != nil && s.budget.rounds > 0 && budgetErr == nil {
		s.budget.record(name, res)
	}

	s.send(Event{Kind: EvToolResult, ToolID: call.ID, ToolName: name, OK: !res.IsError,
		Summary: summarize(name, res), Detail: res.Content, Display: res.Display})
	if res.Image != nil && len(res.Image.Data) > 0 {
		s.images[call.ID] = res.Image
	}
	if question := strings.TrimSpace(res.AwaitUser); question != "" {
		s.needInput = question
		s.send(Event{Kind: EvNeedInput, ToolID: call.ID, ToolName: name, Content: question, Display: res.Display})
		return "", errEinoNeedsInput
	}
	return res.Content, nil
}

func (s *einoRunState) unknown(ctx context.Context, name, args string) (string, error) {
	return s.call(ctx, name, args)
}

// rewrite runs before each Eino model step. It carries image-producing tool
// output forward as a user image message and retains the existing mid-loop
// compaction rule, including safe tool-call/result boundaries.
func (s *einoRunState) rewrite(ctx context.Context, msgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs)+len(s.images))
	for _, msg := range msgs {
		// Replace our transient status on each model step instead of accumulating it.
		if msg != nil && msg.Role == schema.System && strings.HasPrefix(msg.Content, executionStatePrefix) {
			continue
		}
		out = append(out, msg)
		if msg == nil || msg.Role != schema.Tool {
			continue
		}
		img := s.images[msg.ToolCallID]
		if img == nil {
			continue
		}
		delete(s.images, msg.ToolCallID)
		b64 := base64.StdEncoding.EncodeToString(img.Data)
		out = append(out, &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "（下面的图片由工具 " + msg.ToolName + " 返回）"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: img.MIME},
			}},
		}})
	}
	if s.stream != nil {
		plain := schemaMessagesToLLM(out)
		if estimateMessagesTokens(plain) >= midLoopCompactTokens() {
			out = llm.ToSchemaMessages(midLoopCompact(ctx, s.stream, s.cfg.Model, s.cfg.APIKey, plain, midLoopKeepTail))
		}
	}
	s.lastHistory = append([]*schema.Message(nil), out...)
	if s.executed > 0 {
		verification := "本轮尚无成功修改需要核对"
		if s.lastMutationSeq > 0 {
			verification = "最后一次成功修改之后尚未调用核对工具"
			if s.lastVerifySeq > s.lastMutationSeq {
				verification = "最后一次成功修改之后已调用核对工具，请依据工具返回内容判断目标是否达成"
			}
		}
		out = append(out, &schema.Message{Role: schema.System, Content: fmt.Sprintf(
			"%s 已尝试执行 %d 次工具调用；%s。失败或结果未知的操作不能声称完成。继续处理未完成步骤；完成后简洁说明实际结果和验证情况。",
			executionStatePrefix, s.executed, verification)})
	}
	return out
}

func schemaMessagesToLLM(msgs []*schema.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		item := llm.Message{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName}
		for _, tc := range m.ToolCalls {
			item.ToolCalls = append(item.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		for _, part := range m.UserInputMultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				if item.Content == "" {
					item.Content = part.Text
				}
			case schema.ChatMessagePartTypeImageURL:
				if part.Image != nil && part.Image.Base64Data != nil {
					if data, err := base64.StdEncoding.DecodeString(*part.Image.Base64Data); err == nil {
						item.Images = append(item.Images, llm.Image{MIME: part.Image.MIMEType, Data: data})
					}
				}
			}
		}
		out = append(out, item)
	}
	return out
}

// scanEinoOutput detects tool calls across the full model stream, while
// forwarding assistant text as soon as each chunk arrives. Some providers put
// a short text preamble before the first tool-call chunk, so Eino's default
// first-content checker would miss the later tool call.
func scanEinoOutput(state *einoRunState, ctx context.Context, reader *schema.StreamReader[*schema.Message]) (bool, error) {
	state.resetModelText()
	defer reader.Close()
	for {
		msg, err := reader.Recv()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if msg == nil {
			continue
		}
		if msg.Content != "" && (msg.Role == "" || msg.Role == schema.Assistant) {
			state.recordModelText(msg.Content)
			state.send(Event{Kind: EvDelta, Content: msg.Content})
		}
		if len(msg.ToolCalls) > 0 {
			state.markCheckedToolCall()
			return true, nil
		}
	}
}

func runEino(ctx context.Context, cfg Config, history []llm.Message, emit func(Event)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d := runTimeout(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}
	maxCalls := cfg.MaxToolCalls
	if maxCalls <= 0 {
		maxCalls = DefaultMaxToolCalls
	}
	ex := cfg.Exec
	if ex == nil {
		ex = &Exec{}
	}
	reg := NewRegistry(cfg.Tools...)
	infos, err := llm.ToToolInfos(reg.Defs())
	if err != nil {
		emitError(emit, err)
		return err
	}
	budget := newRunBudget(cfg)
	state := &einoRunState{emit: emit, reg: reg, ex: ex, stream: Streamer(), cfg: cfg, budget: budget,
		maxCalls: maxCalls, images: make(map[string]*ImagePart)}
	adapters := make([]tool.BaseTool, 0, len(infos))
	for _, info := range infos {
		inner, _ := reg.Lookup(info.Name)
		adapters = append(adapters, &einoToolAdapter{state: state, inner: inner, info: info})
	}
	modelWrapper := &retryEinoModel{inner: cfg.EinoModel, limit: maxIter, calls: new(atomic.Int64)}
	if budget.rounds > 0 {
		modelWrapper.limit = budget.maxIterations()
		modelWrapper.budget = budget
	}
	reactAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: modelWrapper,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: adapters, ExecuteSequentially: true, UnknownToolsHandler: state.unknown,
		},
		MaxStep:         modelWrapper.limit*2 + 2,
		MessageRewriter: state.rewrite,
		StreamToolCallChecker: func(ctx context.Context, reader *schema.StreamReader[*schema.Message]) (bool, error) {
			return scanEinoOutput(state, ctx, reader)
		},
	})
	if err != nil {
		emitError(emit, err)
		return err
	}
	input := llm.ToSchemaMessages(withSystemPrompt(history, cfg.SystemPrompt))
	var allText strings.Builder
	for pass := 0; pass < 3; pass++ {
		state.resetOutputPass()
		reader, err := reactAgent.Stream(ctx, input)
		if err != nil {
			if state.needInput != "" && errors.Is(err, errEinoNeedsInput) {
				return nil
			}
			return state.runError(ctx, err, maxIter)
		}
		var passText strings.Builder
		for {
			msg, readErr := reader.Recv()
			if ctxErr := ctx.Err(); ctxErr != nil {
				reader.Close()
				return state.runError(ctx, ctxErr, maxIter)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				reader.Close()
				if state.needInput != "" && errors.Is(readErr, errEinoNeedsInput) {
					return nil
				}
				return state.runError(ctx, readErr, maxIter)
			}
			if msg != nil && msg.Role == schema.Assistant && msg.Content != "" {
				passText.WriteString(msg.Content)
			}
		}
		reader.Close()
		toolPreface, checkedToolCall := state.checkedOutput()
		actualText := passText.String()
		if checkedToolCall && toolPreface != "" && !strings.HasPrefix(actualText, toolPreface) {
			passText.Reset()
			passText.WriteString(toolPreface)
			passText.WriteString(actualText)
		}
		allText.WriteString(passText.String())
		if state.needInput != "" {
			return nil
		}
		feedback := ""
		if pass == 0 {
			feedback = uncertainFeedback(state.uncertain)
		}
		if feedback == "" && pass < 2 && needsVerification(state.lastMutationSeq, state.lastVerifySeq) {
			feedback = verificationFeedback()
		}
		if feedback == "" {
			state.send(Event{Kind: EvDone, Content: allText.String()})
			return nil
		}
		input = append(append([]*schema.Message(nil), state.lastHistory...),
			&schema.Message{Role: schema.Assistant, Content: passText.String()},
			&schema.Message{Role: schema.System, Content: feedback})
	}
	state.send(Event{Kind: EvDone, Content: allText.String()})
	return nil
}

func (s *einoRunState) runError(ctx context.Context, err error, maxIter int) error {
	if s.budget.rounds > 0 && (errors.Is(err, ErrMaxIterations) || errors.Is(err, ErrMaxToolCalls) || errors.Is(err, compose.ErrExceedMaxSteps)) {
		if errors.Is(err, compose.ErrExceedMaxSteps) {
			err = fmt.Errorf("%w：自动续行后的执行步骤达到上限", ErrMaxIterations)
		}
		msgs := schemaMessagesToLLM(s.lastHistory)
		if fb := uncertainFeedback(s.uncertain); fb != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: fb})
		}
		if needsVerification(s.lastMutationSeq, s.lastVerifySeq) {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: verificationFeedback()})
		}
		return finishAtLimit(ctx, s.cfg, msgs, s.budget, err, s.emit)
	}
	return einoRunError(s.emit, err, maxIter)
}

func einoRunError(emit func(Event), err error, maxIter int) error {
	if errors.Is(err, compose.ErrExceedMaxSteps) {
		err = fmt.Errorf("%w：已达到 %d 轮模型调用上限", ErrMaxIterations, maxIter)
	}
	emitError(emit, err)
	return err
}
