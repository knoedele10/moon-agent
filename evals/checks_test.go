package evals

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/agent/hooks"
	"blog-my/internal/agent/rules"
	"blog-my/internal/agent/runstore"
	"blog-my/internal/agent/sandbox"
	"blog-my/internal/agent/trigger"
	"blog-my/internal/llm"
	"blog-my/internal/obs"
)

// 这一组是「一条任务 = 一个可复现的验收断言」。它们不打模型，跑的是每个能力真正的
// 入口函数；真模型 smoke 另见 docs/verify（需要供应商密钥，不进 CI）。

func writeEvalFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEvalFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// 规则叠加：全局在前、项目在后，近的覆盖远的（模型按顺序读，后写的优先）。
func taskRulesLayering(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "app")
	global := filepath.Join(root, "global.md")
	writeEvalFile(t, global, "全局：回答用中文。")
	writeEvalFile(t, filepath.Join(child, "AGENTS.md"), "项目：改完跑 go test ./...。")

	res, err := rules.LoadFrom(child, global, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Documents) != 2 {
		t.Fatalf("应当读到两份规则，实际 %d", len(res.Documents))
	}
	if !strings.HasSuffix(filepath.ToSlash(res.Documents[0].Path), "global.md") {
		t.Fatalf("全局规则应在最前，实际 %q", res.Documents[0].Path)
	}
	if !strings.Contains(res.Content, "回答用中文") || !strings.Contains(res.Content, "go test ./...") {
		t.Fatalf("内容缺失：%q", res.Content)
	}
	// 未配置时不应改变提示词：空工作区返回空内容。
	empty, err := rules.LoadFrom("", "", 0)
	if err != nil || empty.Content != "" {
		t.Fatalf("空工作区应当是 no-op：%q %v", empty.Content, err)
	}
}

// AGENTS.md 里的 verify 块要被解析成命令，未知命令留给执行层过滤。
func taskRulesVerifyCommands(t *testing.T) {
	root := t.TempDir()
	writeEvalFile(t, filepath.Join(root, "AGENTS.md"), "<!-- moon:verify\nnpm run build\ngo test ./...\n# 注释忽略\n-->\n")
	rules.ResetVerifyState()
	cmds, err := rules.VerifyCommands(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("应当解析出 2 条验证命令，实际 %#v", cmds)
	}
	if cmds[0][0] != "npm" || cmds[1][0] != "go" {
		t.Fatalf("命令顺序不对：%#v", cmds)
	}
	// 每轮只跑一次：同一 turnID 第二次返回空串（这里没有命令可跑，验证不 panic 即可）。
	if out := rules.MaybeVerify(context.Background(), root, "turn-1", "read", false); out != "" {
		t.Fatalf("read 之后不应触发验证：%q", out)
	}
}

