package llm

import (
	"context"
	"strings"
	"sync"

	einoschema "github.com/cloudwego/eino/schema"
)

// Usage 是一轮模型请求的 token 账。流式分片里的数字通常是累计值，
// 同一轮内取最大；跨轮由调用方相加。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedTokens == 0
}

// MaxUsage 合并同一轮流式分片里的累计用量。
func MaxUsage(a, b Usage) Usage {
	if b.InputTokens > a.InputTokens {
		a.InputTokens = b.InputTokens
	}
	if b.OutputTokens > a.OutputTokens {
		a.OutputTokens = b.OutputTokens
	}
	if b.CachedTokens > a.CachedTokens {
		a.CachedTokens = b.CachedTokens
	}
	return a
}

func UsageFromMessage(msg *einoschema.Message) Usage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return Usage{}
	}
	u := msg.ResponseMeta.Usage
	return Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		CachedTokens: u.PromptTokenDetails.CachedTokens,
	}
}

type usageKey struct{}

type usageBox struct {
	mu sync.Mutex
	v  Usage
}

// WithUsage 在 ctx 上挂一个累加器。没挂时 Observe/Add 是空操作。
func WithUsage(ctx context.Context) (context.Context, *usageBox) {
	if ctx == nil {
		ctx = context.Background()
	}
	if box, ok := ctx.Value(usageKey{}).(*usageBox); ok && box != nil {
		return ctx, box
	}
	box := &usageBox{}
	return context.WithValue(ctx, usageKey{}, box), box
}

func boxFrom(ctx context.Context) *usageBox {
	if ctx == nil {
		return nil
	}
	box, _ := ctx.Value(usageKey{}).(*usageBox)
	return box
}

// AddUsage 把一整轮的用量加进 ctx。同一轮不要重复加。
func AddUsage(ctx context.Context, u Usage) {
	box := boxFrom(ctx)
	if box == nil || u.IsZero() {
		return
	}
	box.mu.Lock()
	box.v.InputTokens += u.InputTokens
	box.v.OutputTokens += u.OutputTokens
	box.v.CachedTokens += u.CachedTokens
	box.mu.Unlock()
}

func (b *usageBox) Snapshot() Usage {
	if b == nil {
		return Usage{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}

// EstimateUSD 用公开标价的粗略数量级估算美元成本，只用于界面提醒。
// 未知模型返回 0, false，避免编造账单。
func EstimateUSD(model string, u Usage) (float64, bool) {
	inPrice, outPrice, cachedPrice, ok := pricePerMillion(model)
	if !ok {
		return 0, false
	}
	uncached := u.InputTokens - u.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	cost := float64(uncached)*inPrice + float64(u.CachedTokens)*cachedPrice + float64(u.OutputTokens)*outPrice
	return cost / 1_000_000, true
}

func pricePerMillion(model string) (input, output, cached float64, ok bool) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "deepseek"):
		return 0.27, 1.10, 0.07, true
	case strings.Contains(m, "claude"):
		return 3, 15, 0.30, true
	case strings.Contains(m, "gpt-4o"):
		return 2.5, 10, 1.25, true
	case strings.Contains(m, "gpt-4.1"):
		return 2, 8, 0.5, true
	default:
		return 0, 0, 0, false
	}
}
