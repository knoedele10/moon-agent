package web

import (
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// 待确认调用的判定逻辑。这些判错了会直接表现为「点了确认什么都没发生」
// 或者「说一句不要却把删除跑了」，所以必须钉住。

func TestIsRejection(t *testing.T) {
	yes := []string{"取消", "不要", "不用", "算了", "停止", "别", "否", "no", "cancel", "NO", "Cancel"}
	for _, s := range yes {
		if !isRejection(s) {
			t.Errorf("%q 应当被判为拒绝", s)
		}
	}
	no := []string{"", "确认", "好的", "继续", "我想想再说", "这个方案不错但是我还是想改一下细节再决定"}
	for _, s := range no {
		if isRejection(s) {
			t.Errorf("%q 不该被判为拒绝", s)
		}
	}
}

// 拒绝词与肯定词不能重叠，否则「确认」会被当成拒绝，风险操作永远跑不了。
func TestAffirmativeAndRejectionDoNotOverlap(t *testing.T) {
	for _, s := range []string{"确认", "确定", "同意", "可以", "继续", "好的", "yes", "ok", "go"} {
		if isRejection(s) {
			t.Errorf("%q 同时被判为肯定与拒绝", s)
		}
	}
}

func TestSummarizeToolResult(t *testing.T) {
	got := summarizeToolResult("write", agent.Result{Content: "已写入 hello.txt\n第二行不该出现"})
	if got != "已写入 hello.txt" {
		t.Errorf("应取第一行，实际 %q", got)
	}
	if got := summarizeToolResult("delete", agent.Result{Content: "  "}); got == "" {
		t.Error("空结果也要给一句话，不能留白")
	}
	if got := summarizeToolResult("delete", agent.Result{IsError: true, Content: ""}); got == "" {
		t.Error("失败且无输出时也要给一句话")
	}
}

// 没有数据库时存取都要安静地返回空，不能 panic。
func TestPendingToolNoDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	defer func() { model.DB = old }()

	savePendingTool(1, "u1", nil) // 传 nil 也不该炸
	if got := takePendingTool(1); got != nil {
		t.Fatalf("无数据库时应返回 nil，实际 %+v", got)
	}
	clearPendingTool(1)
}

// runPendingTool 在工具已不存在时必须返回 nil（调用方据此保持原样继续），
// 而不是塞一条假的工具结果进历史。
func TestRunPendingToolUnknownTool(t *testing.T) {
	msgs, entry := runPendingTool(nil, "u1", t.TempDir(), ModeAuto, &model.AgentPendingTool{
		ConversationID: 1, ToolID: "c1", ToolName: "not-a-real-tool",
	}, func(string, any) {})
	if msgs != nil || entry != nil {
		t.Fatalf("未知工具应返回 nil，实际 msgs=%v entry=%v", msgs, entry)
	}
}
