package agent

import (
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
