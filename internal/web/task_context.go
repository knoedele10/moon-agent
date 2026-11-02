package web

import (
	"os"
	"strconv"
	"strings"
)

// modelContextTokens 返回某个模型的上下文窗口（token）。
//
// 压缩要按「这个模型能装多少」触发，而不是按消息条数：十条带大段工具输出的消息
// 可能已经超过小模型的窗口，而一百条短消息对大窗口模型毫无压力。
//
// 优先级：AGENT_MODEL_CONTEXT_TOKENS（"模型前缀=token,..."，最长前缀匹配）
// → 内置的常见模型前缀表 → AGENT_CONTEXT_TOKENS（全局）→ 32000 保守默认。
func modelContextTokens(modelID string) int {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if n := matchContextPrefix(id, parseContextOverrides(os.Getenv("AGENT_MODEL_CONTEXT_TOKENS"))); n > 0 {
		return n
	}
	if n := matchContextPrefix(id, builtinContextWindows); n > 0 {
		return n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_CONTEXT_TOKENS"))); err == nil && n > 0 {
		return n
	}
	return 32000
}

// builtinContextWindows 只列能确定的公开窗口；匹配不到时走环境变量或保守默认。
var builtinContextWindows = map[string]int{
	"claude":            200000,
	"gpt-4o":            128000,
	"gpt-4.1":           1000000,
	"gpt-5":             400000,
	"o3":                200000,
	"o4":                200000,
	"gemini":            1000000,
	"deepseek":          128000,
	"qwen":              128000,
	"glm":               128000,
	"kimi":              128000,
	"moonshot":          128000,
	"doubao":            128000,
	"llama":             8192,
	"mistral":           32000,
	"mock":              32000,
}

func parseContextOverrides(raw string) map[string]int {
	out := map[string]int{}
	for _, part := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = n
	}
	return out
}

func matchContextPrefix(id string, table map[string]int) int {
	best, bestLen := 0, -1
	for prefix, n := range table {
		if prefix == "" {
			continue
		}
		// 允许 "provider/model" 形式：前缀可出现在最后一个斜杠之后。
		name := id
		if i := strings.LastIndex(id, "/"); i >= 0 {
			name = id[i+1:]
		}
		if (strings.HasPrefix(id, prefix) || strings.HasPrefix(name, prefix)) && len(prefix) > bestLen {
			best, bestLen = n, len(prefix)
		}
	}
	return best
}

// compactTokenTrigger 是入口压缩的 token 阈值：窗口的一半。
// 留出另一半给系统提示、工具定义、本轮工具输出和回复；循环内压缩在 60% 再兜一次。
func compactTokenTrigger(modelID string) int {
	if n := compactIntEnv("AGENT_COMPACT_TOKENS", 0); n > 0 {
		return n
	}
	return modelContextTokens(modelID) / 2
}
