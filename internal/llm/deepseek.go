package llm

import (
	"context"
	"fmt"

	einodeepseek "github.com/cloudwego/eino-ext/components/model/deepseek"
	einomodel "github.com/cloudwego/eino/components/model"
)

// NewDeepSeekBackend uses Eino's native DeepSeek component for text models.
// The v0.1.7 component rejects image parts, while deepseek-flash is used by
// the desktop computer-use path for vision. Keep that model on Eino's OpenAI
// compatible component until the native component supports multimodal input.
func NewDeepSeekBackend(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	if req.Model != "deepseek-chat" && req.Model != "deepseek-reasoner" {
		return NewOpenAIBackend(ctx, ep, apiKey, req)
	}
	if apiKey == "" {
		return nil, ErrMissingKey
	}
	cm, err := einodeepseek.NewChatModel(ctx, &einodeepseek.ChatModelConfig{
		APIKey:      apiKey,
		BaseURL:     ep.BaseURL,
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: float32(req.Temperature),
	})
	if err != nil {
		return nil, fmt.Errorf("deepseek 后端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}
	return cm, nil
}
