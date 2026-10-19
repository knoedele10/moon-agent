package agent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/llm"
)

// 本文件是主循环的**护栏**：统一出口截断、循环内压缩、安全切点、运行预算。
// 它们都不改变「模型 → 工具 → 回灌」的基本语义，只是防止长任务把上下文或时间跑飞。
//
// 设计参考 Cyrene-Agent 的 CyreneHarness（MIT）：它把这几件事写成了硬性 P0，
// 理由很直接——连续 loop 里每轮都会塞进 assistant 回复 + 工具结果，
// 「只在入口检查一次」是旧架构的错觉，跑到中段爆上下文是必然事件。

// ---------------------------------------------------------------------------
// 统一出口截断
// ---------------------------------------------------------------------------

// toolOutputCapDefault 是**统一出口**的硬上限（按 rune 计）。
//
// 为什么需要它：每个工具其实都有自己的预算——read 有 offset/limit（默认 2000 行）、
// web_reader 有 maxChars、sql_query 有行数上限、HTTP 响应体有 io.LimitReader。
// 但那都是**各自自觉**：新加一个工具、或某个工具改坏了，就可能一次吃进几万 token。
// 这里在 executeTool 的出口兜一道底，保证**无论谁写的工具**都不会把上下文撑爆。
//
// 取 16000：比任何单个工具的自觉预算都宽，正常工具永远不会撞到它，
// 只有"漏网"的输出才会被砍——所以它不会误伤正常行为。
const toolOutputCapDefault = 16000

// toolOutputCap 读 AGENT_TOOL_OUTPUT_CAP；非法/<=0 用默认值。
func toolOutputCap() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_TOOL_OUTPUT_CAP")))
	if err != nil || n <= 0 {
		return toolOutputCapDefault
	}
	return n
}

// capToolOutput 在工具出口把过长的输出砍掉，并**明确告诉模型被砍了**。
//
// 为什么必须留提示：静默截断会让模型以为自己看到了全部内容，
// 于是基于半截日志下结论。宁可让它知道"这里被砍了，需要就分段再读"。
func capToolOutput(name, content string) string {
	cap := toolOutputCap()
	r := []rune(content)
	if len(r) <= cap {
		return content
	}
	head := string(r[:cap])
	return fmt.Sprintf("%s\n\n…（%s 的输出过长，已截断：只保留前 %d 个字符，原文共 %d 个字符。需要更多内容请分段重试，不要基于这段不完整的输出下结论。）",
		head, name, cap, len(r))
}

// ---------------------------------------------------------------------------
// 安全切点
// ---------------------------------------------------------------------------

// safeCutIndex 在 msgs 里找一个**不会把 tool_call 和它的结果切到两边**的切点。
//
// 这是 Cyrene 文档里点名的 P0：按条数硬切（`msgs[:len-keep]`）可能把
// assistant(tool_calls) 留在前半、对应的 role=tool 结果留在后半，
// 而 OpenAI / Anthropic 协议**硬性要求两者配对**，切断即整条请求被拒。
// 这类错误会被误判成"供应商 bug"，极难排查。
//
// 规则：从候选切点往前退，直到落在一个**可以安全开头**的位置——
//   - role=user 的消息（正常的对话边界）
//   - 或没有 tool_calls 的 assistant 消息
//
// 绝不从 role=tool 的消息开始（它一定属于前面某个 assistant 的调用）。
//
// 返回 0 表示找不到安全切点（调用方应放弃压缩，保持原样）。
func safeCutIndex(msgs []llm.Message, keepTail int) int {
	if keepTail <= 0 || len(msgs) <= keepTail {
		return 0
	}
	cut := len(msgs) - keepTail
	for cut > 0 {
		m := msgs[cut]
		// role=tool 一定是某次调用的结果，不能作为开头。
		if m.Role == roleTool {
			cut--
			continue
		}
		// 上一条如果是"带 tool_calls 的 assistant"，说明它的结果还没走完，
		// 从这里切会把配对切断。
		prev := msgs[cut-1]
		if prev.Role == llm.RoleAssistant && len(prev.ToolCalls) > 0 {
			cut--
			continue
		}
		return cut
	}
	return 0
}

// ---------------------------------------------------------------------------
// 循环内压缩
// ---------------------------------------------------------------------------

// midLoopCompactTokensDefault 是触发循环内压缩的估算 token 阈值。
//
// 取 24000：比多数模型的上下文窗口留了足够余量，同时远低于"爆窗"的量级。
// 注意这是**估算**（按字符近似），宁可早一点压，也不要等到请求被上游拒掉。
const midLoopCompactTokensDefault = 24000

// midLoopKeepTail 压缩时保留尾部多少条消息不参与摘要（尾部是模型最需要看的部分）。
const midLoopKeepTail = 6

func midLoopCompactTokens() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_MIDLOOP_COMPACT_TOKENS")))
	if err != nil || n <= 0 {
		return midLoopCompactTokensDefault
	}
	return n
}

