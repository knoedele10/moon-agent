package web

import (
	"testing"

	"blog-my/internal/agent"
)

// The child must receive a real build tool only when its autonomy mode permits it.
func TestChildCommandToolPolicy(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		mode string
		want string
	}{
		{ModeAuto, "executable"},
		{ModeConfirm, "denied"},
		{ModePlan, "absent"},
	} {
		tools := subagentToolsFor("user", agent.SubagentRequest{Root: root, Kind: KindCode, Mode: tc.mode}, subagentMaxDepth())
		var found agent.Tool
		for _, tool := range tools {
			if tool.Name() == "run_command" {
				found = tool
				break
			}
		}
		switch tc.want {
		case "executable":
			if _, ok := found.(runCommandTool); !ok {
				t.Fatalf("%s: run_command should be executable, got %T", tc.mode, found)
			}
		case "denied":
			if _, ok := found.(subagentDenyTool); !ok {
				t.Fatalf("%s: run_command should be denied, got %T", tc.mode, found)
			}
		case "absent":
			if found != nil {
				t.Fatalf("%s: run_command should be absent, got %T", tc.mode, found)
			}
		}
	}
}