// 沙箱写策略：工作区内放行，工作区外拒绝；交互式 shell 永远拒绝。
func taskSandboxWriteBoundary(t *testing.T) {
	root := t.TempDir()
	p := sandbox.Policy{WriteRoots: []string{root}}
	if err := p.CheckWrite(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("工作区内应放行：%v", err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := p.CheckWrite(outside); err == nil {
		t.Fatal("工作区外写入应被拒绝")
	}
	if err := p.CheckExec("cmd.exe"); err == nil {
		t.Fatal("cmd.exe 应始终拒绝")
	}
}

// 多轮用量累加：同一轮取最大（流式分片是累计值），跨轮相加。
func taskUsageAcrossRounds(t *testing.T) {
	ctx, box := llm.WithUsage(context.Background())
	round1 := llm.MaxUsage(llm.Usage{InputTokens: 100, OutputTokens: 20}, llm.Usage{InputTokens: 120, OutputTokens: 25, CachedTokens: 80})
	llm.AddUsage(ctx, round1)
	llm.AddUsage(ctx, llm.Usage{InputTokens: 30, OutputTokens: 5, CachedTokens: 10})
	got := box.Snapshot()
	if got.InputTokens != 150 || got.OutputTokens != 30 || got.CachedTokens != 90 {
		t.Fatalf("累加结果不对：%+v", got)
	}
	if cost, ok := llm.EstimateUSD("deepseek-chat", got); !ok || cost < 0 {
		t.Fatalf("应能估算成本：%v %v", cost, ok)
	}
	// 没有累加器时 AddUsage 必须是空操作（后台任务路径可能没挂 box）。
	llm.AddUsage(context.Background(), llm.Usage{InputTokens: 999})
}

// 强杀恢复：未完成的只读/幂等调用标 recovered；未知副作用才 needs_confirm。
func taskReconcileIdempotent(t *testing.T) {
	st, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Start("r1", "s", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// read_only 的调用没 finish：重启后可以直接标 recovered，不重放。
	if err := st.Append("r1", "s", "tool_started", runstore.StatusRunning, map[string]any{
		"id": "c1", "tool": "read", "effect": "read_only",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != runstore.StatusRecovered {
		t.Fatalf("reconcile = %#v", rows)
	}
	events, err := st.Read("r1")
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != "run_reconciled" || !strings.Contains(string(last.Payload), `"replay":false`) {
		t.Fatalf("last = %s %s", last.Type, last.Payload)
	}
}

// 计划是整份替换：第二次提交只剩一步，旧步骤必须消失。
func taskPlanWholeReplacement(t *testing.T) {
	session := "eval-plan-" + t.Name()
	agent.ClearPlan(session)
	t.Cleanup(func() { agent.ClearPlan(session) })

	first := agent.Plan{Steps: []agent.PlanStep{
		{Step: "读代码", Status: "completed"},
		{Step: "改实现", Status: "in_progress"},
	}}
	if _, err := agent.UpdatePlan(session, first); err != nil {
		t.Fatal(err)
	}
	second := agent.Plan{Steps: []agent.PlanStep{{Step: "跑测试", Status: "in_progress"}}}
	if _, err := agent.UpdatePlan(session, second); err != nil {
		t.Fatal(err)
	}
	cur, ok := agent.CurrentPlan(session)
	if !ok || len(cur.Steps) != 1 || cur.Steps[0].Step != "跑测试" {
		t.Fatalf("计划应被整份替换：%#v", cur)
	}
	// 两个 in_progress 必须被拒绝，且不落库。
	bad := agent.Plan{Steps: []agent.PlanStep{
		{Step: "a", Status: "in_progress"},
		{Step: "b", Status: "in_progress"},
	}}
	if _, err := agent.UpdatePlan(session, bad); err == nil {
		t.Fatal("两个 in_progress 应被拒绝")
	}
	cur, _ = agent.CurrentPlan(session)
	if len(cur.Steps) != 1 {
		t.Fatalf("非法计划不应覆盖旧计划：%#v", cur)
	}
}

// 可观测：设置 AGENT_TRACE_DIR 后，每个 span 落一行 JSONL。
func taskObsTraceJSONL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_TRACE_DIR", dir)
	end := obs.Start("chat", map[string]any{"gen_ai.request.model": "eval-model"})
	end(false)
	raw := readEvalFile(t, filepath.Join(dir, "trace.jsonl"))
	var span obs.Span
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &span); err != nil {
		t.Fatalf("trace 不是合法 JSONL：%v（%q）", err, raw)
	}
	if span.Name != "chat" || span.Status != "ok" || span.TraceID == "" {
		t.Fatalf("span = %+v", span)
	}
	if span.Attrs["gen_ai.request.model"] != "eval-model" {
		t.Fatalf("attrs = %#v", span.Attrs)
	}
}

// 文件变化触发：改写文件后快照 diff 能指出是哪个文件。
func taskTriggerFileChange(t *testing.T) {
	root := t.TempDir()
	writeEvalFile(t, filepath.Join(root, "notes.md"), "第一版")
	before, err := trigger.Snapshot(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 文件系统时间精度
	writeEvalFile(t, filepath.Join(root, "notes.md"), "第二版，长度不同")
	after, err := trigger.Snapshot(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	changed := trigger.Changed(before, after)
	if len(changed) != 1 || !strings.HasSuffix(filepath.ToSlash(changed[0]), "notes.md") {
		t.Fatalf("changed = %#v", changed)
	}
}

// 生命周期钩子：PreToolUse 失败 → 拒绝该工具；不在 tools 过滤里的工具照常放行。
func taskHooksPreToolUse(t *testing.T) {
	root := t.TempDir()
	fail := []string{"/bin/false"}
	if runtime.GOOS == "windows" {
		fail = []string{"cmd.exe", "/c", "exit", "1"}
	}
	payload, _ := json.Marshal(map[string]any{
		"hooks": []map[string]any{{"phase": "PreToolUse", "command": fail, "tools": []string{"write"}}},
	})
	writeEvalFile(t, filepath.Join(root, ".moon", "hooks.json"), string(payload))
	if d := hooks.PreToolUse(context.Background(), root, "write", "{}"); d.Allow {
		t.Fatal("失败的 PreToolUse 应当拒绝 write")
	}
	if d := hooks.PreToolUse(context.Background(), root, "read", "{}"); !d.Allow {
		t.Fatalf("read 不在过滤里，应放行：%s", d.Reason)
	}
}

// 文件检查点：覆盖写之后能恢复到写之前的内容（整条工具链，不是内部函数）。
func taskCheckpointRestore(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeEvalFile(t, path, "v1")
	ex := &agent.Exec{Root: root, SessionID: "eval", TurnID: "t1"}
	ctx := context.Background()

	if res, err := (agentfs.ReadTool{}).Execute(ctx, json.RawMessage(`{"file_path":"a.txt"}`), ex); err != nil || res.IsError {
		t.Fatalf("read: %v %+v", err, res)
	}
	if res, err := (agentfs.WriteTool{}).Execute(ctx, json.RawMessage(`{"file_path":"a.txt","content":"v2"}`), ex); err != nil || res.IsError {
		t.Fatalf("write: %v %+v", err, res)
	}
	if got := readEvalFile(t, path); got != "v2" {
		t.Fatalf("写入后应为 v2，实际 %q", got)
	}
	if res, err := (agentfs.RestoreTool{}).Execute(ctx, json.RawMessage(`{"file_path":"a.txt"}`), ex); err != nil || res.IsError {
		t.Fatalf("restore: %v %+v", err, res)
	}
	if got := readEvalFile(t, path); got != "v1" {
		t.Fatalf("恢复后应为 v1，实际 %q", got)
	}
}
