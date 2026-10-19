package web

import (
	"context"
	"testing"

	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 上下文压缩的纯逻辑单测。真正的压缩要调模型 + 写库，属于端到端验证
// （见 scripts/e2e-soul.cjs 的思路）；这里钉住的是「不该压缩时一定不压缩」
// 和「决策函数在各种边界下的行为」，因为这些错了会静默丢上下文。

func TestToLLMMessagesRoleMapping(t *testing.T) {
	rows := []model.Message{
		{Id: 1, Role: "user", Content: "你好"},
		{Id: 2, Role: "assistant", Content: "在的"},
		{Id: 3, Role: "system", Content: "系统提示"},
		{Id: 4, Role: "weird", Content: "未知角色按 user 处理"},
	}
	got := toLLMMessages(rows)
	if len(got) != 4 {
		t.Fatalf("条数不对：want 4, got %d", len(got))
	}
	want := []string{llm.RoleUser, llm.RoleAssistant, llm.RoleSystem, llm.RoleUser}
	for i, m := range got {
		if m.Role != want[i] {
			t.Errorf("第 %d 条角色：want %s, got %s", i, want[i], m.Role)
		}
		if m.Content != rows[i].Content {
			t.Errorf("第 %d 条内容被改动：want %q, got %q", i, rows[i].Content, m.Content)
		}
	}
}

func TestSummarizeIsStale(t *testing.T) {
	rows := []model.Message{{Id: 1}, {Id: 2}, {Id: 3}, {Id: 4}, {Id: 5}}

	cases := []struct {
		name        string
		summaryUpto uint
		minNew      int
		hasSummary  bool
		want        bool
	}{
		{"没有摘要时必须生成", 0, 3, false, true},
		// rows 的 id 是 1..5；summaryUpto=k 时，新消息数 = 5-k。
		{"有摘要、只新增 1 条 -> 不重算", 4, 3, true, false},
		{"有摘要、新增 2 条 -> 不重算", 3, 3, true, false},
		{"有摘要、刚好新增 3 条（达到阈值）-> 重算", 2, 3, true, true},
		{"有摘要、新增 4 条 -> 重算", 1, 3, true, true},
		{"minNew 为 0 视为总是要重算", 5, 0, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeIsStale(rows, c.summaryUpto, c.minNew, c.hasSummary)
			if got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestCompactEnvToggles(t *testing.T) {
	// 默认开启：不配 AGENT_COMPACT 时要压缩。
	t.Setenv("AGENT_COMPACT", "")
	if !compactEnabled() {
		t.Fatal("未配置时应当默认开启压缩")
	}
	// 显式关闭的几种写法都要认。
	for _, off := range []string{"0", "false", "off", "no", "FALSE", " Off "} {
		t.Setenv("AGENT_COMPACT", off)
		if compactEnabled() {
			t.Errorf("AGENT_COMPACT=%q 时应当关闭压缩", off)
		}
	}
	for _, on := range []string{"1", "true", "on", "yes"} {
		t.Setenv("AGENT_COMPACT", on)
		if !compactEnabled() {
			t.Errorf("AGENT_COMPACT=%q 时应当开启压缩", on)
		}
	}
}

func TestCompactIntEnvFallsBack(t *testing.T) {
	t.Setenv("AGENT_COMPACT_KEEP", "")
	if got := compactIntEnv("AGENT_COMPACT_KEEP", 10); got != 10 {
		t.Errorf("空值应回落默认：want 10, got %d", got)
	}
	t.Setenv("AGENT_COMPACT_KEEP", "0")
	if got := compactIntEnv("AGENT_COMPACT_KEEP", 10); got != 10 {
		t.Errorf("0 应回落默认：want 10, got %d", got)
	}
	t.Setenv("AGENT_COMPACT_KEEP", "abc")
	if got := compactIntEnv("AGENT_COMPACT_KEEP", 10); got != 10 {
		t.Errorf("非法值应回落默认：want 10, got %d", got)
	}
	t.Setenv("AGENT_COMPACT_KEEP", "8")
	if got := compactIntEnv("AGENT_COMPACT_KEEP", 10); got != 8 {
		t.Errorf("合法值应生效：want 8, got %d", got)
	}
}

// 没有数据库（脱离 DB 单测 / 未接库的部署）时，压缩必须原样放行，
// 绝不能因为查不到消息就把历史裁掉。
func TestMaybeCompactHistoryNoDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	defer func() { model.DB = old }()

	history := []llm.Message{
		{Role: llm.RoleUser, Content: "第一句"},
		{Role: llm.RoleAssistant, Content: "第一答"},
	}
	conv := &model.Conversation{Id: 42}
	got := maybeCompactHistory(context.Background(), conv, history, "m", "k")
	if len(got.History) != len(history) {
		t.Fatalf("无数据库时历史被改动了：want %d, got %d", len(history), len(got.History))
	}
	if got.Summary != "" || got.Compacted {
		t.Fatalf("无数据库时不应产生摘要：summary=%q compacted=%v", got.Summary, got.Compacted)
	}
}

// 关掉开关时，即使历史很长也必须原样放行。
func TestMaybeCompactHistoryDisabled(t *testing.T) {
	t.Setenv("AGENT_COMPACT", "0")
	old := model.DB
	model.DB = nil
	defer func() { model.DB = old }()

	history := make([]llm.Message, 0, 50)
	for i := 0; i < 50; i++ {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: "x"})
	}
	got := maybeCompactHistory(context.Background(), &model.Conversation{Id: 1}, history, "m", "k")
	if len(got.History) != 50 || got.Compacted {
		t.Fatalf("关闭开关时不应压缩：len=%d compacted=%v", len(got.History), got.Compacted)
	}
}
