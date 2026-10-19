package web

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"blog-my/internal/model"
)

// 跨轮工具历史（借鉴 Cyrene 的 CTA 设计，这里只做最小可用版）。
//
// 背景：历史重建过去把 role=tool_trace 整条丢掉，于是模型跨轮看不到自己上一轮
// 调过什么工具——「接着改刚才那个」这类连续推理直接失忆，只能重新读一遍。
// 折中做法：把轨迹压成一行摘要（**丢弃 Detail**，它可能很长，塞回去会污染上下文），
// 作为运行时旁注注入历史，并且只注入最近若干轮，避免注记本身把上下文吃掉。
//
// 本文件全是纯函数（不碰 DB / 不碰网络），方便单测钉住兼容性与边界。

const (
	// envTraceNoteTurns 是注入最近多少轮轨迹注记的开关；<=0 表示一条都不注入。
	envTraceNoteTurns = "AGENT_TRACE_NOTE_TURNS"
	// defaultTraceNoteTurns 是未配置该环境变量时的轮数。
	defaultTraceNoteTurns = 6

	// traceNoteHeader 是每条注记的开头，明确告诉模型「这是工具轨迹，不是对话正文」。
	traceNoteHeader = "[上一轮你执行过的工具]"

	// traceSummaryLimit 是写操作（edit / write / ...）摘要保留的 rune 数。
	traceSummaryLimit = 120
	// traceReadOnlyLimit 是只读工具摘要保留的 rune 数：只读结果对后续推理的价值
	// 低于写操作，留短一点，省上下文。
	traceReadOnlyLimit = 80
	// traceMaxEntries 是单条注记里最多列出的工具调用数，超出只报个数。
	traceMaxEntries = 6
)

// readOnlyTools 是纯只读工具名单：它们不改变任何状态，注记可以给得更省。
var readOnlyTools = map[string]bool{
	"read":          true,
	"list":          true,
	"web_search":    true,
	"web_reader":    true,
	"memory_search": true,
	"skill_search":  true,
	"skill_read":    true,
}

// isReadOnlyTool 判断一个工具名是不是纯只读工具（大小写、首尾空格不敏感）。
func isReadOnlyTool(name string) bool {
	return readOnlyTools[strings.ToLower(strings.TrimSpace(name))]
}

// traceNoteTurns 读环境变量 AGENT_TRACE_NOTE_TURNS。
// 未配置或解析失败 → 默认 6；<=0 原样返回（调用方据此完全不注入）。
func traceNoteTurns() int {
	raw := strings.TrimSpace(os.Getenv(envTraceNoteTurns))
	if raw == "" {
		return defaultTraceNoteTurns
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultTraceNoteTurns
	}
	return n
}

// toolTraceNote 把一条 tool_trace 的 JSON 压成给模型看的简短说明。
// 返回空串表示这条轨迹不值得注入（解析失败 / 空数组 / 所有条目都没摘要）。
//
// 轨迹的 JSON 形状见 web/agent.go 的 agentTraceEntry：`[{name,ok,summary,detail}]`。
// Detail 一律丢弃（可能很长）；每条摘要按 rune 截断（只读 80 字、其余 120 字），
// 整块最多列 6 条，超出只写一行「另有 N 次调用未列出」。
//
// 解析失败一律返回空串：一条脏数据绝不能打断历史重建。
func toolTraceNote(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	var entries []agentTraceEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return ""
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		summary := strings.TrimSpace(e.Summary)
		// 没有摘要的条目对模型没有任何信息量（比如只落了 {name} 的老数据），跳过。
		if name == "" || summary == "" {
			continue
		}
		limit := traceSummaryLimit
		if isReadOnlyTool(name) {
			limit = traceReadOnlyLimit
		}
		lines = append(lines, "- "+name+"（"+traceStatusWord(e.OK)+"）："+truncateRunes(summary, limit))
	}
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(traceNoteHeader)
	shown := lines
	if len(shown) > traceMaxEntries {
		shown = shown[:traceMaxEntries]
	}
	for _, line := range shown {
		b.WriteString("\n")
		b.WriteString(line)
	}
	if extra := len(lines) - traceMaxEntries; extra > 0 {
		b.WriteString("\n（另有 ")
		b.WriteString(strconv.Itoa(extra))
		b.WriteString(" 次调用未列出）")
	}
	return b.String()
}

// traceStatusWord 把轨迹里的 ok 布尔转成给人看的中文，模型读起来比 true/false 明确。
func traceStatusWord(ok bool) string {
	if ok {
		return "成功"
	}
	return "失败"
}

// recentTraceIndexes 从后往前数 tool_trace，返回最近 limit 条在 history 里的下标集合。
// limit <= 0 时返回空集合，也就是一条轨迹注记都不注入（等价于改造前的行为）。
//
// 只注入最近若干轮的理由：轨迹是每轮一条，聊 50 轮就是 50 条注记，
// 不加限制的话注记本身就会把上下文吃掉。
func recentTraceIndexes(history []model.Message, limit int) map[int]bool {
	keep := make(map[int]bool)
	if limit <= 0 {
		return keep
	}
	seen := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != roleToolTrace {
			continue
		}
		seen++
		if seen > limit {
			break
		}
		keep[i] = true
	}
	return keep
}
