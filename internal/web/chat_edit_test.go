package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 本文件只测「会话/消息编辑」里的纯逻辑：标题规范化、regenerate 的保留切点、
// 导出 Markdown 的拼装、喂给模型的历史重建。
//
// 这四处都不碰数据库，所以单测不依赖 MySQL；需要真库的 handler 行为留给
// AGENT_IT 的端到端用例（见 web/agent_http_it_test.go 的开关约定）。

func TestNormalizeConversationTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"去首尾空格", "  新标题  ", "新标题", true},
		{"空标题", "   ", "", false},
		{"刚好 60", strings.Repeat("阿", 60), strings.Repeat("阿", 60), true},
		{"超长按 rune 截断", strings.Repeat("阿", 70), strings.Repeat("阿", 60), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeConversationTitle(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
			if n := len([]rune(got)); n > conversationTitleMaxRunes {
				t.Fatalf("超过 %d 个 rune：%d", conversationTitleMaxRunes, n)
			}
		})
	}
}

func TestRegenerateHistory(t *testing.T) {
	mk := func(id uint, role string) model.Message {
		return model.Message{Id: id, Role: role}
	}

	t.Run("丢掉最后一条 user 之后的 assistant 与 tool_trace", func(t *testing.T) {
		msgs := []model.Message{
			mk(1, "user"), mk(2, "assistant"), mk(3, roleToolTrace),
			mk(4, "user"), mk(5, "assistant"), mk(6, roleToolTrace),
		}
		kept, dropped := regenerateHistory(msgs)

		wantKept := []uint{1, 2, 3, 4}
		if len(kept) != len(wantKept) {
			t.Fatalf("kept 长度 = %d, want %d", len(kept), len(wantKept))
		}
		for i, id := range wantKept {
			if kept[i].Id != id {
				t.Fatalf("kept[%d].Id = %d, want %d", i, kept[i].Id, id)
			}
		}
		if kept[len(kept)-1].Role != "user" {
			t.Fatalf("kept 末条应为 user，got %s", kept[len(kept)-1].Role)
		}
		if len(dropped) != 2 || dropped[0].Id != 5 || dropped[1].Id != 6 {
			t.Fatalf("dropped 应为 [5 6]，got %+v", dropped)
		}
	})

	t.Run("末条就是 user 时没有可丢的", func(t *testing.T) {
		msgs := []model.Message{mk(1, "user"), mk(2, "assistant"), mk(3, "user")}
		kept, dropped := regenerateHistory(msgs)
		if len(kept) != 3 {
			t.Fatalf("kept 长度 = %d, want 3", len(kept))
		}
		if len(dropped) != 0 {
			t.Fatalf("dropped 应为空，got %+v", dropped)
		}
	})

	t.Run("没有 user 消息时 kept 为空", func(t *testing.T) {
		msgs := []model.Message{mk(1, "assistant"), mk(2, roleToolTrace)}
		kept, _ := regenerateHistory(msgs)
		if len(kept) != 0 {
			t.Fatalf("kept 应为空，got %+v", kept)
		}
	})

	t.Run("空会话", func(t *testing.T) {
		kept, dropped := regenerateHistory(nil)
		if len(kept) != 0 || len(dropped) != 0 {
			t.Fatalf("空输入应返回空，got kept=%v dropped=%v", kept, dropped)
		}
	})
}

func TestConversationExportMarkdown(t *testing.T) {
	conv := model.Conversation{Id: 7, Title: "数学推导", Model: "mock-echo"}
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	msgs := []model.Message{
		{Id: 1, Role: "user", Content: "求 $x^2$ 的导数"},
		{Id: 2, Role: roleToolTrace, Content: `[{"name":"calculator"}]`},
		{Id: 3, Role: "assistant", Content: "答案是 **2x** <ok>"},
	}

	got := conversationExportMarkdown(conv, msgs, now)

	want := "# 数学推导\n\n" +
		"> 模型：mock-echo · 导出时间：2026-10-13T10:00:00+08:00\n\n" +
		"## 用户\n\n求 $x^2$ 的导数\n\n" +
		"## 助手\n\n答案是 **2x** <ok>\n"
	if got != want {
		t.Fatalf("导出正文不一致\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "calculator") || strings.Contains(got, roleToolTrace) {
		t.Fatalf("tool_trace 不应出现在导出里：%q", got)
	}
	// 内容必须原样输出：Markdown/HTML 字符不能被转义。
	if !strings.Contains(got, "<ok>") {
		t.Fatalf("内容被转义了：%q", got)
	}
}

// TestSSESendFrame 钉住 /api/chat 与 regenerate 共用的 SSE 帧格式：
// 少一个空行、字段名不同，前端的事件解析就会挂，所以这层值得单独测。
func TestSSESendFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	sse := startSSE(c)
	if sse == nil {
		t.Fatal("httptest.ResponseRecorder 应支持 Flush")
	}
	sse.send("delta", gin.H{"content": "hi"})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	want := "event: delta\ndata: {\"content\":\"hi\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("SSE 帧 = %q, want %q", got, want)
	}
}

func TestRebuildLLMHistory(t *testing.T) {
	history := []model.Message{
		{Id: 1, Role: "user", Content: "你好"},
		{Id: 2, Role: roleToolTrace, Content: `[{"name":"read"}]`},
		{Id: 3, Role: "assistant", Content: "你好，有什么可以帮你？"},
		{Id: 4, Role: "system", Content: "系统提示"},
	}

	got := rebuildLLMHistory(history)
	if len(got) != 3 {
		t.Fatalf("tool_trace 应被跳过，长度 = %d, want 3：%+v", len(got), got)
	}
	want := []llm.Message{
		{Role: llm.RoleUser, Content: "你好"},
		{Role: llm.RoleAssistant, Content: "你好，有什么可以帮你？"},
		{Role: llm.RoleSystem, Content: "系统提示"},
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
