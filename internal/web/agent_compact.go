package web

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 上下文压缩：对话变长后，把「最近 keepN 条以外」的旧消息压成一段滚动摘要，
// 只把摘要 + 最近若干条发给模型。思路对齐 Open WebUI 的 context_compaction。
//
// 为什么需要它：模型的上下文窗口是有限的。一轮工具调用可能塞进几千字（文件内容、
// 网页正文、SQL 结果），十几轮之后历史就爆了——要么请求被上游拒绝，要么模型开始
// 丢失最早的约定（"用 Go 写""别改那个文件"）。
//
// 三条不变量：
//   - 摘要**覆盖到哪一条**用 message.id 记住（conversation.summary_upto），所以
//     不会重复摘要、也不会漏掉中间的消息；
//   - 压缩失败一律降级为「不压缩、照原样发全量历史」——宁可多花 token，也不能丢上下文；
//   - 只裁掉**已经离开最近窗口**的消息，最近的对话永远原样保留。

const (
	// compactKeepDefault 最近保留多少条消息不参与压缩。
	compactKeepDefault = 10
	// compactTriggerDefault 历史超过多少条才启动压缩。
	// 必须明显大于 keep，否则每轮都要重算摘要，白烧模型调用。
	compactTriggerDefault = 24
	// compactMaxRunes 摘要正文上限。它每轮都注入，必须短。
	compactMaxRunes = 1600
	// compactMinNewMessages 距离上次摘要又攒了多少条新消息才值得重新摘要。
	// 太小会导致每轮都重算；太大则摘要跟不上。
	compactMinNewMessages = 6
)

