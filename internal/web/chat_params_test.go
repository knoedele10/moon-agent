package web

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 本文件只测「对话参数」这一层：请求体归一化、上下文搬运，以及它贯通到
// 直连路径（llm.ChatRequest）与 agent 路径（agent.Config）的效果。
//
// 全部不依赖数据库：归一化是纯函数；直连路径只测装配请求的纯函数；
// agent 路径用脚本化的假 Streamer 记录下游拿到的请求。

func TestNormalizeChatParams(t *testing.T) {
	long := strings.Repeat("阿", chatSystemPromptMaxRunes+50)

	cases := []struct {
		name string
		in   chatParams
		want chatParams
	}{
		{
			name: "全缺省 = 零值（不设置任何参数）",
			in:   chatParams{},
			want: chatParams{},
		},
		{
			name: "system_prompt 去首尾空白",
			in:   chatParams{SystemPrompt: "  只用中文回答  "},
			want: chatParams{SystemPrompt: "只用中文回答"},
		},
		{
			name: "system_prompt 全空白 = 不加",
			in:   chatParams{SystemPrompt: "   \n\t "},
			want: chatParams{},
		},
		{
			name: "system_prompt 按 rune 截到 2000",
			in:   chatParams{SystemPrompt: long},
			want: chatParams{SystemPrompt: strings.Repeat("阿", chatSystemPromptMaxRunes)},
		},
		{
			name: "temperature 负值夹到 0（= 不设置）",
			in:   chatParams{Temperature: -1},
			want: chatParams{},
		},
		{
			name: "temperature 超上限夹到 2",
			in:   chatParams{Temperature: 3},
			want: chatParams{Temperature: 2},
		},
		{
			name: "temperature 边界内原样保留",
			in:   chatParams{Temperature: 1.5},
			want: chatParams{Temperature: 1.5},
		},
		{
			name: "temperature 边界值 2 保留",
			in:   chatParams{Temperature: 2},
			want: chatParams{Temperature: 2},
		},
		{
			name: "temperature NaN 当没传",
			in:   chatParams{Temperature: math.NaN()},
			want: chatParams{},
		},
		{
			name: "temperature +Inf 当没传",
			in:   chatParams{Temperature: math.Inf(1)},
			want: chatParams{},
		},
		{
			name: "max_tokens <=0 当没传",
			in:   chatParams{MaxTokens: -5},
			want: chatParams{},
		},
		{
			name: "max_tokens 取最小有效值 1",
			in:   chatParams{MaxTokens: chatMaxTokensMin},
			want: chatParams{MaxTokens: 1},
		},
		{
			name: "max_tokens 超上限夹到 65536",
			in:   chatParams{MaxTokens: chatMaxTokensMax + 1},
			want: chatParams{MaxTokens: chatMaxTokensMax},
		},
		{
			name: "max_tokens 边界值 65536 保留",
			in:   chatParams{MaxTokens: chatMaxTokensMax},
			want: chatParams{MaxTokens: chatMaxTokensMax},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeChatParams(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeChatParams(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
			if n := len([]rune(got.SystemPrompt)); n > chatSystemPromptMaxRunes {
				t.Fatalf("system_prompt 超过 %d 个 rune：%d", chatSystemPromptMaxRunes, n)
			}
		})
	}
}

func TestChatParamsContextRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	// 没设置过时取到零值。
	if got := chatParamsFromContext(c); got != (chatParams{}) {
		t.Fatalf("空上下文应取到零值，got %+v", got)
	}

	want := chatParams{SystemPrompt: "额外指令", Temperature: 1.2, MaxTokens: 512}
	setChatParams(c, want)
	if got := chatParamsFromContext(c); got != want {
		t.Fatalf("往返后 = %+v, want %+v", got, want)
	}

	// nil 上下文不 panic，取到零值。
	setChatParams(nil, want)
	if got := chatParamsFromContext(nil); got != (chatParams{}) {
		t.Fatalf("nil 上下文应取到零值，got %+v", got)
	}
}

