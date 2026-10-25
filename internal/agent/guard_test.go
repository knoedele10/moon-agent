package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"blog-my/internal/llm"
)

// 护栏层的纯逻辑单测。这几条错了的后果都很隐蔽：
// 截断算错会丢数据、切点算错会让整条请求被上游拒掉、token 估算离谱会让压缩时机完全失效。

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯中文按字算", "你好世界", 4},
		{"纯英文按四字符算", "abcdefgh", 2},
		{"中英混合", "你好abcd", 3},
	}
	for _, c := range cases {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("%s: estimateTokens(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

func TestEstimateMessagesTokensCountsToolCalls(t *testing.T) {
	plain := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}
	withCall := []llm.Message{{
		Role:    llm.RoleAssistant,
		Content: "你好",
		ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "read", Arguments: `{"file_path":"a.go"}`},
		},
	}}
	if estimateMessagesTokens(withCall) <= estimateMessagesTokens(plain) {
		t.Fatal("带 tool_calls 的消息应当估得更多")
	}
}

func TestToolOutputCapFollowsContextWindow(t *testing.T) {
	t.Setenv("AGENT_TOOL_OUTPUT_CAP", "")
	t.Setenv("AGENT_CONTEXT_TOKENS", "")
	if got := toolOutputCap(); got != toolOutputCapDefault {
		t.Fatalf("默认上限变了：%d", got)
	}
	t.Setenv("AGENT_CONTEXT_TOKENS", "200000")
	if got := toolOutputCap(); got != 20000 {
		t.Fatalf("应按窗口 10%% 取上限，实际 %d", got)
	}
	t.Setenv("AGENT_TOOL_OUTPUT_CAP", "900")
	if got := toolOutputCap(); got != 900 {
		t.Fatalf("显式上限应优先，实际 %d", got)
	}
}

func TestCapToolOutput(t *testing.T) {
	// 没超上限：原样返回，一个字都不能动。
	short := "hello 世界"
	if got := capToolOutput("read", short); got != short {
		t.Fatalf("未超上限时不该改动：%q", got)
	}

	// 超上限：截断 + 明确告知。
	long := strings.Repeat("中", 100)
	t.Setenv("AGENT_TOOL_OUTPUT_CAP", "10")
	got := capToolOutput("read", long)
	if !strings.Contains(got, "已截断") {
		t.Fatalf("截断后必须带提示，实际 %q", got)
	}
	if !strings.Contains(got, "read") {
		t.Fatal("提示里应当带工具名")
	}
	// 中文不能被截出乱码：截断部分应当仍是合法的 UTF-8。
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("截断产生了乱码：%q", got)
	}
}

// 核心断言：切点**永远不能**落在 role=tool 上，也不能落在"带 tool_calls 的 assistant 之后紧邻 tool"的中间。
func TestSafeCutIndexNeverSplitsToolPairs(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "用户1"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: roleTool, Content: "结果1", ToolCallID: "c1"},
		{Role: llm.RoleAssistant, Content: "助手1"},
		{Role: llm.RoleUser, Content: "用户2"},
		{Role: llm.RoleAssistant, Content: "助手2"},
	}
	for keep := 1; keep <= len(msgs); keep++ {
		cut := safeCutIndex(msgs, keep)
		if cut == 0 {
			continue
		}
		if cut >= len(msgs) {
			t.Fatalf("keep=%d 时切点越界：%d", keep, cut)
		}
		if msgs[cut].Role == roleTool {
			t.Fatalf("keep=%d 时切点落在 tool 消息上（配对会被切断）：%d", keep, cut)
		}
		prev := msgs[cut-1]
		if prev.Role == llm.RoleAssistant && len(prev.ToolCalls) > 0 && msgs[cut].Role == roleTool {
			t.Fatalf("keep=%d 时把 assistant.tool_calls 和它的结果切到了两边", keep)
		}
	}
}

func TestSafeCutIndexBasic(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "u1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleUser, Content: "u2"},
		{Role: llm.RoleAssistant, Content: "a2"},
	}
	// 保留尾部 2 条 → 从 index 3（u2）开始，是安全边界。
	if got := safeCutIndex(msgs, 2); got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
	// keepTail<=0 或消息不够 → 不压缩。
	if got := safeCutIndex(msgs, 0); got != 0 {
		t.Fatalf("keepTail=0 应当返回 0，实际 %d", got)
	}
	if got := safeCutIndex(msgs, len(msgs)); got != 0 {
		t.Fatalf("消息数不超过 keepTail 时应当返回 0，实际 %d", got)
	}
}

func TestSafeCutIndexAllToolMessages(t *testing.T) {
	// 全是 tool 消息：找不到安全切点 → 返回 0（调用方放弃压缩）。
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: roleTool, Content: "r1"},
		{Role: roleTool, Content: "r2"},
		{Role: roleTool, Content: "r3"},
	}
	if got := safeCutIndex(msgs, 1); got != 0 {
		t.Fatalf("全是 tool 消息时应当返回 0，实际 %d", got)
	}
}

func TestRunTimeoutEnv(t *testing.T) {
	t.Setenv("AGENT_RUN_TIMEOUT_SEC", "")
	if got := runTimeout(); got != 0 {
		t.Fatalf("未配置时应当不限时，实际 %v", got)
	}
	t.Setenv("AGENT_RUN_TIMEOUT_SEC", "0")
	if got := runTimeout(); got != 0 {
		t.Fatalf("0 应当不限时，实际 %v", got)
	}
	t.Setenv("AGENT_RUN_TIMEOUT_SEC", "abc")
	if got := runTimeout(); got != 0 {
		t.Fatalf("非法值应当不限时，实际 %v", got)
	}
	t.Setenv("AGENT_RUN_TIMEOUT_SEC", "30")
	if got := runTimeout().Seconds(); got != 30 {
		t.Fatalf("want 30s, got %v", got)
	}
}