// compactEnabled 读 AGENT_COMPACT，默认开启；写 0/false/off/no 关掉。
func compactEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_COMPACT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// compactIntEnv 读一个正整数环境变量，缺省/非法/非正时用 fallback。
func compactIntEnv(name string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// compactSystemPrompt 是压缩用的提示词模板，结构对齐 Open WebUI 的
// DEFAULT_CONTEXT_COMPACTION_PROMPT：明确要求保留决定、偏好、产物、当前状态与下一步。
const compactSystemPrompt = `你在压缩一段即将被移出上下文的对话历史，供助手在看不到原文的情况下继续工作。

要求：
- 保留关键决定、用户明确表达的偏好与约束。
- 保留重要的文件路径、产物、工具执行结果与代码改动。
- 保留当前任务状态、未解决的问题、下一步要做什么。
- 事实准确，**不要编造**原文里没有的内容。
- 用中文，条理清楚，控制在 600 字以内。

输出格式：
## 已确定的事
## 用户偏好与约束
## 当前进度与下一步`

// compactResult 是一次压缩决策的结果。
type compactResult struct {
	// History 是真正发给模型的历史（可能已被裁短）。没压缩时与传入值相同。
	History []llm.Message
	// Summary 是被裁掉部分的摘要；空串表示本轮没有摘要可注入。
	Summary string
	// Compacted 表示本轮是否发生了「裁短历史」这件事（用于观测/日志）。
	Compacted bool
}

// maybeCompactHistory 决定并执行本轮的历史压缩。
//
// 传入的 history 是 chat.go 已经建好的 llm.Message 列表（不含 tool_trace）。
// 为了拿到每条消息的 id（用于记录摘要覆盖到哪），这里按会话重新查一次原始行。
// 查不到 / 没数据库 / 开关关着时一律原样返回，绝不阻断对话。
func maybeCompactHistory(ctx context.Context, conv *model.Conversation, history []llm.Message, modelID, key string) compactResult {
	out := compactResult{History: history}
	if !compactEnabled() || conv == nil || model.DB == nil {
		return out
	}

	keepN := compactIntEnv("AGENT_COMPACT_KEEP", compactKeepDefault)
	trigger := compactIntEnv("AGENT_COMPACT_TRIGGER", compactTriggerDefault)
	if trigger <= keepN {
		trigger = keepN + 1
	}
	if len(history) <= trigger {
		// 历史还不长，但已经存过摘要（比如之前压缩过、用户又接着聊）时，
		// 仍然把摘要带上，否则被裁掉的那段就永远看不到了。
		if strings.TrimSpace(conv.Summary) != "" {
			out.Summary = strings.TrimSpace(conv.Summary)
		}
		return out
	}

	// 取原始行以拿到 id。跳过 tool_trace，与 chat.go 建历史时的口径保持一致。
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ? AND role <> ?", conv.Id, roleToolTrace).
		Order("id asc").Find(&rows).Error; err != nil {
		log.Printf("[compact] 读取会话消息失败，本轮不压缩：%v", err)
		return out
	}
	if len(rows) != len(history) || len(rows) <= keepN {
		// 行数与 history 对不上（并发写入 / 口径不一致）时保守放弃，
		// 免得把「保留最近 N 条」算错、裁掉了不该裁的内容。
		if strings.TrimSpace(conv.Summary) != "" {
			out.Summary = strings.TrimSpace(conv.Summary)
		}
		return out
	}

	cut := len(rows) - keepN // rows[cut:] 保留，rows[:cut] 进入摘要
	coveredUpto := rows[cut-1].Id

	// 摘要已经覆盖到这一条之后的内容 → 直接复用，不必再调模型。
	if conv.SummaryUpto >= coveredUpto && strings.TrimSpace(conv.Summary) != "" {
		out.History = toLLMMessages(rows[cut:])
		out.Summary = strings.TrimSpace(conv.Summary)
		out.Compacted = true
		return out
	}

	// 距离上次摘要没攒够新消息就先用旧摘要顶着，避免每轮都重算。
	// 距上次摘要没攒够新消息就先用旧摘要顶着，避免每轮都重算。
	if !summarizeIsStale(rows[:cut], conv.SummaryUpto, compactMinNewMessages, strings.TrimSpace(conv.Summary) != "") {
		out.History = toLLMMessages(rows[cut:])
		out.Summary = strings.TrimSpace(conv.Summary)
		out.Compacted = true
		return out
	}

	summary, err := generateCompactSummary(ctx, modelID, key, strings.TrimSpace(conv.Summary), rows[:cut])
	if err != nil {
		// 失败就退全量历史：多花 token 好过丢上下文。
		log.Printf("[compact] 生成摘要失败，本轮不压缩：%v", err)
		if strings.TrimSpace(conv.Summary) != "" {
			out.Summary = strings.TrimSpace(conv.Summary)
		}
		return out
	}
	if strings.TrimSpace(summary) == "" {
		return out
	}

	// 落库：摘要与覆盖水位一起写，下次直接从水位判断要不要重算。
	if err := model.DB.Model(&model.Conversation{}).Where("id = ?", conv.Id).
		Updates(map[string]any{"summary": summary, "summary_upto": coveredUpto}).Error; err != nil {
		log.Printf("[compact] 保存摘要失败（不影响本轮）：%v", err)
	}
	conv.Summary = summary
	conv.SummaryUpto = coveredUpto

	out.History = toLLMMessages(rows[cut:])
	out.Summary = summary
	out.Compacted = true
	return out
}

// toLLMMessages 把库里的消息行投影成 llm.Message，角色映射与 chat.go 完全一致。
func toLLMMessages(rows []model.Message) []llm.Message {
	out := make([]llm.Message, 0, len(rows))
	for _, m := range rows {
		role := llm.RoleUser
		switch m.Role {
		case "assistant":
			role = llm.RoleAssistant
		case "system":
			role = llm.RoleSystem
		}
		out = append(out, llm.Message{Role: role, Content: m.Content})
	}
	return out
}

// summarizeIsStale 判断已有摘要是否已经跟不上：距离上次摘要又攒够了 minNew 条新消息。
// hasSummary=false 时永远返回 true——没有摘要就必须生成一个。
// 抽成纯函数是为了能脱离数据库单测这段决策。
func summarizeIsStale(covered []model.Message, summaryUpto uint, minNew int, hasSummary bool) bool {
	if !hasSummary {
		return true
	}
	if minNew <= 0 {
		return true
	}
	newSince := 0
	for _, r := range covered {
		if r.Id > summaryUpto {
			newSince++
		}
	}
	return newSince >= minNew
}

// generateCompactSummary 用一次模型调用把旧消息压成摘要。
// previous 非空时带上，让新摘要是在旧摘要基础上增量的，而不是从零重写。
func generateCompactSummary(ctx context.Context, modelID, key, previous string, rows []model.Message) (string, error) {
	if strings.TrimSpace(modelID) == "" {
		return "", fmt.Errorf("本轮没有可用模型")
	}

	var b strings.Builder
	b.WriteString("### 已有摘要\n")
	if previous == "" {
		b.WriteString("（无，这是第一次压缩）\n")
	} else {
		b.WriteString(previous)
		b.WriteString("\n")
	}
	b.WriteString("\n### 需要压缩的消息\n")
	for _, m := range rows {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(clipRunes(content, 1200))
		b.WriteString("\n")
	}

	out, err := memoryLLMCaller(modelID, key)(ctx, compactSystemPrompt, b.String())
	if err != nil {
		return "", err
	}
	return clipRunes(strings.TrimSpace(out), compactMaxRunes), nil
}
