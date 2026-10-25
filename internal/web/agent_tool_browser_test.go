package web

import (
	"testing"

	"blog-my/internal/agent"
)

func toolNameSet(tools []agent.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, tool := range tools {
		out[tool.Name()] = true
	}
	return out
}

var browserToolNames = []string{
	"browser_open", "browser_snapshot", "browser_click",
	"browser_type", "browser_screenshot", "browser_network",
}

// 浏览器工具只在「辅助工作」这类全量会话里出现；
// 代码协作不联网、日常聊天不碰浏览器、计划档只列计划。
func TestBrowserToolsRegistration(t *testing.T) {
	work := toolNameSet(buildAgentTools(agentToolPolicy{UserID: "u1", Kind: KindWork, Mode: ModeAuto}))
	for _, name := range browserToolNames {
		if !work[name] {
			t.Fatalf("辅助工作模式应当注册 %s", name)
		}
	}

	code := toolNameSet(buildAgentTools(agentToolPolicy{UserID: "u1", Kind: KindCode, Mode: ModeAuto, Root: t.TempDir()}))
	for _, name := range browserToolNames {
		if code[name] {
			t.Fatalf("代码协作模式不该注册 %s（联网工具按 Kind 收紧）", name)
		}
	}

	plan := toolNameSet(buildAgentTools(agentToolPolicy{UserID: "u1", Kind: KindWork, Mode: ModePlan, Root: t.TempDir()}))
	for _, name := range browserToolNames {
		if plan[name] {
			t.Fatalf("计划档不该注册 %s", name)
		}
	}
}
