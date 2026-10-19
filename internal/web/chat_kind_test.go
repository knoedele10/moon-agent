package web

import (
	"testing"

	agentfs "blog-my/internal/agent/fs"
)

// 四个模式的单测。这些判错了的后果是"切到陪伴模式却发现还能删文件"——
// 用户以为收窄了权限，实际没有。

func TestNormalizeKind(t *testing.T) {
	cases := map[string]string{
		"":       KindWork, // 空串 = work：升级前建的会话必须行为不变
		"chat":   KindChat,
		"CHAT":   KindChat,
		" work ": KindWork,
		"code":   KindCode,
		"learn":  KindLearn,
		"bogus":  KindWork, // 未知值回落 work
	}
	for in, want := range cases {
		if got := normalizeKind(in); got != want {
			t.Errorf("normalizeKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKindProfilesMemoryScope(t *testing.T) {
	// 陪伴与学习算"生活"，工作与代码算"工作"——这是记忆分域的判据。
	cases := map[string]string{
		KindChat:  scopeLife,
		KindLearn: scopeLife,
		KindWork:  scopeWork,
		KindCode:  scopeWork,
	}
	for kind, want := range cases {
		if got := profileFor(kind).MemoryScope; got != want {
			t.Errorf("%s 的 memory scope = %q, want %q", kind, got, want)
		}
	}
}

// 每个模式都必须有自己的提示词段落，否则模型不知道自己在哪个模式里。
func TestKindProfilesHavePrompt(t *testing.T) {
	for _, kind := range []string{KindChat, KindWork, KindCode, KindLearn} {
		if len(profileFor(kind).Prompt) < 20 {
			t.Errorf("%s 缺少模式提示词", kind)
		}
	}
}

// 核心断言：陪伴模式**绝不能**拿到写类/删除/联网/SQL 工具。
func TestChatKindDropsDangerousTools(t *testing.T) {
	// 用生产用的真实工具集（手搓清单容易漏掉记忆工具）。
	all := agentToolsFor("u1", t.TempDir(), ModeAuto, true, subagentMaxDepth())

	got := filterToolsByKind(all, KindChat)
	names := map[string]bool{}
	for _, tl := range got {
		names[tl.Name()] = true
	}
	for _, banned := range []string{"write", "edit", "move", "delete", "office_edit", "web_search", "web_reader", "sql_query"} {
		if names[banned] {
			t.Errorf("陪伴模式不该拿到 %s", banned)
		}
	}
	// 但记忆、时间、计算器必须留着——不然陪伴模式连"现在几点"都答不了。
	for _, want := range []string{"memory_search", "memory_write", "datetime", "calculator", "ask_user"} {
		if !names[want] {
			t.Errorf("陪伴模式应当保留 %s", want)
		}
	}
}

// 学习模式能写笔记（仍受工作区沙箱约束），但不能删除或移动。
func TestLearnKindKeepsWriteButDropsDelete(t *testing.T) {
	all := agentfs.ToolsWithMutation()
	names := map[string]bool{}
	for _, tl := range filterToolsByKind(all, KindLearn) {
		names[tl.Name()] = true
	}
	if !names["write"] || !names["edit"] {
		t.Error("学习模式应当能写笔记（write/edit）")
	}
	if names["delete"] || names["move"] {
		t.Error("学习模式不该有删除与移动能力")
	}
}

// 工作模式是**白名单为 nil = 全部保留**：升级前的行为必须逐字不变。
func TestWorkKindKeepsEverything(t *testing.T) {
	all := agentfs.ToolsWithMutation()
	got := filterToolsByKind(all, KindWork)
	if len(got) != len(all) {
		t.Fatalf("工作模式应当原样返回全部工具：want %d, got %d", len(all), len(got))
	}
}
