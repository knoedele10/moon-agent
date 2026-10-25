package evals

// 这几条是真正的「任务级」评测：用真实文件工具 + 脚本化模型跑完整 agent 循环，
// 断言磁盘终态、工具序列和步数预算。脚本模型不产生真实 token 账单，所以这里的
// 成本维度用「模型轮数」近似：每多一轮就是一次完整上下文重发。
//
// 真模型 smoke（需要供应商密钥）不在 CI 里跑，见 docs/verify。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/llm"
)

type evalStep struct {
	text  string
	calls []llm.ToolCall
}

type evalModel struct {
	mu    sync.Mutex
	steps []evalStep
	next  int
}

func (m *evalModel) stream(_ context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.next >= len(m.steps) {
		return nil, fmt.Errorf("eval 脚本已用尽（第 %d 次模型调用）", m.next+1)
	}
	step := m.steps[m.next]
	m.next++
	ch := make(chan llm.Event, len(step.calls)+2)
	if step.text != "" {
		ch <- llm.Event{Type: llm.EventDelta, Content: step.text}
	}
	for i := range step.calls {
		call := step.calls[i]
		ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &call}
	}
	ch <- llm.Event{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

func (m *evalModel) turns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next
}

func evalCall(id, name string, args map[string]any) llm.ToolCall {
	raw, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Arguments: string(raw)}
}

type evalScenario struct {
	id string
	// maxTurns 是模型轮数上限：超过说明 agent 在原地打转。
	maxTurns int
	// maxSeconds 是墙钟预算：脚本模型下应当秒级完成。
	maxSeconds time.Duration
	fixture    func(t *testing.T, root string)
	steps      []evalStep
	assert     func(t *testing.T, root string, results []agent.Event)
}