func TestDirectChatRequest(t *testing.T) {
	base := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}

	t.Run("全零参数与改造前一致（历史原样、参数不设置）", func(t *testing.T) {
		req := directChatRequest(base, chatParams{})
		if len(req.Messages) != len(base) ||
			req.Messages[0].Role != base[0].Role || req.Messages[0].Content != base[0].Content {
			t.Fatalf("消息不应被改动：%+v", req.Messages)
		}
		if req.Temperature != 0 || req.MaxTokens != 0 {
			t.Fatalf("零值参数应保持 0，got temp=%v max=%d", req.Temperature, req.MaxTokens)
		}
	})

	t.Run("有 system_prompt 时前插一条 system，不覆盖已有 system", func(t *testing.T) {
		history := []llm.Message{
			{Role: llm.RoleSystem, Content: "内置提示词"},
			{Role: llm.RoleUser, Content: "你好"},
		}
		req := directChatRequest(history, chatParams{
			SystemPrompt: "只用中文回答",
			Temperature:  1.5,
			MaxTokens:    256,
		})
		if len(req.Messages) != 3 {
			t.Fatalf("应前插一条，长度 = %d, want 3", len(req.Messages))
		}
		if req.Messages[0].Role != llm.RoleSystem || req.Messages[0].Content != "只用中文回答" {
			t.Fatalf("首条应为额外 system，got %+v", req.Messages[0])
		}
		if req.Messages[1].Role != llm.RoleSystem || req.Messages[1].Content != "内置提示词" {
			t.Fatalf("已有 system 应原样保留在第二位，got %+v", req.Messages[1])
		}
		if req.Messages[2].Role != history[1].Role || req.Messages[2].Content != history[1].Content {
			t.Fatalf("用户消息应原样保留，got %+v", req.Messages[2])
		}
		if req.Temperature != 1.5 || req.MaxTokens != 256 {
			t.Fatalf("参数未带下去：temp=%v max=%d", req.Temperature, req.MaxTokens)
		}
	})

	t.Run("不改动调用方传入的历史切片", func(t *testing.T) {
		history := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}
		_ = directChatRequest(history, chatParams{SystemPrompt: "额外"})
		if len(history) != 1 || history[0].Role != llm.RoleUser {
			t.Fatalf("入参切片被改动了：%+v", history)
		}
	})
}

// chatParamsRecorder 记录 agent 循环每次发给模型的请求，供断言参数是否贯通。
type chatParamsRecorder struct {
	mu   sync.Mutex
	reqs []llm.ChatRequest
}

func (r *chatParamsRecorder) stream(_ context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()

	ch := make(chan llm.Event, 2)
	ch <- llm.Event{Type: llm.EventDelta, Content: "好的"}
	ch <- llm.Event{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

// TestRunAgentChatAppliesChatParams 钉住 agent 路径：
// 额外系统提示词追加在系统提示词最末尾（不替换内置），温度 / token 传到 llm.ChatRequest。
func TestRunAgentChatAppliesChatParams(t *testing.T) {
	agentTestRegistry(t)
	agentTestEnv(t, true, t.TempDir())

	rec := &chatParamsRecorder{}
	agent.SetStreamer(rec.stream)

	var events []agentTestEvent
	c := agentTestContext()
	setChatParams(c, normalizeChatParams(chatParams{
		SystemPrompt: "只用中文回答",
		Temperature:  1.5,
		MaxTokens:    256,
	}))
	conv := &model.Conversation{Id: 9, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}

	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", "你好", history, agentTestSender(&events))

	if len(rec.reqs) != 1 {
		t.Fatalf("应只调用一次模型，实际 %d 次", len(rec.reqs))
	}
	req := rec.reqs[0]
	if req.Temperature != 1.5 || req.MaxTokens != 256 {
		t.Fatalf("参数未贯通到 agent 请求：temp=%v max=%d", req.Temperature, req.MaxTokens)
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("系统提示词应在首位，got %+v", req.Messages)
	}
	sys := req.Messages[0].Content
	if !strings.Contains(sys, "你是 blog-my 的助手") {
		t.Fatalf("内置提示词不应被替换，got %q", sys)
	}
	if !strings.HasSuffix(sys, "【本次会话的额外指令】\n只用中文回答\n") {
		t.Fatalf("额外指令应追加在系统提示词末尾，got %q", sys)
	}
}

// TestRunAgentChatZeroChatParamsUnchanged 钉住向后兼容：不设任何对话参数时，
// agent 请求里的温度 / token 都是零值，系统提示词里也没有额外指令的小标题。
func TestRunAgentChatZeroChatParamsUnchanged(t *testing.T) {
	agentTestRegistry(t)
	agentTestEnv(t, true, t.TempDir())

	rec := &chatParamsRecorder{}
	agent.SetStreamer(rec.stream)

	var events []agentTestEvent
	c := agentTestContext()
	conv := &model.Conversation{Id: 10, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}

	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", "你好", history, agentTestSender(&events))

	if len(rec.reqs) != 1 {
		t.Fatalf("应只调用一次模型，实际 %d 次", len(rec.reqs))
	}
	req := rec.reqs[0]
	if req.Temperature != 0 || req.MaxTokens != 0 {
		t.Fatalf("未传参数时温度 / token 应保持 0，got temp=%v max=%d", req.Temperature, req.MaxTokens)
	}
	if len(req.Messages) == 0 {
		t.Fatal("应有系统提示词")
	}
	if strings.Contains(req.Messages[0].Content, "【本次会话的额外指令】") {
		t.Fatalf("未传 system_prompt 时不得出现额外指令块：%q", req.Messages[0].Content)
	}
}
