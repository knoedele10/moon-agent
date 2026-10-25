package llm

import (
	"context"
	"os"
	"strings"

	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// PromptCacheEnabled 控制 Claude 的 cache_control。
// 缺省开启；AGENT_PROMPT_CACHE=0/false/off/no 时不改请求，行为与未接缓存时一致。
func PromptCacheEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_PROMPT_CACHE"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// CacheOptions 只在 Claude 且开关开启时返回自动缓存选项。其它后端返回 nil。
func CacheOptions(backend string) []einomodel.Option {
	if backend != "claude" || !PromptCacheEnabled() {
		return nil
	}
	return []einomodel.Option{einoclaude.WithAutoCacheControl(&einoclaude.CacheControl{})}
}

// cachingModel 把自动缓存选项加到每一次 Claude 调用上。
// WithTools 返回的新实例同样包一层，避免工具绑定把缓存丢掉。
type cachingModel struct {
	inner einomodel.ToolCallingChatModel
}

func (m *cachingModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	next, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &cachingModel{inner: next}, nil
}

func (m *cachingModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	return m.inner.Generate(ctx, input, append(opts, CacheOptions("claude")...)...)
}

func (m *cachingModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, append(opts, CacheOptions("claude")...)...)
}

func maybeCache(backend string, cm einomodel.ToolCallingChatModel) einomodel.ToolCallingChatModel {
	if len(CacheOptions(backend)) == 0 || cm == nil {
		return cm
	}
	return &cachingModel{inner: cm}
}
