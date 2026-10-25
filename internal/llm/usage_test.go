package llm

import (
	"context"
	"testing"

	einoschema "github.com/cloudwego/eino/schema"
)

func TestUsageAccumulatesAcrossRoundsAndMaxesWithinARound(t *testing.T) {
	ctx, box := WithUsage(context.Background())
	round := MaxUsage(Usage{InputTokens: 10, OutputTokens: 1}, Usage{InputTokens: 12, OutputTokens: 4, CachedTokens: 8})
	AddUsage(ctx, round)
	AddUsage(ctx, Usage{InputTokens: 3, OutputTokens: 1, CachedTokens: 1})
	got := box.Snapshot()
	if got.InputTokens != 15 || got.OutputTokens != 5 || got.CachedTokens != 9 {
		t.Fatalf("got %+v", got)
	}
	AddUsage(context.Background(), Usage{InputTokens: 9})
	if box.Snapshot().InputTokens != 15 {
		t.Fatal("usage without a box should be ignored")
	}
}

func TestUsageFromMessage(t *testing.T) {
	msg := &einoschema.Message{ResponseMeta: &einoschema.ResponseMeta{Usage: &einoschema.TokenUsage{
		PromptTokens: 20, CompletionTokens: 5,
		PromptTokenDetails: einoschema.PromptTokenDetails{CachedTokens: 7},
	}}}
	got := UsageFromMessage(msg)
	if got.InputTokens != 20 || got.OutputTokens != 5 || got.CachedTokens != 7 {
		t.Fatalf("got %+v", got)
	}
	if !UsageFromMessage(nil).IsZero() {
		t.Fatal("nil message should be zero")
	}
}

func TestEstimateUSDKnownAndUnknown(t *testing.T) {
	cost, ok := EstimateUSD("deepseek-chat", Usage{InputTokens: 1_000_000, CachedTokens: 0, OutputTokens: 0})
	if !ok || cost < 0.2 || cost > 0.3 {
		t.Fatalf("deepseek estimate = %v ok=%v", cost, ok)
	}
	if _, ok := EstimateUSD("local-ollama", Usage{InputTokens: 10}); ok {
		t.Fatal("unknown model must not invent a price")
	}
}
