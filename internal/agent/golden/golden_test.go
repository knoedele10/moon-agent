// Package golden 是 Agent 的离线 golden set：用真实 fs 工具 + 脚本化模型跑完整循环，
// 断言「模型看到的反馈」与「磁盘终态」，而不是只测单个工具。
//
// 为什么单独成包：internal/agent 的包内测试没法 import internal/agent/fs（会成环），
// 而这里站在外部把 agent + fs 组装起来，正好覆盖 ACI 的端到端行为：
// 先检索再读、没读就改会被拒绝且能自纠、压缩/截断提示可见、改完必须验证。
package golden

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/llm"
)

type scriptStep struct {
	text  string
	calls []llm.ToolCall
}

type scriptedModel struct {
	mu    sync.Mutex
	steps []scriptStep
	i     int
	seen  [][]llm.Message
}

func (m *scriptedModel) stream(_ context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, append([]llm.Message(nil), req.Messages...))
	if m.i >= len(m.steps) {
		return nil, fmt.Errorf("golden 脚本已用尽（第 %d 次模型调用）", m.i+1)
	}
	step := m.steps[m.i]
	m.i++
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

func (m *scriptedModel) requests() [][]llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]llm.Message(nil), m.seen...)
}

func toolCall(id, name string, args map[string]any) llm.ToolCall {
	raw, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Arguments: string(raw)}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runScenario 用真实 fs 工具执行一个脚本；返回事件与最终文本。
func runScenario(t *testing.T, root string, model *scriptedModel, history []llm.Message) ([]agent.Event, string) {
	t.Helper()
	agent.SetStreamer(model.stream)
	t.Cleanup(func() { agent.SetStreamer(nil) })

	tools := append(agentfs.ToolsWithMutation(), agentfs.SearchTools()...)
	var events []agent.Event
	err := agent.Run(context.Background(), agent.Config{
		Model: "golden-model",
		Tools: tools,
		Exec:  &agent.Exec{SessionID: t.Name(), Root: root},
	}, history, func(ev agent.Event) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	done := ""
	for _, ev := range events {
		if ev.Kind == agent.EvDone {
			done = ev.Content
		}
	}
	return events, done
}

func toolResults(events []agent.Event) []agent.Event {
	var out []agent.Event
	for _, ev := range events {
		if ev.Kind == agent.EvToolResult {
			out = append(out, ev)
		}
	}
	return out
}

// 场景 1：先 grep 定位，再 read，再 edit，最后读回验证。
// 这是编码任务最常见的路径；缺任何一步都应该在断言里暴露。
func TestGoldenSearchReadEditVerify(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "config.txt"), "alpha\nfoo=1\nomega\n")
	model := &scriptedModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			toolCall("g1", "grep", map[string]any{"pattern": "foo"}),
			toolCall("r1", "read", map[string]any{"file_path": "config.txt"}),
		}},
		{calls: []llm.ToolCall{
			toolCall("e1", "edit", map[string]any{"file_path": "config.txt", "old_string": "foo=1", "new_string": "foo=2"}),
		}},
		{calls: []llm.ToolCall{
			toolCall("r2", "read", map[string]any{"file_path": "config.txt"}),
		}},
		{text: "已把 foo 改成 2，并读回确认。"},
	}}
	events, done := runScenario(t, root, model, []llm.Message{{Role: llm.RoleUser, Content: "把 foo 改成 2"}})

	results := toolResults(events)
	if len(results) != 4 {
		t.Fatalf("应有 4 个工具结果，实际 %d：%+v", len(results), results)
	}
	for _, ev := range results {
		if !ev.OK {
			t.Fatalf("工具不应失败：%+v", ev)
		}
	}
	if !strings.Contains(results[0].Detail, "config.txt:2:") {
		t.Fatalf("grep 应命中第 2 行：%q", results[0].Detail)
	}
	if !strings.Contains(results[2].Detail, "修改区域回显") || !strings.Contains(results[2].Detail, "foo=2") {
		t.Fatalf("edit 应回显修改区域：%q", results[2].Detail)
	}
	if got := readFile(t, filepath.Join(root, "config.txt")); got != "alpha\nfoo=2\nomega\n" {
		t.Fatalf("磁盘终态不对：%q", got)
	}
	if !strings.Contains(done, "读回确认") {
		t.Fatalf("最终答复应基于真实执行：%q", done)
	}
}

