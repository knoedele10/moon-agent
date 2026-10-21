package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"blog-my/internal/llm"

	einomodel "github.com/cloudwego/eino/components/model"
)

// EventKind 是主循环投递给上层的事件种类（契约 §3）。
type EventKind string

const (
	EvDelta      EventKind = "delta"       // 模型增量文本
	EvToolCall   EventKind = "tool_call"   // 模型请求调用工具
	EvToolResult EventKind = "tool_result" // 工具执行完毕
	EvDone       EventKind = "done"        // 正常结束（模型给出纯文本回复）
	EvNeedInput  EventKind = "need_input"  // 工具在向用户提问，本轮到此为止
	EvError      EventKind = "error"       // 出错 / 触顶
)

// roleTool 是工具结果消息的角色。契约 §2 规定 role ∈ {system,user,assistant,tool}；
// 用本地常量而不是 llm 包的常量，避免与并行推进的 llm 改动产生不必要的编译期耦合。
const roleTool = "tool"

// Event 是主循环投递的事件（契约 §3）。
type Event struct {
	Kind      EventKind
	Content   string         // delta / done：文本
	ToolCall  *llm.ToolCall  // tool_call
	ToolName  string         // tool_result
	ToolID    string         // tool_result
	OK        bool           // tool_result：false 表示工具报错（已回灌给模型，不代表循环结束）
	AwaitUser bool           // tool_result：正在等待用户输入/确认，不能作为已执行成功的学习证据
	Summary   string         // tool_result：给前端一行摘要
	Detail    string         // tool_result：完整结果（前端折叠展示）
	Display   map[string]any // tool_result：结构化摘要
	Err       error
}

// Config 是一次 agent 运行的配置（契约 §3）。
type Config struct {
	Model  string
	APIKey string
	// EinoModel enables Eino ReAct orchestration for this run. Nil keeps the
	// established loop available for compatibility and focused rollback.
	EinoModel    einomodel.ToolCallingChatModel
	SystemPrompt string
	// Temperature / MaxTokens 是请求级采样参数（契约 chat_params）。零值 = 不设置，
	// 与改造前的行为逐字节一致：各 llm 后端都是「Temperature != 0 才设 / MaxTokens > 0 才设」。
	Temperature         float64
	MaxTokens           int
	MaxIterations       int // 0 → DefaultMaxIterations
	MaxToolCalls        int // 0 → DefaultMaxToolCalls
	LimitRecoveryRounds int // 0 keeps strict limits; auto mode enables bounded replanning windows
	Tools               []Tool
	// ToolSource 非空时优先于 Tools：循环每轮从它取定义、按名解析工具，
	// 因此运行中新增的工具（tool_search 激活）下一轮就能被模型看到。
	// 为空时行为与改造前完全一致（由 Tools 建静态注册表）。
	ToolSource ToolSource
	Exec       *Exec

	// Parallel 为 true 时，同一轮里**连续**的只读调用并发执行（见 parallel.go）；
	// 写/执行/网络工具仍严格串行，事件与历史按输入顺序回放。
	// 默认 false：与改造前逐字节一致（web 层由 AGENT_TOOL_PARALLEL=read 打开）。
	Parallel bool
	// ParallelMax 是并发上限，0 → DefaultParallelMax。
	ParallelMax int
	// ParallelTools 是允许并发的工具名白名单。agent 包不认识具体工具，
	// 只按名字判断，避免把「看起来像读」当成「真的只读」。
	ParallelTools map[string]bool
}

const (
	// DefaultMaxIterations 是默认的最大模型轮数（契约 §3）。
	DefaultMaxIterations = 32
	// DefaultMaxToolCalls 是默认的工具调用次数上限（契约 §3）。
	// 契约原文写「一轮内总调用上限」有歧义，这里按**整次运行累计**实现：
	// 次数按 Run 全程累加；自主模式可通过 LimitRecoveryRounds 分阶段续行。
	DefaultMaxToolCalls = 96
)

