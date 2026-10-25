package web

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"":          ModeConfirm,
		"plan":      ModePlan,
		"PLAN":      ModePlan,
		" confirm ": ModeConfirm,
		"auto":      ModeAuto,
		"乱写":        ModeConfirm, // 非法值一律回落到最保守的 confirm
	}
	for in, want := range cases {
		if got := normalizeMode(in); got != want {
			t.Fatalf("normalizeMode(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestRiskyToolClassification(t *testing.T) {
	risky := []string{"write", "edit", "move", "delete", "office_edit", "skill_run", "run_command"}
	for _, n := range risky {
		if !riskyTool(n) {
			t.Errorf("%s 应当算风险工具", n)
		}
	}
	// memory_write 也属于 safe：写记忆库不动用户文件，所有档位都开放。
	safe := []string{"read", "list", "read_image", "calculator", "datetime", "web_search", "web_reader", "ask_user", "json_format", "sql_query", "memory_search", "memory_write", "skill_search", "skill_read"}
	for _, n := range safe {
		if riskyTool(n) {
			t.Errorf("%s 是只读/无副作用工具，不该每次都要确认", n)
		}
	}
}

func TestIsAffirmative(t *testing.T) {
	yes := []string{"确认", "确定", "可以", "继续", "好的", "好", "yes", "OK", "confirm"}
	for _, m := range yes {
		if !isAffirmative(m) {
			t.Errorf("%q 应当算肯定答复", m)
		}
	}
	no := []string{"", "不要", "取消", "先别动", "我想想再说吧这个问题比较复杂我们先讨论一下", "为什么"}
	for _, m := range no {
		if isAffirmative(m) {
			t.Errorf("%q 不该算肯定答复", m)
		}
	}
}

func TestLastAssistantWasConfirmRequest(t *testing.T) {
	confirm := "我要执行一个会改动文件的操作，需要你确认：\n\n工具：delete"
	hist := []llmMessageLike{
		{Role: "user", Content: "删掉 a.txt"},
		{Role: "assistant", Content: confirm},
		{Role: "user", Content: "确认"},
	}
	if !lastAssistantWasConfirmRequest(hist) {
		t.Fatal("上一条 assistant 是确认请求，应当判为 true")
	}

	hist2 := []llmMessageLike{
		{Role: "assistant", Content: "好的，我来处理。"},
		{Role: "user", Content: "确认"},
	}
	if lastAssistantWasConfirmRequest(hist2) {
		t.Fatal("上一条 assistant 是普通回复，不该判为 true")
	}

	if lastAssistantWasConfirmRequest(nil) {
		t.Fatal("空历史不该判为 true")
	}
}

// 确认壳绝不能真的执行内层工具——它的全部职责就是停下来问用户。
type recordingTool struct {
	called bool
}

func (r *recordingTool) Name() string               { return "delete" }
func (r *recordingTool) Description() string        { return "test" }
func (r *recordingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (r *recordingTool) Execute(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error) {
	r.called = true
	return agent.Result{Content: "executed"}, nil
}

func TestConfirmToolStopsInsteadOfExecuting(t *testing.T) {
	inner := &recordingTool{}
	res, err := confirmTool{inner: inner}.Execute(context.Background(), json.RawMessage(`{"file_path":"a.txt"}`), &agent.Exec{})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called {
		t.Fatal("确认壳不该执行内层工具")
	}
	if res.AwaitUser == "" {
		t.Fatal("必须设置 AwaitUser，循环才会停下来问用户")
	}
	if res.IsError {
		t.Fatal("等待确认不是错误")
	}
}

// confirm 模式必须把风险工具包起来，auto 模式必须原样放行，plan 模式必须一个工具都不给。
func TestAgentToolsRespectMode(t *testing.T) {
	root := t.TempDir()

	// plan 模式只留四类：记忆工具 + 只读的技能工具（查说明书，不碰磁盘）
	// + 只能写 .md 的 write（计划存档是默认操作）+ update_plan（计划状态本身）。
	plan := agentToolsFor("u1", root, ModePlan, false, subagentMaxDepth())
	if len(plan) != 6 {
		t.Fatalf("plan 模式应当注册 6 个工具（memory_search/memory_write/skill_search/skill_read/update_plan/write），实际 %d 个", len(plan))
	}
	for _, tl := range plan {
		switch tl.Name() {
		case "memory_search", "memory_write", "skill_search", "skill_read", "update_plan", "write":
		default:
			t.Fatalf("plan 模式不该注册 %s", tl.Name())
		}
	}

	// 没配工作区时连计划存档都写不了，只剩记忆、技能与计划状态。
	if got := agentToolsFor("u1", "", ModePlan, false, subagentMaxDepth()); len(got) != 5 {
		t.Fatalf("无工作区时 plan 模式应当只剩 5 个不碰磁盘的工具（记忆 2 + 技能 2 + update_plan），实际 %d 个", len(got))
	}

	auto := agentToolsFor("u1", root, ModeAuto, false, subagentMaxDepth())
	for _, tl := range auto {
		if _, wrapped := tl.(confirmTool); wrapped {
			t.Fatalf("auto 模式不该有确认壳，但 %s 被包了", tl.Name())
		}
	}

	confirm := agentToolsFor("u1", root, ModeConfirm, false, subagentMaxDepth())
	sawWrapped, sawPlain := false, false
	for _, tl := range confirm {
		if _, wrapped := tl.(confirmTool); wrapped {
			if tl.Name() == "delete" {
				sawWrapped = true
			}
		} else if tl.Name() == "read" {
			sawPlain = true
		}
	}
	if !sawWrapped {
		t.Fatal("confirm 模式下 delete 应当被确认壳包住")
	}
	if !sawPlain {
		t.Fatal("confirm 模式下只读工具不该被包（否则每步都要确认）")
	}

	// 用户确认过之后，同一次运行放行真正的工具。
	afterConfirm := agentToolsFor("u1", root, ModeConfirm, true, subagentMaxDepth())
	for _, tl := range afterConfirm {
		if _, wrapped := tl.(confirmTool); wrapped {
			t.Fatalf("用户已确认时不该再有确认壳：%s", tl.Name())
		}
	}
}

// ---------- 外部路径确认壳（工作区是首选目录，不是硬沙箱） ----------

func outsideTestRaw(t *testing.T, path string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// auto 档直接做（不拦），confirm 档拦下来问用户且绝不执行内层，工作区内路径照旧交给内层。
func TestOutsideWorkspaceToolModes(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	ex := &agent.Exec{Root: root, AllowOutside: true}

	// auto：外部绝对路径直接执行内层。
	autoInner := &recordingTool{}
	raw := outsideTestRaw(t, filepath.Join(out, "a.txt"))
	res, err := (outsideWorkspaceTool{inner: autoInner, root: root, mode: ModeAuto}).Execute(context.Background(), raw, ex)
	if err != nil {
		t.Fatal(err)
	}
	if !autoInner.called {
		t.Fatal("auto 档不该拦工作区外的路径")
	}
	if res.AwaitUser != "" {
		t.Fatalf("auto 档不该产生确认请求: %q", res.AwaitUser)
	}

	// confirm：拦下来，返回 AwaitUser + Display，且不执行内层。
	confirmInner := &recordingTool{}
	res, err = (outsideWorkspaceTool{inner: confirmInner, root: root, mode: ModeConfirm}).Execute(context.Background(), raw, ex)
	if err != nil {
		t.Fatal(err)
	}
	if confirmInner.called {
		t.Fatal("confirm 档外部路径不该执行内层工具")
	}
	if res.AwaitUser == "" {
		t.Fatal("confirm 档外部路径必须返回 AwaitUser")
	}
	if !lastAssistantWasConfirmRequest([]llmMessageLike{{Role: "assistant", Content: res.AwaitUser}}) {
		t.Fatal("外部确认文案必须能被确认流程识别（否则用户回“确认”不会被当成确认）")
	}
	if ok, _ := res.Display["confirm"].(bool); !ok {
		t.Fatalf("Display 应带 confirm=true，got %v", res.Display)
	}
	wantPath := filepath.ToSlash(filepath.Clean(filepath.Join(out, "a.txt")))
	paths, _ := res.Display["paths"].([]string)
	if len(paths) != 1 || paths[0] != wantPath {
		t.Fatalf("Display.paths 应为工作区外路径 %q，got %v", wantPath, res.Display)
	}

	// 工作区内：不拦，交给内层（confirm 档随后由风险确认壳处理）。
	insideInner := &recordingTool{}
	insideRaw := outsideTestRaw(t, filepath.Join(root, "a.txt"))
	if _, err := (outsideWorkspaceTool{inner: insideInner, root: root, mode: ModeConfirm}).Execute(context.Background(), insideRaw, ex); err != nil {
		t.Fatal(err)
	}
	if !insideInner.called {
		t.Fatal("工作区内路径不该被外部确认壳拦住")
	}
}

// move 的两个路径参数都要检查；有一个在工作区外就要问。
func TestOutsideWorkspaceToolCollectsAllPathArgs(t *testing.T) {
	root := t.TempDir()
	outA, outB := t.TempDir(), t.TempDir()
	raw, err := json.Marshal(map[string]any{
		"from_path": filepath.Join(outA, "a.txt"),
		"to_path":   filepath.Join(outB, "b.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingTool{}
	res, err := (outsideWorkspaceTool{inner: inner, root: root, mode: ModeConfirm}).
		Execute(context.Background(), raw, &agent.Exec{Root: root, AllowOutside: true})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called {
		t.Fatal("confirm 档不该执行内层工具")
	}
	paths, _ := res.Display["paths"].([]string)
	if len(paths) != 2 {
		t.Fatalf("from_path/to_path 都该被收集，got %v", res.Display)
	}
}

// confirm 模式下写类工具只弹一次窗：外层是风险确认壳，外部调用让给内层的外部路径壳。
func TestConfirmModeExternalWriteAsksOnce(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	ex := &agent.Exec{Root: root, AllowOutside: true}

	var del agent.Tool
	for _, tl := range agentToolsFor("u1", root, ModeConfirm, false, subagentMaxDepth()) {
		if tl.Name() == "delete" {
			del = tl
		}
	}
	if del == nil {
		t.Fatal("confirm 模式应当注册 delete")
	}
	ct, ok := del.(confirmTool)
	if !ok {
		t.Fatalf("confirm 模式下写类工具外层应是风险确认壳，got %T", del)
	}
	if _, ok := ct.inner.(outsideWorkspaceTool); !ok {
		t.Fatalf("风险确认壳的内层应是外部路径确认壳，got %T", ct.inner)
	}

	// 外部路径：提示里带具体路径，且不是普通风险确认文案。
	res, err := del.Execute(context.Background(), outsideTestRaw(t, filepath.Join(out, "x.txt")), ex)
	if err != nil {
		t.Fatal(err)
	}
	if res.AwaitUser == "" {
		t.Fatal("外部路径必须等用户确认")
	}
	if !strings.Contains(res.AwaitUser, filepath.ToSlash(out)) {
		t.Fatalf("确认文案应带具体外部路径，got %q", res.AwaitUser)
	}
	if !strings.Contains(res.AwaitUser, "工作区之外") {
		t.Fatalf("外部确认文案应说明是工作区外，got %q", res.AwaitUser)
	}

	// 工作区内：仍是原来的风险确认文案（不带外部路径）。
	res, err = del.Execute(context.Background(), outsideTestRaw(t, filepath.Join(root, "x.txt")), ex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.AwaitUser, confirmRequestPrefix) {
		t.Fatalf("工作区内应走普通风险确认，got %q", res.AwaitUser)
	}
	if strings.Contains(res.AwaitUser, "工作区之外") {
		t.Fatalf("工作区内的风险确认不该提外部路径，got %q", res.AwaitUser)
	}
}
