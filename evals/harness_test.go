package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent/rules"
	"blog-my/internal/agent/runstore"
	"blog-my/internal/agent/sandbox"
	"blog-my/internal/agent/skill"
	"blog-my/internal/llm"
)

// 这些任务用脚本断言代替真模型：CI 里不打供应商，也能证明规则、沙箱、用量和续跑决策还在。
func TestScriptedTasks(t *testing.T) {
	tasks := []struct {
		id string
		fn func(t *testing.T)
	}{
		{"rules-scaffold", taskRulesScaffold},
		{"sandbox-deny-shell", taskSandboxDenyShell},
		{"usage-estimate", taskUsageEstimate},
		{"run-reconcile-no-replay", taskReconcileNoReplay},
		{"skill-meta", taskSkillMeta},
	}
	for _, task := range tasks {
		t.Run(task.id, task.fn)
	}
}

func taskRulesScaffold(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, created, err := rules.Scaffold(dir, false)
	if err != nil || !created {
		t.Fatalf("scaffold = %s %v %v", path, created, err)
	}
	loaded, err := rules.LoadFrom(dir, "", 0)
	if err != nil || !strings.Contains(loaded.Content, "go test") {
		t.Fatalf("rules = %q %v", loaded.Content, err)
	}
}

func taskSandboxDenyShell(t *testing.T) {
	p := sandbox.Policy{}
	if err := p.CheckExec("cmd.exe"); err == nil {
		t.Fatal("cmd.exe 应被拒绝")
	}
	if err := p.CheckExec("go"); err != nil {
		t.Fatal(err)
	}
}

func taskUsageEstimate(t *testing.T) {
	cost, ok := llm.EstimateUSD("claude-3-5-sonnet", llm.Usage{InputTokens: 1000, OutputTokens: 1000})
	if !ok || cost <= 0 {
		t.Fatalf("estimate = %v %v", cost, ok)
	}
	if _, ok := llm.EstimateUSD("unknown-model", llm.Usage{InputTokens: 10}); ok {
		t.Fatal("未知模型不应编造价格")
	}
}

func taskReconcileNoReplay(t *testing.T) {
	dir := t.TempDir()
	st, err := runstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Start("run-1", "s", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append("run-1", "s", "tool_started", runstore.StatusRunning, map[string]any{
		"id": "c1", "tool": "write", "effect": "non_idempotent_side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != runstore.StatusNeedsConfirm {
		t.Fatalf("reconcile = %#v", rows)
	}
	events, err := st.Read("run-1")
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, ev := range events {
		if ev.Type == "tool_started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("对账不应再启动工具，tool_started=%d", started)
	}
	last := events[len(events)-1]
	if last.Type != "run_reconciled" || !strings.Contains(string(last.Payload), `"replay":false`) {
		t.Fatalf("last = %s %s", last.Type, last.Payload)
	}
}

func taskSkillMeta(t *testing.T) {
	meta := skill.ParseMeta("---\nname: weekly\ndescription: 周报\nversion: 0.1.0\nallowed-tools: Read\n---\n\nbody\n")
	if meta.Version != "0.1.0" || len(meta.AllowedTools) != 1 {
		t.Fatalf("%#v", meta)
	}
}