var (
	// ErrMaxIterations 表示轮数触顶：模型迟迟不给最终答复。
	ErrMaxIterations = errors.New("agent: 达到最大迭代次数（触顶）")
	// ErrMaxToolCalls 表示工具调用次数触顶。
	ErrMaxToolCalls = errors.New("agent: 达到工具调用次数上限（触顶）")
	// ErrNoStreamer 表示没有安装模型调用实现（见 SetStreamer）。
	ErrNoStreamer = errors.New("agent: 未安装模型调用实现")
	// ErrNilStream 表示模型后端返回了空的事件流。
	ErrNilStream = errors.New("agent: 模型后端返回了空的事件流")
)

// Run 跑完整循环：模型 → 若有 tool_calls 则执行 → 结果回灌 → 再来一轮，
// 直到模型给出纯文本回复或触顶。事件通过 emit 同步投递（调用方负责并发安全）。
//
// 六条循环语义（契约 §3）：
//  1. 每轮把 history 发给模型（带 Tools）。
//  2. 该轮有 tool_calls：发 EvToolCall → 执行 → 发 EvToolResult → assistant(tool_calls) 与每条 tool 结果追加进 history → 下一轮。
//  3. 该轮是纯文本：delta 已实时发出，发 EvDone，结束。
//  4. 严格模式触顶发 EvError；启用续行时先重新规划，最终触顶保留执行结果并生成进度报告。
//  5. 工具报错**不回滚循环**：错误作为 is_error=true 的 tool 结果回灌，由模型决定下一步。
//  6. 每个工具执行前检查 ctx 取消（每轮发模型前也检查）。
//
// 正常结束返回 nil；出错返回错误。LimitReportError 表示已输出进度报告的触顶结果，供上层区分处理。
func Run(ctx context.Context, cfg Config, history []llm.Message, emit func(Event)) error {
	if cfg.EinoModel != nil {
		return runEino(ctx, cfg, history, emit)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 执行时钟：AGENT_RUN_TIMEOUT_SEC 限制**这一次 Run 的墙钟时间**，防止一个挂死的工具
	// 把请求永远占住。默认不限（0）。
	//
	// 关于 Cyrene 的「双时钟」：它需要把“等用户回答”从预算里剔出去，是因为它的 ask_user
	// 是在循环内部 pending 等答案。本项目不是——ask_user 会让循环**立即返回**，
	// 用户回答是下一轮 Run 的事，天然不计入这里的预算。所以不需要额外的暂停计时器。
	if d := runTimeout(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	stream := Streamer()
	if stream == nil {
		emitError(emit, ErrNoStreamer)
		return ErrNoStreamer
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
		// 空 Exec：Root 为空，文件工具的沙箱会拒绝一切根外路径（默认安全）。
		ex = &Exec{}
	}

	reg := NewRegistry(cfg.Tools...)
	source := toolSourceOf(cfg, reg)
	defs := source.Defs()
	msgs := withSystemPrompt(history, cfg.SystemPrompt)

	executed := 0
	toolState := &runToolState{}
	// Completion reminders are injected at most once per run.
	feedbackInjected, verifyReminded := false, false
	budget := newRunBudget(cfg)
	limitReport := func(cause error) error {
		input := append([]llm.Message(nil), msgs...)
		if fb := uncertainFeedback(toolState.uncertain); fb != "" {
			input = append(input, llm.Message{Role: llm.RoleSystem, Content: fb})
		}
		if needsVerification(toolState.lastMutationSeq, toolState.lastVerifySeq) {
			input = append(input, llm.Message{Role: llm.RoleSystem, Content: verificationFeedback()})
		}
		return finishAtLimit(ctx, cfg, input, budget, cause, emit)
	}
	iterationLimit := maxIter
	if budget.rounds > 0 {
		iterationLimit = budget.maxIterations()
	}
	for iter := 1; iter <= iterationLimit; iter++ {
		if err := ctx.Err(); err != nil {
			emitError(emit, err)
			return err
		}

		// 循环内压缩：长工具链跑到中段会把上下文撑爆，而入口那次检查管不到这里
		// （每轮都在往 msgs 里塞 assistant 回复 + 工具输出）。
		// 阈值按估算 token 判；压缩失败会原样返回，不影响这一轮。
		if estimateMessagesTokens(msgs) >= midLoopCompactTokens() {
			msgs = midLoopCompact(ctx, stream, cfg.Model, cfg.APIKey, msgs, midLoopKeepTail)
		}
		if budget.rounds > 0 {
			notice, err := budget.reserveModel()
			if err != nil {
				return limitReport(err)
			}
			if notice != "" {
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: notice})
			}
		}
		// 每轮重新取一次定义：动态目录（tool_search 激活）会在运行中增删工具，
		// 只取一次的话模型永远看不到新激活的工具。静态注册表下这一步是常量开销。
		defs = source.Defs()
		req := llm.ChatRequest{
			Model:       cfg.Model,
			Messages:    msgs,
			Temperature: cfg.Temperature,
			MaxTokens:   cfg.MaxTokens,
			Tools:       defs,
		}
		text, calls, err := modelRound(ctx, stream, cfg.Model, cfg.APIKey, req, emit)
		if err != nil {
			emitError(emit, err)
			return err
		}

		// 语义 3：纯文本轮——delta 已经实时发出过，这里只补 EvDone。
		if len(calls) == 0 {
			// 还有「结果未知的副作用」没交代清楚：不能让它就这样收尾。
			// 注入一条运行时提醒（用 role=system，**不伪装成 role=user**），要求模型如实告知用户。
			// 只注入一次，避免模型反复不改导致死循环。
			if fb := uncertainFeedback(toolState.uncertain); fb != "" && !feedbackInjected {
				feedbackInjected = true
				msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: text})
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: fb})
				continue
			}
			// 完成义务：改完之后没验证就想收尾 → 拦一次，要求它去验或**明说没验**。
			// 只拦一次，避免模型死活不改导致死循环。
			if needsVerification(toolState.lastMutationSeq, toolState.lastVerifySeq) && !verifyReminded {
				verifyReminded = true
				msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: text})
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: verificationFeedback()})
				continue
			}
			emitEvent(emit, Event{Kind: EvDone, Content: text})
			return nil
		}

		// 语义 2 前半：assistant 那条（含 tool_calls）必须先入历史，
		// 否则后面的 role=tool 消息在 OpenAI 系后端上会因为「没有对应的 tool_call」被拒。
		msgs = append(msgs, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   text,
			ToolCalls: calls,
		})

		// 语义 2 后半：执行调用。只读调用在 AGENT_TOOL_PARALLEL=read 时按「连续批」并发，
		// 其余（以及开关关闭时）逐个串行——串行路径与改造前一字不差。
		for _, group := range planCallGroups(cfg, calls) {
			if len(group) == 1 {
				call := calls[group[0]]
				// 每个被阻止的调用仍必须有结果，避免破坏工具消息的对应关系。
				// 语义 6：执行前检查取消。
				if err := ctx.Err(); err != nil {
					emitError(emit, err)
					return err
				}
				// 语义 4：调用次数触顶。
				if budget.rounds == 0 && executed >= maxCalls {
					err := fmt.Errorf("%w：本次运行已执行 %d 次工具调用（上限 %d），模型在第 %d 轮仍在请求调用 %q",
						ErrMaxToolCalls, executed, maxCalls, iter, call.Name)
					emitError(emit, err)
					return err
				}
				var budgetErr error
				if budget.rounds > 0 {
					budgetErr = budget.reserveTool()
				}
				if budgetErr == nil {
					executed++
				}

				res := toolState.execute(ctx, source, call, ex, budgetErr)
				if budget.rounds > 0 && budgetErr == nil {
					budget.record(call.Name, res)
				}

				emitEvent(emit, Event{
					Kind:      EvToolResult,
					ToolID:    call.ID,
					ToolName:  call.Name,
					OK:        !res.IsError,
					AwaitUser: strings.TrimSpace(res.AwaitUser) != "",
					Summary:   summarize(call.Name, res),
					Detail:    res.Content,
					Display:   res.Display,
				})

				// 语义 2.5：工具要求用户补充输入 → 立即停轮，把问题交给上层展示。
				// 不能继续调模型：模型会自己把用户的答案编出来（这正是要避免的幻觉）。
				if strings.TrimSpace(res.AwaitUser) != "" {
					// 带上 Display：ask_user 的候选项要能传到前端渲染成按钮，而不是只当成一段文字。
					emitEvent(emit, Event{Kind: EvNeedInput, ToolID: call.ID, ToolName: call.Name, Content: res.AwaitUser, Display: res.Display})
					return nil
				}

				msgs = append(msgs, llm.Message{
					Role:       roleTool,
					Content:    res.Content,
					ToolCallID: call.ID,
					ToolName:   call.Name,
				})

				// Result.Image：tool 角色的消息带不了图片，所以作为一条独立 user 消息追加。
				if res.Image != nil && len(res.Image.Data) > 0 {
					msgs = append(msgs, llm.Message{
						Role:    llm.RoleUser,
						Content: fmt.Sprintf("（下面的图片由工具 %s 返回）", call.Name),
						Images:  []llm.Image{{MIME: res.Image.MIME, Data: res.Image.Data}},
					})
				}
				continue
			}

			// 并行只读批：先把预算一次性占好，再并发执行，最后按输入顺序回放事件与消息。
			// 预算在**启动前**占：中途取消不会留下「已经算进上限但没跑」的空账。
			if err := ctx.Err(); err != nil {
				emitError(emit, err)
				return err
			}
			if budget.rounds == 0 && executed+len(group) > maxCalls {
				err := fmt.Errorf("%w：本次运行已执行 %d 次工具调用（上限 %d），模型在第 %d 轮仍在请求调用 %q",
					ErrMaxToolCalls, executed, maxCalls, iter, calls[group[len(group)-1]].Name)
				emitError(emit, err)
				return err
			}
			batch := make([]llm.ToolCall, 0, len(group))
			budgetErrOf := make(map[int]error, len(group))
			for _, idx := range group {
				call := calls[idx]
				// 与串行同一判据：调用次数触顶就立即中止（不是整批拒绝，也不吞掉剩余额度）。
				if budget.rounds == 0 && executed >= maxCalls {
					err := fmt.Errorf("%w：本次运行已执行 %d 次工具调用（上限 %d），模型在第 %d 轮仍在请求调用 %q",
						ErrMaxToolCalls, executed, maxCalls, iter, call.Name)
					emitError(emit, err)
					return err
				}
				var budgetErr error
				if budget.rounds > 0 {
					budgetErr = budget.reserveTool()
				}
				if budgetErr != nil {
					// 被预算拒绝的调用**不执行**：串行路径在 tool_state.go 里同样不调工具，
					// 这里若把它塞进批，副作用已经发生、模型却收到「没有执行」——那是假话。
					budgetErrOf[idx] = budgetErr
					continue
				}
				executed++
				batch = append(batch, call)
			}
			var results []Result
			if len(batch) > 0 {
				results = runReadOnlyBatch(ctx, source, ex, batch, parallelMax(cfg))
			}
			// 回放：按输入顺序补齐每个调用的 tool 消息与事件（含被预算拒绝的），
			// 这样 tool_call/result 永远配对；需要用户输入时先补完同批，再单独发 need_input。
			cur := 0
			needInputAt := -1
			for _, idx := range group {
				call := calls[idx]
				var res Result
				if budgetErr, denied := budgetErrOf[idx]; denied {
					res = Result{IsError: true, Content: budgetErr.Error()}
				} else {
					res = results[cur]
					cur++
				}
				if budget.rounds > 0 {
					if _, denied := budgetErrOf[idx]; !denied {
						budget.record(call.Name, res)
					}
				}
				// 记账必须走唯一入口：并发读回的文件也算「验证过」。
				toolState.recordStep(call.Name, res)

				emitEvent(emit, Event{
					Kind:      EvToolResult,
					ToolID:    call.ID,
					ToolName:  call.Name,
					OK:        !res.IsError,
					AwaitUser: strings.TrimSpace(res.AwaitUser) != "",
					Summary:   summarize(call.Name, res),
					Detail:    res.Content,
					Display:   res.Display,
				})
				if needInputAt < 0 && strings.TrimSpace(res.AwaitUser) != "" {
					needInputAt = idx
				}
				msgs = append(msgs, llm.Message{
					Role:       roleTool,
					Content:    res.Content,
					ToolCallID: call.ID,
					ToolName:   call.Name,
				})
				if res.Image != nil && len(res.Image.Data) > 0 {
					msgs = append(msgs, llm.Message{
						Role:    llm.RoleUser,
						Content: fmt.Sprintf("（下面的图片由工具 %s 返回）", call.Name),
						Images:  []llm.Image{{MIME: res.Image.MIME, Data: res.Image.Data}},
					})
				}
			}
			if needInputAt >= 0 {
				call := calls[needInputAt]
				var res Result
				if budgetErr, denied := budgetErrOf[needInputAt]; denied {
					res = Result{IsError: true, Content: budgetErr.Error()}
				} else {
					// 批内下标 → 结果下标：被拒绝的调用不占 results。
					cur := 0
					for _, idx := range group {
						if _, denied := budgetErrOf[idx]; denied {
							continue
						}
						if idx == needInputAt {
							res = results[cur]
							break
						}
						cur++
					}
				}
				emitEvent(emit, Event{Kind: EvNeedInput, ToolID: call.ID, ToolName: call.Name, Content: res.AwaitUser, Display: res.Display})
				return nil
			}
		}
	}

	// 语义 4：轮数触顶。
	err := fmt.Errorf("%w：已连续 %d 轮模型都在请求工具调用而没有给出最终答复，为避免无限循环而停止",
		ErrMaxIterations, iterationLimit)
	if budget.rounds > 0 {
		return limitReport(err)
	}
	emitError(emit, err)
	return err
}

