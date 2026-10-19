package llm

import "strings"

// DefaultEndpoints 返回内置的端点清单。
//
// EnvKey 只是"服务端默认 key 从哪个环境变量读"，用户在前台自己填的 key 会覆盖它。
// 加新厂商时在这里加一条即可，前提是它属于已有的某个 Backend。
func DefaultEndpoints() []Endpoint {
	return []Endpoint{
		{
			ID: "deepseek", Label: "DeepSeek", Backend: "deepseek",
			BaseURL: "https://api.deepseek.com/v1",
			EnvKey:  "DEEPSEEK_API_KEY",
			// deepseek-flash 实测支持视觉，deepseek-v4-pro 实测不支持（见文件末尾的 visionModels）。
			Models: []string{"deepseek-chat", "deepseek-reasoner", "deepseek-flash", "deepseek-v4-pro"},
		},
		{
			ID: "openai", Label: "OpenAI", Backend: "openai",
			BaseURL: "https://api.openai.com/v1",
			EnvKey:  "OPENAI_API_KEY",
			Models:  []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			ID: "xai", Label: "Grok (xAI)", Backend: "openai",
			BaseURL: "https://api.x.ai/v1",
			EnvKey:  "XAI_API_KEY",
			// Official Chat Completions model; supports streaming and function calling.
			Models: []string{"grok-4.7"},
		},
		{
			ID: "moonshot", Label: "Kimi (Moonshot)", Backend: "openai",
			BaseURL: "https://api.moonshot.cn/v1",
			EnvKey:  "MOONSHOT_API_KEY",
			Models:  []string{"moonshot-v1-8k", "moonshot-v1-32k"},
		},
		{
			ID: "qwen", Label: "通义千问", Backend: "openai",
			BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
			EnvKey:  "DASHSCOPE_API_KEY",
			Models:  []string{"qwen-plus", "qwen-max", "qwen-turbo"},
		},
		{
			ID: "zhipu", Label: "智谱 GLM", Backend: "openai",
			BaseURL: "https://open.bigmodel.cn/api/paas/v4",
			EnvKey:  "ZHIPU_API_KEY",
			Models:  []string{"glm-4-plus", "glm-4-flash"},
		},
		{
			ID: "anthropic", Label: "Claude", Backend: "claude",
			BaseURL: "https://api.anthropic.com",
			EnvKey:  "ANTHROPIC_API_KEY",
			Models:  []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"},
		},
		{
			ID: "gemini", Label: "Gemini", Backend: "gemini",
			BaseURL: "https://generativelanguage.googleapis.com",
			EnvKey:  "GEMINI_API_KEY",
			Models:  []string{"gemini-2.0-flash", "gemini-1.5-pro"},
		},
		{
			ID: "ernie", Label: "文心一言", Backend: "openai",
			// 文心千帆 v2 已提供 OpenAI 兼容端点，直接走 openai 后端：
			// 不再做两段式 token 交换，用户填的 key 作为 bearer 使用。
			// 注意 v2 的 key 在千帆控制台 v2 页面生成，与旧版 "API_KEY:SECRET_KEY" 不同。
			BaseURL: "https://qianfan.baidubce.com/v2",
			EnvKey:  "ERNIE_API_KEY",
			Models:  []string{"ernie-4.0-8k", "ernie-3.5-8k"},
		},
		{
			// 火山方舟：ark 后端本侧未实现，仅占位。
			// Route 到它时不会 panic——Registry.Stream 对未注册后端返回 ErrNoBackend。
			ID: "ark", Label: "火山方舟", Backend: "ark",
			EnvKey: "ARK_API_KEY",
			Models: []string{"doubao-pro-32k"},
		},
		{
			// Ollama 本地模型：无需 key，BaseURL 指向本机服务，可在前台改成局域网地址。
			ID: "ollama", Label: "Ollama 本地模型", Backend: "ollama",
			BaseURL: "http://localhost:11434",
			EnvKey:  "",
			// 本机 Ollama 模型：通用对话、代码、推理及视觉模型。
			Models: []string{"qwen3.5:4b", "qwen2.5:7b", "qwen3:4b", "qwen2.5vl:7b"},
		},
		{
			// 不需要任何 key，用于在没有真实凭证时验证整条流式链路。
			ID: "mock", Label: "本地回声（测试用）", Backend: "mock",
			Models: []string{"mock-echo"},
		},
	}
}

// visionModels 是官方文档或实测确认支持视觉的模型白名单。
//
// 名字启发式（vl / vision / gemma3）认不出 deepseek-flash 这种名字，而 CUA 必须有视觉，
// 所以视觉能力必须显式标注，不能靠猜。实测（2026-10-11，白底 + 左边蓝圆 + 右边红方）：
//   - deepseek-flash ：答出「白底 / 左蓝圆 / 右红方」，视觉正确；
//   - deepseek-v4-pro：答「无法查看你上传的图片」，不支持视觉，故刻意不在此白名单里。
var visionModels = map[string]bool{
	"deepseek-flash": true,
	"qwen3.5:4b":     true,
	// https://docs.x.ai/developers/models/grok-4.7 documents text/image input.
	"grok-4.7": true,
}

// ModelSupportsVision 判断模型是否支持视觉：先查实测白名单，再回落名字启发式。
// 白名单优先，是为了让「名字看不出、但实测能看图」的模型也能被 CUA 选中。
func ModelSupportsVision(model string) bool {
	if visionModels[model] {
		return true
	}
	m := strings.ToLower(model)
	return strings.Contains(m, "vl") || strings.Contains(m, "vision") || strings.Contains(m, "gemma3")
}
