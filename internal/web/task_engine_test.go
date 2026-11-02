package web

import (
	"context"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// 生产路径回归：Eino 引擎 + 真实 mock 后端 + 未开启渐进披露（装配返回 nil ToolSource）。
// 这条路径曾在容器里 panic（nil ToolSource），单测里因环境不同没有覆盖到。
func TestTaskRealEngineStaticToolSource(t *testing.T) {
	h := newTaskHarness(t, func(h *taskHarness, c taskCall) []llm.Event { return ttext("x") })
	StopTaskScheduler()
	old := registry
	registry = llm.NewDefaultRegistry()
	agent.SetStreamer(agent.RegistryStreamer(registry))
	t.Cleanup(func() { registry = old })
	t.Setenv("AGENT_ENGINE", "eino")
	t.Setenv("AGENT_EVOLVE", "1")
	t.Setenv("AGENT_TOOL_DISCLOSURE", "off")
	t.Setenv("AGENT_TOOL_SPILL_BYTES", "")
	if toolDisclosureMode() == "search" {
		t.Skip("当前环境强制渐进披露")
	}
	task, err := createTask(simpleSpec(), taskCreateOptions{UserID: h.user, MainConversationID: h.main.Id, Model: "mock-echo", Kind: KindWork, Mode: ModeConfirm})
	if err != nil {
		t.Fatal(err)
	}
	runTaskStep(context.Background(), task.Id)
	cur, _ := loadTask(task.Id)
	if cur.RoundsUsed != 1 || cur.ConsecutiveFailures != 0 || cur.StopReason != "auto_continue" {
		t.Fatalf("真实引擎一轮应正常结束并自动续跑：rounds=%d failures=%d stop=%s err=%s", cur.RoundsUsed, cur.ConsecutiveFailures, cur.StopReason, cur.LastError)
	}
}