// modelRound 调用一轮模型。上游在繁忙时常直接返回 502/503，工具链会因此表现成
// “工具刚执行完就报错”，因为工具结果还需要再发给模型整理。这里只在尚未收到任何
// 文本或 tool_call 时重试一次；已有输出就不重试，避免重复显示文本或重复执行工具。
func modelRound(ctx context.Context, stream StreamFunc, modelID, apiKey string, req llm.ChatRequest, emit func(Event)) (string, []llm.ToolCall, error) {
	for attempt := 0; attempt < 2; attempt++ {
		ch, err := stream(ctx, modelID, apiKey, req)
		text := ""
		var calls []llm.ToolCall
		if err == nil {
			text, calls, err = drain(ctx, ch, emit)
		}
		if err == nil {
			return text, calls, nil
		}
		if ctx.Err() != nil || attempt > 0 || text != "" || len(calls) > 0 || !retryableModelError(err) {
			return text, calls, err
		}
		if err := waitModelRetry(ctx); err != nil {
			return text, calls, err
		}
	}
	return "", nil, errors.New("agent: 模型重试流程异常结束")
}

// drain 读一整轮的模型事件流：实时转发 delta、收集 tool_calls，返回该轮文本与工具调用。
func drain(ctx context.Context, ch <-chan llm.Event, emit func(Event)) (string, []llm.ToolCall, error) {
	if ch == nil {
		return "", nil, ErrNilStream
	}
	var text strings.Builder
	var calls []llm.ToolCall
	for {
		select {
		case <-ctx.Done():
			return text.String(), calls, ctx.Err()
		case ev, ok := <-ch:
			// Cancellation and EOF may become ready together. select may choose
			// either, so observing EOF must not turn a canceled run into success.
			if err := ctx.Err(); err != nil {
				return text.String(), calls, err
			}
			if !ok {
				return text.String(), calls, nil
			}
			switch ev.Type {
			case llm.EventDelta:
				text.WriteString(ev.Content)
				emitEvent(emit, Event{Kind: EvDelta, Content: ev.Content})
			case llm.EventToolCall:
				if ev.ToolCall == nil {
					continue
				}
				calls = append(calls, *ev.ToolCall)
				emitEvent(emit, Event{Kind: EvToolCall, ToolCall: ev.ToolCall})
			case llm.EventError:
				if ev.Err == nil {
					return text.String(), calls, errors.New("agent: 模型返回了错误事件但没有原因")
				}
				return text.String(), calls, ev.Err
			case llm.EventDone:
				return text.String(), calls, nil
			}
		}
	}
}