// 工作集必须从 tool_call 参数直接提取：模型摘要可能丢路径，这里不能丢。
func TestCollectWorkingSet(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "改一下登录页"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "read", Arguments: `{"file_path":"internal/web/agent.go"}`},
			{ID: "2", Name: "grep", Arguments: `{"pattern":"login","path":"internal/"}`},
		}},
		{Role: roleTool, Content: "结果", ToolCallID: "1"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "3", Name: "edit", Arguments: `{"file_path":"internal\\web\\agent.go","old_string":"a","new_string":"b"}`},
			{ID: "4", Name: "run_command", Arguments: `{"cwd":".","commands":[{"program":"npm","args":["run","build"]}]}`},
		}},
		{Role: roleTool, Content: "ok", ToolCallID: "3"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "5", Name: "read", Arguments: `{"file_path":"."}`},
			{ID: "6", Name: "read", Arguments: `not json`},
		}},
	}
	ws := collectWorkingSet(msgs)
	// 反斜杠路径要归一成 /，重复路径只留一次，"." 不算工作集。
	wantFiles := []string{"internal/web/agent.go", "internal/"}
	if len(ws.Files) != len(wantFiles) {
		t.Fatalf("files = %v, want %v", ws.Files, wantFiles)
	}
	for i, want := range wantFiles {
		if ws.Files[i] != want {
			t.Errorf("files[%d] = %q, want %q", i, ws.Files[i], want)
		}
	}
	if len(ws.Commands) != 1 || ws.Commands[0] != "npm run build" {
		t.Fatalf("commands = %v, want [npm run build]", ws.Commands)
	}
	if block := ws.block(); !strings.Contains(block, "不要凭记忆") {
		t.Fatalf("工作集块要提醒重新 read：%q", block)
	}
}

func TestCollectWorkingSetCapsEntries(t *testing.T) {
	var calls []llm.ToolCall
	for i := 0; i < workingSetMaxFiles+5; i++ {
		calls = append(calls, llm.ToolCall{ID: "x", Name: "read", Arguments: fmt.Sprintf(`{"file_path":"f%d.go"}`, i)})
	}
	ws := collectWorkingSet([]llm.Message{{Role: llm.RoleAssistant, ToolCalls: calls}})
	if len(ws.Files) != workingSetMaxFiles {
		t.Fatalf("工作集应封顶 %d，实际 %d", workingSetMaxFiles, len(ws.Files))
	}
}

// 压缩后必须同时保留：模型摘要 + 确定性工作集 + 「重新 read」的提醒。
func TestMidLoopCompactKeepsWorkingSet(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "把登录页修好"},
		{Role: llm.RoleAssistant, Content: "先看看代码", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read", Arguments: `{"file_path":"login.go"}`},
		}},
		{Role: roleTool, Content: "package login", ToolCallID: "c1"},
		{Role: llm.RoleAssistant, Content: "再改一处", ToolCalls: []llm.ToolCall{
			{ID: "c2", Name: "edit", Arguments: `{"file_path":"login.go","old_string":"x","new_string":"y"}`},
		}},
		{Role: roleTool, Content: "Edited", ToolCallID: "c2"},
		{Role: llm.RoleUser, Content: "继续"},
		{Role: llm.RoleAssistant, Content: "好"},
	}
	stream := func(ctx context.Context, model, key string, req llm.ChatRequest) (<-chan llm.Event, error) {
		ch := make(chan llm.Event, 2)
		ch <- llm.Event{Type: llm.EventDelta, Content: "用户想修登录页；已读并改过 login.go。"}
		ch <- llm.Event{Type: llm.EventDone}
		close(ch)
		return ch, nil
	}
	got := midLoopCompact(context.Background(), stream, "m", "k", msgs, 2)
	if len(got) >= len(msgs) {
		t.Fatalf("应当压缩掉中间消息：%d -> %d", len(msgs), len(got))
	}
	joined := ""
	for _, m := range got {
		joined += m.Content
	}
	for _, want := range []string{"用户想修登录页", "login.go", "压缩时保留的工作集", "重新 read"} {
		if !strings.Contains(joined, want) {
			t.Errorf("压缩结果缺少 %q：\n%s", want, joined)
		}
	}
}

func TestMidLoopCompactTokensEnv(t *testing.T) {
	t.Setenv("AGENT_MIDLOOP_COMPACT_TOKENS", "")
	t.Setenv("AGENT_CONTEXT_TOKENS", "")
	if got := midLoopCompactTokens(); got != midLoopCompactTokensDefault {
		t.Fatalf("默认阈值变了：%d", got)
	}
	t.Setenv("AGENT_CONTEXT_TOKENS", "100000")
	if got := midLoopCompactTokens(); got != 60000 {
		t.Fatalf("应按窗口 60%% 触发，实际 %d", got)
	}
	t.Setenv("AGENT_MIDLOOP_COMPACT_TOKENS", "1234")
	if got := midLoopCompactTokens(); got != 1234 {
		t.Fatalf("显式阈值应优先，实际 %d", got)
	}
}

func TestMidLoopCompactReturnsUnchangedWhenNoSafeCut(t *testing.T) {
	// 全是 tool 消息时找不到安全切点 → 必须**原样返回**，绝不能把消息弄丢。
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: roleTool, Content: "r1"},
		{Role: roleTool, Content: "r2"},
		{Role: roleTool, Content: "r3"},
	}
	got := midLoopCompact(nil, nil, "m", "k", msgs, 1)
	if len(got) != len(msgs) {
		t.Fatalf("找不到安全切点时应原样返回：want %d, got %d", len(msgs), len(got))
	}
}
