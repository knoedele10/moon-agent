package llm

import (
	"context"
	"fmt"
	"time"

	einoollama "github.com/cloudwego/eino-ext/components/model/ollama"
	einomodel "github.com/cloudwego/eino/components/model"
)

// ollama 默认服务地址。eino-ext 的 ollama 组件不会自动填默认值，
// BaseURL 为空时需要本侧兜底。
const ollamaDefaultBaseURL = "http://localhost:11434"

// NewOllamaBackend 用 Eino 的 ollama 组件调本地模型。
// 本地服务无需 APIKey，apiKey 参数忽略。
func NewOllamaBackend(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	baseURL := ep.BaseURL
	if baseURL == "" {
		baseURL = ollamaDefaultBaseURL
	}
	cfg := &einoollama.ChatModelConfig{
		BaseURL: baseURL,
		Model:   req.Model,
		Timeout: 2 * time.Minute,
	}
	// 只有调用方显式传了参数才设置 Options：全零时不设置，保持与改造前一致。
	// eino-ext 的 ollama 组件用 ChatModelConfig.Options（= api.Options）承载采样参数：
	// 温度 → Options.Temperature（JSON temperature），最大输出 → Options.NumPredict（JSON num_predict）。
	if req.Temperature != 0 || req.MaxTokens > 0 {
		opts := &einoollama.Options{}
		if req.Temperature != 0 {
			opts.Temperature = float32(req.Temperature)
		}
		if req.MaxTokens > 0 {
			opts.NumPredict = req.MaxTokens
		}
		cfg.Options = opts
	}
	cm, err := einoollama.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("ollama 后端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}
	return cm, nil
}
