package llm

import (
	"context"
	"fmt"

	einogemini "github.com/cloudwego/eino-ext/components/model/gemini"
	einomodel "github.com/cloudwego/eino/components/model"
	"google.golang.org/genai"
)

// NewGeminiBackend 用 Eino 的 gemini 组件实现 Google generateContent 协议。
// genai.Client 只依赖 APIKey 与 BaseURL（自定义网关），配置为空时走官方地址。
func NewGeminiBackend(ctx context.Context, ep Endpoint, apiKey string, req ChatRequest) (einomodel.BaseChatModel, error) {
	if apiKey == "" {
		return nil, ErrMissingKey
	}
	clientConfig := &genai.ClientConfig{APIKey: apiKey}
	if ep.BaseURL != "" {
		clientConfig.HTTPOptions = genai.HTTPOptions{BaseURL: ep.BaseURL}
	}
	client, err := genai.NewClient(ctx, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("gemini 客户端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}

	cfg := &einogemini.Config{Client: client, Model: req.Model}
	if req.MaxTokens > 0 {
		cfg.MaxTokens = &req.MaxTokens
	}
	if req.Temperature != 0 {
		t := float32(req.Temperature)
		cfg.Temperature = &t
	}
	cm, err := einogemini.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("gemini 后端初始化失败 (endpoint=%s): %w", ep.ID, err)
	}
	return cm, nil
}
