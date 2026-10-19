package web

// 子 agent 运行器的单测（契约 docs/agent/06-subagent.md §3.3）。
//
// 全部离线：不连 MySQL，只测纯函数——env 读取、容量判断、通知文案拼装、
// 拒绝壳语义、终态分类与参数归一化。落库路径由 verifier 的端到端验证覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// ---------------------------------------------------------------------------
// 环境变量与容量
// ---------------------------------------------------------------------------

func TestNamedTeammateRunKeepsIdentity(t *testing.T) {
	req := agent.SubagentRequest{Name: " architect ", Model: "mock-model", Context: "fresh"}
	run := newSubagentRun(model.Conversation{UserID: "user"}, 23, 12, 12, 1, "技术方案", "完成目录骨架", req, KindWork)
	if run.Name != "architect" || run.ChildConversationID != 23 || run.ParentConversationID != 12 || run.UserID != "user" {
		t.Fatalf("named teammate lost its persisted identity: %+v", run)
	}
}

func TestSubagentMaxDepthDefaultAndEnvOverride(t *testing.T) {
	t.Setenv("AGENT_MAX_DEPTH", "")
	if got := subagentMaxDepth(); got != subagentDefaultMaxDepth {
		t.Fatalf("未配置时 subagentMaxDepth() = %d，想要默认值 %d", got, subagentDefaultMaxDepth)
	}

	t.Setenv("AGENT_MAX_DEPTH", "3")
	if got := subagentMaxDepth(); got != 3 {
		t.Fatalf("配置 3 时 subagentMaxDepth() = %d，想要 3", got)
	}

	// 非法值与非正数一律回落默认：0 表示「没配」，不能变成「一层都不许派」。
	for _, raw := range []string{"abc", "0", "-2", "  "} {
		t.Setenv("AGENT_MAX_DEPTH", raw)
		if got := subagentMaxDepth(); got != subagentDefaultMaxDepth {
			t.Errorf("配置 %q 时 subagentMaxDepth() = %d，想要默认值 %d", raw, got, subagentDefaultMaxDepth)
		}
	}
}

func TestSubagentAllowOutsideFollowsMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{{ModeAuto, true}, {ModeConfirm, false}, {ModePlan, false}, {"unknown", false}} {
		if got := subagentAllowOutside(tc.mode); got != tc.want {
			t.Errorf("subagentAllowOutside(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestSubagentMaxChildrenDefaultAndEnvOverride(t *testing.T) {
	t.Setenv("AGENT_MAX_CHILDREN", "")
	if got := subagentMaxChildren(); got != subagentDefaultMaxChildren {
		t.Fatalf("未配置时 subagentMaxChildren() = %d，想要默认值 %d", got, subagentDefaultMaxChildren)
	}

	t.Setenv("AGENT_MAX_CHILDREN", "2")
	if got := subagentMaxChildren(); got != 2 {
		t.Fatalf("配置 2 时 subagentMaxChildren() = %d，想要 2", got)
	}

	t.Setenv("AGENT_MAX_CHILDREN", "nope")
	if got := subagentMaxChildren(); got != subagentDefaultMaxChildren {
		t.Fatalf("非法值时 subagentMaxChildren() = %d，想要默认值 %d", got, subagentDefaultMaxChildren)
	}
}

func TestSubagentAtCapacity(t *testing.T) {
	cases := []struct {
		name          string
		running, max  int
		wantAtCapcity bool
	}{
		{"空池未到上限", 0, 4, false},
		{"刚好到上限", 4, 4, true},
		{"超限", 5, 4, true},
		{"max 漏传时按默认上限（未满）", 0, 0, false},
		{"max 漏传时按默认上限（已满）", 4, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subagentAtCapacity(tc.running, tc.max); got != tc.wantAtCapcity {
				t.Fatalf("subagentAtCapacity(%d, %d) = %v，想要 %v", tc.running, tc.max, got, tc.wantAtCapcity)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 通知文案的拼装
// ---------------------------------------------------------------------------

func TestSubagentNoticeContentIsPlainText(t *testing.T) {
	notice := agent.SubagentNotice{
		ChildID:    "12",
		Label:      "整理会议纪要",
		Status:     model.SubagentDone,
		StopReason: subagentStopDone,
		Reply:      "三条结论：A、B、C。",
		DurationMS: 1234,
	}
	got := subagentNoticeContent(notice)
	if strings.TrimSpace(got) == "" {
		t.Fatal("通知文案为空，父级被唤醒后会看不到任何线索")
	}
	for _, want := range []string{"整理会议纪要", "三条结论：A、B、C。"} {
		if !strings.Contains(got, want) {
			t.Errorf("通知文案缺少 %q：\n%s", want, got)
		}
	}
	// 契约 §3.5：结构化字段不塞进 content（前端走 GET .../subagents 拿 run 行）。
	if strings.Contains(got, `"child_id"`) {
		t.Errorf("通知文案里混进了结构化 JSON：\n%s", got)
	}
}

func TestSubagentNoticeContentFailedKeepsError(t *testing.T) {
	notice := agent.SubagentNotice{
		ChildID: "7",
		Label:   "跑回归测试",
		Status:  model.SubagentFailed,
		Error:   "模型调用失败：EOF",
	}
	got := subagentNoticeContent(notice)
	if !strings.Contains(got, "跑回归测试") {
		t.Errorf("失败通知丢了标签：\n%s", got)
	}
	if !strings.Contains(got, "模型调用失败：EOF") {
		t.Errorf("失败通知丢了错误原因：\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// 拒绝壳
// ---------------------------------------------------------------------------

// subagentStubTool 只用来验证拒绝壳不会执行内层工具。
type subagentStubTool struct {
	name     string
	executed *int
}

func (s subagentStubTool) Name() string               { return s.name }
func (s subagentStubTool) Description() string        { return "测试用的假工具" }
func (s subagentStubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (s subagentStubTool) Execute(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error) {
	*s.executed++
	return agent.Result{Content: "内层工具不该被调用"}, nil
}

func TestSubagentDenyToolRejectsWithoutExecutingInner(t *testing.T) {
	executed := 0
	deny := subagentDenyTool{inner: subagentStubTool{name: "write", executed: &executed}}

	// 外壳必须保持内层工具的对外形状，模型看到的还是同一个工具。
	if deny.Name() != "write" {
		t.Fatalf("拒绝壳改了工具名：%s", deny.Name())
	}
	if deny.Description() == "" || deny.Parameters() == nil {
		t.Fatal("拒绝壳丢了 Description/Parameters，模型会看到残缺的工具定义")
	}

	res, err := deny.Execute(context.Background(), json.RawMessage(`{"file_path":"a.md"}`), nil)
	if err != nil {
		t.Fatalf("拒绝是正常语义，不该返回 error：%v", err)
	}
	if !res.IsError {
		t.Fatal("拒绝壳必须返回 IsError=true，否则模型会以为自己成功了")
	}
	if executed != 0 {
		t.Fatalf("内层工具被执行了 %d 次，拒绝壳必须直接拒绝", executed)
	}
	if !strings.Contains(res.Content, "不能执行需要用户确认的操作") ||
		!strings.Contains(res.Content, "主 agent") {
		t.Errorf("拒绝文案不符合契约 §3.3 第 6 条：%s", res.Content)
	}
	if res.Display["code"] != "SUBAGENT_DENIED" {
		t.Errorf("Display.code = %v，想要 SUBAGENT_DENIED", res.Display["code"])
	}
}

// ---------------------------------------------------------------------------
// 终态分类与参数归一化
// ---------------------------------------------------------------------------

func TestClassifySubagentOutcome(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantStop   string
	}{
		{"正常结束", nil, model.SubagentDone, subagentStopDone},
		{"取消", context.Canceled, model.SubagentAborted, subagentStopCanceled},
		{"超时", context.DeadlineExceeded, model.SubagentAborted, subagentStopTimeout},
		{"轮数触顶", agent.ErrMaxIterations, model.SubagentFailed, subagentStopMaxIterations},
		{"工具调用触顶", fmt.Errorf("包了一层：%w", agent.ErrMaxToolCalls), model.SubagentFailed, subagentStopMaxToolCalls},
		{"模型调用失败", errors.New("EOF"), model.SubagentFailed, subagentStopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, stop := classifySubagentOutcome(tc.err)
			if status != tc.wantStatus || stop != tc.wantStop {
				t.Fatalf("classifySubagentOutcome(%v) = (%s, %s)，想要 (%s, %s)",
					tc.err, status, stop, tc.wantStatus, tc.wantStop)
			}
		})
	}
}

func TestNormalizeSubagentContext(t *testing.T) {
	cases := map[string]string{
		"":        "fresh",
		"fresh":   "fresh",
		"fork":    "fork",
		"FORK":    "fork",
		"  Fork ": "fork",
		"weird":   "fresh",
	}
	for raw, want := range cases {
		if got := normalizeSubagentContext(raw); got != want {
			t.Errorf("normalizeSubagentContext(%q) = %q，想要 %q", raw, got, want)
		}
	}
}

func TestParseSubagentConvID(t *testing.T) {
	if id, err := parseSubagentConvID(" 42 "); err != nil || id != 42 {
		t.Fatalf("parseSubagentConvID(\" 42 \") = (%d, %v)，想要 (42, nil)", id, err)
	}
	for _, raw := range []string{"", "0", "-1", "abc"} {
		if _, err := parseSubagentConvID(raw); err == nil {
			t.Errorf("parseSubagentConvID(%q) 应该报错", raw)
		}
	}
}
