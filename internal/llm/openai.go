package llm

import (
	"context"
	"fmt"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
)

// NewOpenAIBackend 用 Eino 的 openai 组件实现 OpenAI 兼容协议，
// 服务 DeepSeek / OpenAI / Kimi / 通义 / 智谱 等端点。
func NewOpenAIBackend(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	if apiKey == "" {
		return nil, ErrMissingKey
	}
	temp := float32(req.Temperature)
	cm, err := einoopenai.NewChatModel(ctx, &einoopenai.ChatModelConfig{
		APIKey:      apiKey,
		BaseURL:     ep.BaseURL,
		Model:       req.Model,
		MaxTokens:   intPtr(req.MaxTokens),
		Temperature: &temp,
	})
	if err != nil {
		return nil, fmt.Errorf("openai 后端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}
	return cm, nil
}

func intPtr(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}