// 场景 2：没 read 就 edit 会被拒绝；模型应能从错误文案自纠，而不是原样重试。
func TestGoldenEditWithoutReadRecovers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "old\n")
	model := &scriptedModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			toolCall("e0", "edit", map[string]any{"file_path": "a.txt", "old_string": "old", "new_string": "new"}),
		}},
		{calls: []llm.ToolCall{
			toolCall("r1", "read", map[string]any{"file_path": "a.txt"}),
		}},
		{calls: []llm.ToolCall{
			toolCall("e1", "edit", map[string]any{"file_path": "a.txt", "old_string": "old", "new_string": "new"}),
		}},
		{calls: []llm.ToolCall{
			toolCall("r2", "read", map[string]any{"file_path": "a.txt"}),
		}},
		{text: "先读后改，并读回确认。"},
	}}
	events, _ := runScenario(t, root, model, []llm.Message{{Role: llm.RoleUser, Content: "改 a.txt"}})
	results := toolResults(events)
	if len(results) != 4 {
		t.Fatalf("应有 4 个工具结果（拒绝/读/改/读回），实际 %d", len(results))
	}
	if results[0].OK || !strings.Contains(results[0].Detail, "has not been read") {
		t.Fatalf("第一次 edit 应被观察守卫拒绝：%+v", results[0])
	}
	for _, ev := range results[1:] {
		if !ev.OK {
			t.Fatalf("读后重试应成功：%+v", results)
		}
	}
	if got := readFile(t, filepath.Join(root, "a.txt")); got != "new\n" {
		t.Fatalf("磁盘终态不对：%q", got)
	}
}

// 场景 3：grep 命中过多时必须给出截断与收窄提示，而不是静默截断。
func TestGoldenGrepLimitTellsModelToNarrow(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "needle %d\n", i)
	}
	writeFile(t, filepath.Join(root, "many.txt"), b.String())
	model := &scriptedModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			toolCall("g1", "grep", map[string]any{"pattern": "needle", "limit": 3}),
		}},
		{text: "命中太多，先看到前三条。"},
	}}
	events, _ := runScenario(t, root, model, []llm.Message{{Role: llm.RoleUser, Content: "找 needle"}})
	results := toolResults(events)
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("grep 应成功：%+v", results)
	}
	for _, want := range []string{"已截断", "还有更多匹配", "缩小范围"} {
		if !strings.Contains(results[0].Detail, want) {
			t.Errorf("截断提示缺少 %q：%q", want, results[0].Detail)
		}
	}
}

// 场景 4：write 成功后直接收尾，必须被「修改后验证」提醒拦一次；
// 提醒后仍不给验证也要允许收尾，不能死循环。
func TestGoldenWriteRequiresVerificationOnce(t *testing.T) {
	root := t.TempDir()
	model := &scriptedModel{steps: []scriptStep{
		{calls: []llm.ToolCall{
			toolCall("w1", "write", map[string]any{"file_path": "new.txt", "content": "hello"}),
		}},
		{text: "已经写好了。"},
		{text: "已写入，但没有验证。"},
	}}
	_, done := runScenario(t, root, model, []llm.Message{{Role: llm.RoleUser, Content: "创建 new.txt"}})
	if got := readFile(t, filepath.Join(root, "new.txt")); got != "hello" {
		t.Fatalf("文件未创建：%q", got)
	}
	reqs := model.requests()
	if len(reqs) != 3 {
		t.Fatalf("应发生 3 次模型调用（写入 / 收尾 / 提醒后再收尾），实际 %d", len(reqs))
	}
	last := reqs[2]
	sawReminder := false
	for _, msg := range last {
		if strings.Contains(msg.Content, "验证") || strings.Contains(msg.Content, "核对") {
			sawReminder = true
		}
	}
	if !sawReminder {
		t.Fatalf("收尾前的请求应带验证提醒：%+v", last)
	}
	if !strings.Contains(done, "没有验证") {
		t.Fatalf("最终答复应保留模型的如实说明：%q", done)
	}
}
