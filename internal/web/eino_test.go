package web

import (
	"os"
	"path/filepath"
	"testing"

	"blog-my/internal/llm"
	"blog-my/internal/model"
)

func TestEinoAgentWritesThroughExistingToolPolicy(t *testing.T) {
	agentTestRegistry(t)
	root := t.TempDir()
	agentTestEnv(t, true, root)
	t.Setenv("AGENT_ENGINE", "eino")
	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "eino-write-1", Name: "write", Arguments: `{"file_path":"a.txt","content":"hello from Eino"}`}}},
		{Text: "已写入。"},
	})
	var events []agentTestEvent
	c := agentTestContext()
	conv := &model.Conversation{Id: 1, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "写入 a.txt"}}
	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", history[0].Content, history, agentTestSender(&events))
	data, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("Eino 写入失败：%v；事件：%+v", err, events)
	}
	if string(data) != "hello from Eino" {
		t.Fatalf("写入内容 = %q；事件：%+v", string(data), events)
	}
}
