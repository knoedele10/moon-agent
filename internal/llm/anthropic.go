package llm

import (
	"context"
	"fmt"

	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einomodel "github.com/cloudwego/eino/components/model"
)

const anthropicMaxTokensFallback = 4096

// NewClaudeBackend 用 Eino 的 claude 组件实现 Anthropic Messages 协议。
// 注意 Anthropic 必填 max_tokens：调用方未指定时回落 4096（与手写版行为一致）。
func NewClaudeBackend(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	if apiKey == "" {
		return nil, ErrMissingKey
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokensFallback
	}
	var temp *float32
	if req.Temperature != 0 {
		t := float32(req.Temperature)
		temp = &t
	}
	baseURL := ep.BaseURL
	cm, err := einoclaude.NewChatModel(ctx, &einoclaude.Config{
		APIKey:      apiKey,
		BaseURL:     &baseURL,
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Temperature: temp,
	})
	if err != nil {
		return nil, fmt.Errorf("claude 后端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}
	return cm, nil
}