// executeTool 查表 + 参数预处理，把一切失败都折成 is_error 结果（契约 §3.5：不回滚循环）。
func executeTool(ctx context.Context, source ToolSource, call llm.ToolCall, ex *Exec) Result {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Content: err.Error(), Display: map[string]any{"code": "TOOL_CANCELLED", "tool": call.Name}}
	}
	tool, ok := source.Lookup(call.Name)
	if !ok {
		return Result{
			IsError: true,
			Content: fmt.Sprintf("未知工具 %q：当前可用工具是 %s。请改用可用工具，或直接给出最终答复。",
				call.Name, availableToolNames(source)),
			Display: map[string]any{"code": "TOOL_UNKNOWN", "tool": call.Name},
		}
	}
	raw := strings.TrimSpace(call.Arguments)
	args := json.RawMessage(raw)
	if len(args) == 0 {
		// 无参工具：有些后端对无参数调用会发空字符串而不是 "{}"，兜一下。
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return Result{
			IsError: true,
			Content: fmt.Sprintf("工具 %q 的参数不是合法 JSON：%s", call.Name, clipRunes(raw, 200)),
			Display: map[string]any{"code": "TOOL_BAD_ARGS", "tool": call.Name},
		}
	}
	res := callTool(ctx, tool, args, ex)
	// 统一出口截断（见 guard.go）：放在这里是为了覆盖**所有**调用方
	// （主循环、待确认工具的直接执行……），不依赖每个工具自觉。
	res.Content = capToolOutput(call.Name, res.Content)
	return res
}

