package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// Opt-in local inference check. It uses the real tool policy and runtime with
// only the four tools needed by this task, avoiding unrelated services/personas.
func TestAutonomousLocalModel(t *testing.T) {
	modelID := strings.TrimSpace(os.Getenv("AGENT_LIVE_MODEL"))
	if modelID == "" {
		t.Skip("set AGENT_LIVE_MODEL to an installed Ollama model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	root := t.TempDir()
	baseURL := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	reg := llm.NewRegistry()
	reg.SetEndpoints([]llm.Endpoint{{ID: "local-verification", Backend: "ollama", BaseURL: baseURL, Models: []string{modelID}}})
	cm, err := reg.NewToolCallingModel(ctx, modelID, "", llm.ChatRequest{MaxTokens: 4096, Temperature: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	var tools []agent.Tool
	for _, tool := range buildAgentTools(agentToolPolicy{Root: root, Kind: KindWork, Mode: ModeAuto}) {
		switch tool.Name() {
		case "read", "write", "list", "ask_user":
			tools = append(tools, tool)
		}
	}
	const content = "自主执行验证：你好，世界。"
	prompt := "请在当前工作区创建 autonomy-check.txt，内容必须精确为：" + content + "（不含这段括号说明）。随后用 read 工具读回核对，完成后简短汇报。直接执行。/no_think"
	wrote, verified, questioned, done := false, false, false, false
	err = agent.Run(ctx, agent.Config{Model: modelID, EinoModel: cm, Tools: tools,
		SystemPrompt:  "你是文件任务执行助手。只能调用提供的四个工具；写入后必须读回核对。" + modePrompt(ModeAuto, root),
		MaxIterations: 6, MaxToolCalls: 12, LimitRecoveryRounds: 2, Exec: &agent.Exec{Root: root, SessionID: "local-autonomy-check"}},
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}}, func(ev agent.Event) {
			if ev.Kind == agent.EvToolResult {
				t.Logf("tool=%s ok=%v", ev.ToolName, ev.OK)
				wrote = wrote || ev.ToolName == "write" && ev.OK
				verified = verified || wrote && ev.ToolName == "read" && ev.OK
			}
			questioned = questioned || ev.Kind == agent.EvNeedInput
			done = done || ev.Kind == agent.EvDone
		})
	if err != nil {
		t.Fatalf("local model run failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "autonomy-check.txt"))
	if err != nil || string(data) != content || !wrote || !verified || questioned || !done {
		t.Fatalf("local task not fulfilled: file=%q readErr=%v write=%v verify=%v asked=%v done=%v", data, err, wrote, verified, questioned, done)
	}
}