// estimateTokens 粗略估算一段文本的 token 数。
//
// 中文按 1 字 ≈ 1 token、其余（英文/代码）按 4 字符 ≈ 1 token。
// 不引 tokenizer：这里只需要"什么时候该压"的粗略判断，差 20% 无所谓。
func estimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// estimateMessagesTokens 估算整组消息的 token（含角色开销的粗略常量）。
func estimateMessagesTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m.Content) + 4 // 角色/分隔的粗略开销
		for _, tc := range m.ToolCalls {
			total += estimateTokens(tc.Name) + estimateTokens(tc.Arguments) + 4
		}
	}
	return total
}

// midLoopCompact 在循环中段把过长的历史压成一段摘要。
//
// 与入口压缩的区别：入口压缩处理的是"用户历史"，这里处理的是**本轮工具链**——
// 一次任务里模型可能读十几个文件，每轮都往 msgs 里塞工具输出，
// 跑到中段上下文可能是入场时的好几倍。
//
// 保留策略：**第一条（system）+ 尾部若干条**，中间压成一条 assistant 摘要。
// 尾部条数按"条"给，但实际切点走 safeCutIndex，保证配对完整。
//
// 返回新的 msgs；任何一步失败都原样返回，绝不因为压缩把这一轮弄坏。
func midLoopCompact(ctx context.Context, stream StreamFunc, model, key string, msgs []llm.Message, keepTail int) []llm.Message {
	if len(msgs) < 4 {
		return msgs
	}
	cut := safeCutIndex(msgs, keepTail)
	if cut <= 1 {
		// 找不到安全切点（或切完没剩什么）：放弃压缩，保持原样。
		return msgs
	}

	var b strings.Builder
	for _, m := range msgs[1:cut] {
		switch m.Role {
		case llm.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				names := make([]string, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					names = append(names, tc.Name)
				}
				fmt.Fprintf(&b, "助手请求调用：%s\n", strings.Join(names, ", "))
			}
			if s := strings.TrimSpace(m.Content); s != "" {
				b.WriteString("助手：" + clipRunes(s, 400) + "\n")
			}
		case roleTool:
			b.WriteString("工具结果：" + clipRunes(strings.TrimSpace(m.Content), 600) + "\n")
		case llm.RoleUser:
			b.WriteString("用户：" + clipRunes(strings.TrimSpace(m.Content), 400) + "\n")
		}
	}
	body := b.String()
	if strings.TrimSpace(body) == "" {
		return msgs
	}

	summary, err := summarizeForLoop(ctx, stream, model, key, body)
	if err != nil || strings.TrimSpace(summary) == "" {
		// 摘要失败就退而求其次：用**确定性压缩**（截断后的拼接）而不是放弃。
		// 放弃意味着这一轮可能直接爆窗，那更糟。
		summary = clipRunes(body, 4000)
	}

	out := make([]llm.Message, 0, len(msgs)-cut+2)
	out = append(out, msgs[0])
	out = append(out, llm.Message{
		Role: llm.RoleAssistant,
		Content: "【本轮早前步骤的摘要】以下是本次任务里更早的工具执行记录，已压缩；" +
			"需要细节时请重新调用对应工具，不要凭这段摘要臆断。\n" + summary,
	})
	out = append(out, msgs[cut:]...)
	return out
}

// summarizeForLoop 用一次**不带工具**的模型调用把一段记录压成摘要。
func summarizeForLoop(ctx context.Context, stream StreamFunc, model, key, body string) (string, error) {
	if stream == nil || strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("没有可用的模型调用")
	}
	ch, err := stream(ctx, model, key, llm.ChatRequest{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "你在压缩一段 Agent 执行记录。保留：用户目标、已经做过的工具操作及其确定性结果（路径/命令/退出码/错误）、" +
				"尚未解决的问题。丢弃：寒暄、重复内容、已被后续步骤取代的中间态。用中文，条目式，600 字以内。"},
			{Role: llm.RoleUser, Content: body},
		},
	})
	if err != nil {
		return "", err
	}
	var acc strings.Builder
	for ev := range ch {
		switch ev.Type {
		case llm.EventDelta:
			acc.WriteString(ev.Content)
		case llm.EventError:
			return "", ev.Err
		}
	}
	return strings.TrimSpace(acc.String()), nil
}

// ---------------------------------------------------------------------------
// 运行预算
// ---------------------------------------------------------------------------

// runTimeout 读 AGENT_RUN_TIMEOUT_SEC；非法/<=0 表示不限。
//
// 这是"执行时钟"：只限制**这一轮 Run 的墙钟时间**。
// 注意本项目里 ask_user 会让循环**立即返回**（等用户回答是下一轮的事），
// 所以"用户思考时间"天然不会计进这个预算——Cyrene 的双时钟在这里不需要额外实现，
// 只要把这一条写清楚，避免以后有人误以为要加"暂停计时器"。
func runTimeout() time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_RUN_TIMEOUT_SEC")))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
