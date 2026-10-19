package web

import (
	"strings"
	"testing"

	"blog-my/internal/llm"
)

// 检索查询要带上最近几轮用户消息：只看当前这句，“那个方案怎么样”这种指代查不到东西。
func TestMemoryQuery(t *testing.T) {
	// 没有历史：原样返回当前这句。
	if got := memoryQuery("端口是多少", nil); got != "端口是多少" {
		t.Fatalf("无历史时应当原样返回，实际 %q", got)
	}

	history := []llm.Message{
		{Role: llm.RoleUser, Content: "我们在聊部署方案，端口定了 8300"},
		{Role: llm.RoleAssistant, Content: "好的，我记下了"}, // 助手消息不该进查询
		{Role: llm.RoleUser, Content: "那它什么时候上线"},
	}
	got := memoryQuery("那它什么时候上线", history)

	if !strings.Contains(got, "端口定了 8300") {
		t.Fatalf("应当带上上一轮用户消息作为上下文，实际 %q", got)
	}
	if strings.Contains(got, "我记下了") {
		t.Fatalf("助手消息不该进检索查询，实际 %q", got)
	}
	// 当前这句只出现一次（不能因为历史里也有它就重复）。
	if strings.Count(got, "那它什么时候上线") != 1 {
		t.Fatalf("当前这句应当只出现一次，实际 %q", got)
	}
}
