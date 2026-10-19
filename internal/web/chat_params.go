package web

import (
	"math"
	"strings"

	"github.com/gin-gonic/gin"
)

// 「对话参数」是与单次请求绑定的可选生成参数：额外的系统提示词、温度、最大输出长度。
//
// 为什么走 gin.Context 而不是给 streamReply / runAgentChat 加参数：
// runAgentChat 已有 7 个参数且被多处调用，加参数要改一圈签名；助手（assistant_resolve.go）
// 已经证明「请求级配置放上下文」这条路可行。这里沿用同一写法。
//
// 三个字段全为「零值」时，下一次生成的行为必须与改造前**完全一致**：
// SystemPrompt 空 = 不加；Temperature 0 = 不设置（沿用既有约定，见下）；MaxTokens 0 = 不设置。
const (
	// chatSystemPromptMaxRunes 是用户额外系统提示词的 rune 上限。太长会挤掉内置提示词。
	chatSystemPromptMaxRunes = 2000
	// chatTemperatureMax 是温度上限（下限为 0）。
	chatTemperatureMax = 2.0
	// chatMaxTokensMin / Max 是最大输出 token 的可接受范围。
	chatMaxTokensMin = 1
	chatMaxTokensMax = 65536
)

// chatParams 是一次请求的生成参数（归一化之后的最终形态）。
//
// Temperature 沿用既有约定：0（含缺省）表示「不设置」，交给后端默认值——
// 各 llm 后端都是 `if req.Temperature != 0`。所以这里不发明 new sentinel。
type chatParams struct {
	SystemPrompt string
	Temperature  float64
	MaxTokens    int
}

// normalizeChatParams 把请求体里原样解析出的三字段归一化：
//
//   - system_prompt：trim；按 rune 截到 2000；空串就是不加。
//   - temperature：NaN/Inf 当没传；越界夹到 [0, 2]（3 → 2，-1 → 0）。
//   - max_tokens：<=0 当没传（0）；上限 65536。
//
// 纯函数、不碰依赖，便于单测钉住边界。
func normalizeChatParams(raw chatParams) chatParams {
	out := chatParams{SystemPrompt: strings.TrimSpace(raw.SystemPrompt)}
	if out.SystemPrompt != "" {
		out.SystemPrompt = truncateRunes(out.SystemPrompt, chatSystemPromptMaxRunes)
	}

	temp := raw.Temperature
	switch {
	case math.IsNaN(temp), math.IsInf(temp, 0):
		temp = 0
	case temp < 0:
		temp = 0
	case temp > chatTemperatureMax:
		temp = chatTemperatureMax
	}
	out.Temperature = temp

	switch {
	case raw.MaxTokens <= 0:
		out.MaxTokens = 0
	case raw.MaxTokens > chatMaxTokensMax:
		out.MaxTokens = chatMaxTokensMax
	default:
		out.MaxTokens = raw.MaxTokens
	}
	return out
}

// ctxChatParamsKey 是本轮请求的 chatParams 在 gin.Context 里的键。
const ctxChatParamsKey = "ctxChatParams"

// setChatParams 由 handler 在进入生成路径之前调用，把本轮参数记进请求上下文。
func setChatParams(c *gin.Context, p chatParams) {
	if c == nil {
		return
	}
	c.Set(ctxChatParamsKey, p)
}

// chatParamsFromContext 取本轮请求携带的对话参数；没有就是零值（不设置任何参数）。
func chatParamsFromContext(c *gin.Context) chatParams {
	if c == nil {
		return chatParams{}
	}
	v, ok := c.Get(ctxChatParamsKey)
	if !ok {
		return chatParams{}
	}
	p, _ := v.(chatParams)
	return p
}
