package web

import (
	"strings"
	"testing"
)

func TestAutoMemoryEnabledDefaultsOn(t *testing.T) {
	t.Setenv("AGENT_AUTO_MEMORY", "")
	if !autoMemoryEnabled() {
		t.Fatal("默认应当开启（用户要求自动记住对话）")
	}
	for _, off := range []string{"0", "false", "OFF", "no"} {
		t.Setenv("AGENT_AUTO_MEMORY", off)
		if autoMemoryEnabled() {
			t.Fatalf("%q 应当关闭自动记忆", off)
		}
	}
	t.Setenv("AGENT_AUTO_MEMORY", "1")
	if !autoMemoryEnabled() {
		t.Fatal("1 应当开启")
	}
}

func TestClipRunesCutsByRune(t *testing.T) {
	if got := clipRunes("你好世界", 2); got != "你好…" {
		t.Fatalf("应当按字符截断并加省略号: %q", got)
	}
	if got := clipRunes("  hi  ", 10); got != "hi" {
		t.Fatalf("应当去掉首尾空白: %q", got)
	}
}

func TestAutoMemoryTextIncludesUserActionAndReply(t *testing.T) {
	trace := []agentTraceEntry{
		{Name: "write", OK: true, Summary: "Created notes/a.md (8 字节)"},
		{Name: "delete", OK: false, Summary: "失败 — 文件不存在"},
	}
	got := autoMemoryText("把 demo 文件建好再删掉", trace, "已经处理完了。")

	if !strings.Contains(got, "用户说：把 demo 文件建好再删掉") {
		t.Fatalf("应当记录用户原话: %q", got)
	}
	if !strings.Contains(got, "write（成功）") || !strings.Contains(got, "delete（失败）") {
		t.Fatalf("应当记录每个工具及成败: %q", got)
	}
	if !strings.Contains(got, "AI 的回复：已经处理完了。") {
		t.Fatalf("应当记录回复要点: %q", got)
	}
}

// 关闭自动记忆时，saveAutoMemory 必须在碰 kb() 之前就返回——
// 否则单测会因为去连本地 Ollama 而变慢甚至挂住。
func TestSaveAutoMemoryDisabledReturnsEarly(t *testing.T) {
	t.Setenv("AGENT_AUTO_MEMORY", "0")
	saveAutoMemory("u1", "", "", "这是一句足够长的话，本来会被记录", nil, "回复")
}

// 没有工具调用、用户这句话又很短（寒暄）时也不该记录。
func TestSaveAutoMemorySkipsChitchat(t *testing.T) {
	t.Setenv("AGENT_AUTO_MEMORY", "1")
	saveAutoMemory("u1", "", "", "在吗", nil, "在的")
}