func runEvalScenario(t *testing.T, sc evalScenario) {
	t.Helper()
	root := t.TempDir()
	if sc.fixture != nil {
		sc.fixture(t, root)
	}
	model := &evalModel{steps: sc.steps}
	agent.SetStreamer(model.stream)
	t.Cleanup(func() { agent.SetStreamer(nil) })

	started := time.Now()
	var results []agent.Event
	tools := append(agentfs.ToolsWithMutation(), agentfs.SearchTools()...)
	err := agent.Run(context.Background(), agent.Config{
		Model: "eval-model",
		Tools: tools,
		Exec:  &agent.Exec{SessionID: "eval-" + sc.id, TurnID: sc.id, Root: root},
	}, []llm.Message{{Role: llm.RoleUser, Content: "按脚本执行任务"}}, func(ev agent.Event) {
		if ev.Kind == agent.EvToolResult {
			results = append(results, ev)
		}
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if sc.maxTurns > 0 && model.turns() > sc.maxTurns {
		t.Fatalf("模型轮数 %d 超过预算 %d", model.turns(), sc.maxTurns)
	}
	if elapsed := time.Since(started); elapsed > sc.maxSeconds {
		t.Fatalf("耗时 %s 超过预算 %s", elapsed, sc.maxSeconds)
	}
	sc.assert(t, root, results)
}

func TestLoopScenarios(t *testing.T) {
	cases := []struct {
		id  string
		run func(t *testing.T)
	}{
		{"fix-config-value", taskFixConfig},
		{"unread-edit-selfcorrects", taskUnreadEdit},
		{"write-weekly-report", taskWeeklyReport},
		{"organize-rename-file", taskOrganize},
	}
	for _, tc := range cases {
		t.Run(tc.id, tc.run)
	}
}

func taskFixConfig(t *testing.T) {
	runEvalScenario(t, evalScenario{
		id:         "fix-config-value",
		maxTurns:   5,
		maxSeconds: 15 * time.Second,
		fixture: func(t *testing.T, root string) {
			writeEvalFile(t, filepath.Join(root, "config.txt"), "alpha\nfoo=1\nomega\n")
		},
		steps: []evalStep{
			{calls: []llm.ToolCall{
				evalCall("g1", "grep", map[string]any{"pattern": "foo"}),
				evalCall("r1", "read", map[string]any{"file_path": "config.txt"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("e1", "edit", map[string]any{"file_path": "config.txt", "old_string": "foo=1", "new_string": "foo=2"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("r2", "read", map[string]any{"file_path": "config.txt"}),
			}},
			{text: "已把 foo 改成 2，并读回确认。"},
		},
		assert: func(t *testing.T, root string, results []agent.Event) {
			if len(results) != 4 {
				t.Fatalf("应有 4 个工具结果，实际 %d", len(results))
			}
			for _, ev := range results {
				if !ev.OK {
					t.Fatalf("工具不应失败：%s %s", ev.ToolName, ev.Detail)
				}
			}
			if got := readEvalFile(t, filepath.Join(root, "config.txt")); !strings.Contains(got, "foo=2") {
				t.Fatalf("文件内容未更新：%q", got)
			}
		},
	})
}

func taskUnreadEdit(t *testing.T) {
	runEvalScenario(t, evalScenario{
		id:         "unread-edit-selfcorrects",
		maxTurns:   6,
		maxSeconds: 15 * time.Second,
		fixture: func(t *testing.T, root string) {
			writeEvalFile(t, filepath.Join(root, "notes.txt"), "old line\n")
		},
		steps: []evalStep{
			// 故意没读就改：必须被拒绝，且模型能从错误里学会先读。
			{calls: []llm.ToolCall{
				evalCall("e0", "edit", map[string]any{"file_path": "notes.txt", "old_string": "old line", "new_string": "new line"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("r1", "read", map[string]any{"file_path": "notes.txt"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("e1", "edit", map[string]any{"file_path": "notes.txt", "old_string": "old line", "new_string": "new line"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("r2", "read", map[string]any{"file_path": "notes.txt"}),
			}},
			{text: "先读后改，已完成。"},
		},
		assert: func(t *testing.T, root string, results []agent.Event) {
			if len(results) != 4 {
				t.Fatalf("应有 4 个工具结果，实际 %d", len(results))
			}
			if results[0].OK {
				t.Fatal("未读就改应当失败")
			}
			for _, ev := range results[1:] {
				if !ev.OK {
					t.Fatalf("自纠后不应再失败：%s %s", ev.ToolName, ev.Detail)
				}
			}
			if got := readEvalFile(t, filepath.Join(root, "notes.txt")); got != "new line\n" {
				t.Fatalf("最终内容 = %q", got)
			}
		},
	})
}

func taskWeeklyReport(t *testing.T) {
	runEvalScenario(t, evalScenario{
		id:         "write-weekly-report",
		maxTurns:   4,
		maxSeconds: 15 * time.Second,
		fixture: func(t *testing.T, root string) {
			writeEvalFile(t, filepath.Join(root, "notes.md"), "周一：修登录\n周二：写文档\n")
		},
		steps: []evalStep{
			{calls: []llm.ToolCall{
				evalCall("r1", "read", map[string]any{"file_path": "notes.md"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("w1", "write", map[string]any{
					"file_path": "weekly.md",
					"content":   "# 周报\n\n- 修登录\n- 写文档\n",
				}),
			}},
			{calls: []llm.ToolCall{
				evalCall("r2", "read", map[string]any{"file_path": "weekly.md"}),
			}},
			{text: "周报已生成：weekly.md"},
		},
		assert: func(t *testing.T, root string, results []agent.Event) {
			if len(results) != 3 {
				t.Fatalf("应有 3 个工具结果，实际 %d", len(results))
			}
			got := readEvalFile(t, filepath.Join(root, "weekly.md"))
			if !strings.Contains(got, "修登录") || !strings.Contains(got, "写文档") {
				t.Fatalf("周报内容缺失：%q", got)
			}
		},
	})
}

func taskOrganize(t *testing.T) {
	runEvalScenario(t, evalScenario{
		id:         "organize-rename-file",
		maxTurns:   5,
		maxSeconds: 15 * time.Second,
		fixture: func(t *testing.T, root string) {
			writeEvalFile(t, filepath.Join(root, "inbox", "note.txt"), "待归档\n")
		},
		steps: []evalStep{
			{calls: []llm.ToolCall{
				evalCall("l1", "list", map[string]any{"path": "inbox"}),
				evalCall("r1", "read", map[string]any{"file_path": "inbox/note.txt"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("m1", "move", map[string]any{"from_path": "inbox/note.txt", "to_path": "archive/note.txt"}),
			}},
			{calls: []llm.ToolCall{
				evalCall("l2", "list", map[string]any{"path": "archive"}),
			}},
			{text: "已归档到 archive/note.txt"},
		},
		assert: func(t *testing.T, root string, results []agent.Event) {
			if len(results) != 4 {
				t.Fatalf("应有 4 个工具结果，实际 %d", len(results))
			}
			if _, err := os.Stat(filepath.Join(root, "archive", "note.txt")); err != nil {
				t.Fatalf("归档后的文件不存在：%v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "inbox", "note.txt")); !os.IsNotExist(err) {
				t.Fatalf("源文件应已移走：%v", err)
			}
		},
	})
}