// callTool 调一次工具，并用 recover 兜住工具内部的 panic：
// 一个工具的 bug 不该把整条会话流打挂（按 is_error 回灌，模型还能继续）。
func callTool(ctx context.Context, tool Tool, args json.RawMessage, ex *Exec) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{
				IsError: true,
				Content: fmt.Sprintf("工具 %q 内部错误：%v", tool.Name(), r),
				Display: map[string]any{"code": "TOOL_PANIC", "tool": tool.Name()},
			}
		}
	}()

	res, err := tool.Execute(ctx, args, ex)
	if err != nil {
		// provider 只管原因（契约 §4.5 同一精神）：给模型看的话以 error 文本为准，
		// Display 保留工具自己填的结构化摘要。
		res.IsError = true
		res.Content = err.Error()
	}
	if res.IsError && strings.TrimSpace(res.Content) == "" {
		res.Content = fmt.Sprintf("工具 %q 失败，但没有给出说明", tool.Name())
	}
	return res
}

// withSystemPrompt 保证「配置的 system prompt 恰好生效一份」：
// history 首条已是 system 就替换其内容，否则前插一条；不会改动调用方传入的切片。
func withSystemPrompt(history []llm.Message, systemPrompt string) []llm.Message {
	msgs := append([]llm.Message(nil), history...)
	if strings.TrimSpace(systemPrompt) == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		msgs[0].Content = systemPrompt
		return msgs
	}
	return append([]llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}, msgs...)
}

// summarize 给前端一行摘要：优先用工具自己写在 Display["summary"] 里的说法。
func summarize(name string, res Result) string {
	if res.Display != nil {
		if s, ok := res.Display["summary"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	line := firstLine(res.Content)
	switch {
	case strings.TrimSpace(line) == "" && res.IsError:
		return name + "：失败"
	case strings.TrimSpace(line) == "":
		return name + "：完成"
	case res.IsError:
		return name + "：失败 — " + clipRunes(line, 80)
	default:
		return name + "：" + clipRunes(line, 80)
	}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func emitEvent(emit func(Event), ev Event) {
	if emit != nil {
		emit(ev)
	}
}

func emitError(emit func(Event), err error) {
	emitEvent(emit, Event{Kind: EvError, Err: err})
}
