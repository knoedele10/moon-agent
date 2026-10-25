package web

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/agent/rules"
	"blog-my/internal/agent/runstore"
	"blog-my/internal/model"
)

func TestPromptOmitsRulesWhenNoneLoaded(t *testing.T) {
	t.Setenv("MOON_GLOBAL_RULES", filepath.Join(t.TempDir(), "missing.md"))
	root := t.TempDir()
	loaded, err := rules.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	prompt := agentSystemPrompt(root, "")
	if strings.TrimSpace(loaded.Content) == "" && strings.Contains(prompt, "[Project rules from AGENTS.md]") {
		t.Fatal("没有规则时不应注入规则标题")
	}
	again := agentSystemPrompt(root, "")
	if prompt != again {
		t.Fatal("同一工作区、没有规则时，提示词应逐字节稳定")
	}
}

func TestRunCommandSandboxDeniesCmd(t *testing.T) {
	t.Setenv("AGENT_SANDBOX", "1")
	t.Setenv("AGENT_SANDBOX_AUDIT", "")
	root := t.TempDir()
	res, err := (runCommandTool{}).Execute(t.Context(), []byte(`{"commands":[{"program":"cmd.exe","args":["/c","echo","hi"]}]}`), &agent.Exec{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "cmd.exe") {
		t.Fatalf("沙箱应拒绝 cmd.exe，实际 %#v", res)
	}
}

func TestSessionSearchSQLDialect(t *testing.T) {
	t.Setenv("DB_DRIVER", "sqlite")
	if got := sessionLocateSQL(); got != "instr(m.content, ?)" {
		t.Fatalf("sqlite locate = %s", got)
	}
	if !strings.Contains(sessionSnippetSQL(), "substr(") {
		t.Fatalf("sqlite snippet = %s", sessionSnippetSQL())
	}
	t.Setenv("DB_DRIVER", "mysql")
	if model.Driver() != "mysql" {
		t.Fatal(model.Driver())
	}
	if got := sessionLocateSQL(); got != "LOCATE(?, m.content)" {
		t.Fatalf("mysql locate = %s", got)
	}
}

func TestCollectArtifactsDedupesPaths(t *testing.T) {
	events := []runstore.Event{
		{Type: "tool_started", Payload: json.RawMessage(`{"tool":"write"}`)},
		{Type: "tool_finished", Payload: json.RawMessage(`{"tool":"write","path":"a.txt","ok":true}`)},
		{Type: "tool_finished", Payload: json.RawMessage(`{"tool":"write","path":"a.txt","ok":true}`)},
		{Type: "tool_finished", Payload: json.RawMessage(`{"tool":"edit","path":"b.txt","ok":false}`)},
	}
	got := collectArtifacts(events)
	if len(got) != 2 {
		t.Fatalf("artifacts = %#v", got)
	}
	if got[0]["path"] != "a.txt" || got[1]["path"] != "b.txt" {
		t.Fatalf("paths = %#v", got)
	}
}
