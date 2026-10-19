package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestDeepSeekNativeTextAndVisionFallback(t *testing.T) {
	ep := Endpoint{ID: "deepseek", Backend: "deepseek", BaseURL: "https://api.deepseek.com/v1"}
	for _, tc := range []struct {
		model string
		want  string
	}{
		{model: "deepseek-chat", want: "deepseek.ChatModel"},
		{model: "deepseek-reasoner", want: "deepseek.ChatModel"},
		{model: "deepseek-flash", want: "openai.ChatModel"},
		{model: "deepseek-v4-pro", want: "openai.ChatModel"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			cm, err := NewDeepSeekBackend(context.Background(), ep, "test-key", ChatRequest{Model: tc.model})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%T", cm); !strings.Contains(got, tc.want) {
				t.Fatalf("模型组件 = %s，想要 %s", got, tc.want)
			}
		})
	}
}
