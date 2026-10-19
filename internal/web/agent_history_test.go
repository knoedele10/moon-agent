package web

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// mustTraceJSON 把若干条轨迹条目编成落库时那一行的形状（role=tool_trace 的 Content）。
func mustTraceJSON(t *testing.T, entries ...agentTraceEntry) string {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("编轨迹 JSON 失败：%v", err)
	}
	return string(b)
}

// 正常轨迹 → 注记里带工具名、成功/失败与摘要。
func TestToolTraceNoteBasic(t *testing.T) {
	raw := mustTraceJSON(t,
		agentTraceEntry{Name: "read", OK: true, Summary: "读取了 a.go（1-40 行）"},
		agentTraceEntry{Name: "edit", OK: true, Summary: "把 foo 替换成 bar"},
	)

	got := toolTraceNote(raw)
	want := traceNoteHeader +
		"\n- read（成功）：读取了 a.go（1-40 行）" +
		"\n- edit（成功）：把 foo 替换成 bar"
	if got != want {
		t.Fatalf("toolTraceNote = %q, want %q", got, want)
	}
}

// 失败的调用要标成「失败」，否则模型会以为那步做成了。
func TestToolTraceNoteMarksFailure(t *testing.T) {
	raw := mustTraceJSON(t, agentTraceEntry{Name: "edit", OK: false, Summary: "文件不存在"})

	got := toolTraceNote(raw)
	if !strings.Contains(got, "- edit（失败）：文件不存在") {
		t.Fatalf("失败调用应标为「失败」：%q", got)
	}
}

// Detail 可能很长，一律丢弃：注记里不能出现它的任何片段。
func TestToolTraceNoteDropsLongDetail(t *testing.T) {
	huge := strings.Repeat("DETAIL-SHOULD-NEVER-APPEAR ", 500)
	raw := mustTraceJSON(t, agentTraceEntry{
		Name: "read", OK: true, Summary: "读取了 a.go", Detail: huge,
	})

	got := toolTraceNote(raw)
	if strings.Contains(got, "DETAIL") {
		t.Fatalf("Detail 不应出现在注记里：%q", got)
	}
	want := traceNoteHeader + "\n- read（成功）：读取了 a.go"
	if got != want {
		t.Fatalf("toolTraceNote = %q, want %q", got, want)
	}
}

// 超长摘要按 rune 截断：中文不能截出半个字（乱码）。
func TestToolTraceNoteTruncatesLongSummaryByRune(t *testing.T) {
	long := strings.Repeat("测", 200)
	raw := mustTraceJSON(t, agentTraceEntry{Name: "edit", OK: true, Summary: long})

	got := toolTraceNote(raw)
	want := traceNoteHeader + "\n- edit（成功）：" + strings.Repeat("测", traceSummaryLimit)
	if got != want {
		t.Fatalf("超长摘要应按 rune 截到 %d 字：got=%q", traceSummaryLimit, got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断后不是合法 UTF-8：%q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("截断把中文切成了乱码：%q", got)
	}
}

// 只读工具的摘要在同一个上限下截得更短（80 字 vs 120 字）。
func TestToolTraceNoteReadOnlySummaryIsShorter(t *testing.T) {
	long := strings.Repeat("测", 200)

	readOnly := toolTraceNote(mustTraceJSON(t, agentTraceEntry{Name: "read", OK: true, Summary: long}))
	writeOp := toolTraceNote(mustTraceJSON(t, agentTraceEntry{Name: "edit", OK: true, Summary: long}))

	wantRead := traceNoteHeader + "\n- read（成功）：" + strings.Repeat("测", traceReadOnlyLimit)
	wantWrite := traceNoteHeader + "\n- edit（成功）：" + strings.Repeat("测", traceSummaryLimit)
	if readOnly != wantRead {
		t.Fatalf("只读工具摘要 = %q, want %q", readOnly, wantRead)
	}
	if writeOp != wantWrite {
		t.Fatalf("写操作摘要 = %q, want %q", writeOp, wantWrite)
	}
	if len([]rune(readOnly)) >= len([]rune(writeOp)) {
		t.Fatalf("只读注记应短于写操作注记：read=%d write=%d",
			len([]rune(readOnly)), len([]rune(writeOp)))
	}
}

// 单条轨迹超过 6 次调用 → 只列 6 条，并写明还有几次没列。
func TestToolTraceNoteCapsEntries(t *testing.T) {
	entries := make([]agentTraceEntry, 0, 8)
	for i := 0; i < 8; i++ {
		entries = append(entries, agentTraceEntry{Name: "edit", OK: true, Summary: "改动" + string(rune('a'+i))})
	}

	got := toolTraceNote(mustTraceJSON(t, entries...))
	if n := strings.Count(got, "\n- "); n != traceMaxEntries {
		t.Fatalf("最多应列 %d 条，实际 %d：%q", traceMaxEntries, n, got)
	}
	if !strings.Contains(got, "另有 2 次调用未列出") {
		t.Fatalf("应写明还有 2 次调用未列出：%q", got)
	}
}

// 解析失败 / 空数组 / null / 空串 → 返回空串，且不 panic。
func TestToolTraceNoteBadInputReturnsEmpty(t *testing.T) {
	cases := map[string]string{
		"空串":      "",
		"纯空白":     "   \n\t ",
		"非法 JSON": "{not json",
		"对象不是数组":  `{"name":"read","ok":true,"summary":"x"}`,
		"空数组":     "[]",
		"null":    "null",
		"只有名字没摘要": `[{"name":"read"}]`,
	}
	for name, raw := range cases {
		if got := toolTraceNote(raw); got != "" {
			t.Fatalf("%s：应返回空串，实际 %q", name, got)
		}
	}
}

// 只读工具名单：这七个算只读，其余不算。
func TestIsReadOnlyTool(t *testing.T) {
	readOnly := []string{
		"read", "list", "web_search", "web_reader", "memory_search", "skill_search", "skill_read",
	}
	for _, name := range readOnly {
		if !isReadOnlyTool(name) {
			t.Errorf("%q 应算只读工具", name)
		}
	}
	// 大小写与首尾空格不敏感。
	if !isReadOnlyTool("  READ ") {
		t.Errorf("工具名匹配应忽略大小写与空格")
	}

	notReadOnly := []string{"edit", "write", "delete", "move", "sql_query", "office_edit", "memory_write", "", "reads"}
	for _, name := range notReadOnly {
		if isReadOnlyTool(name) {
			t.Errorf("%q 不应算只读工具", name)
		}
	}
}

// 环境变量：未配置 / 解析失败回落默认 6；<=0 原样返回（调用方据此不注入）。
func TestTraceNoteTurnsFromEnv(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultTraceNoteTurns},
		{"   ", defaultTraceNoteTurns},
		{"3", 3},
		{"0", 0},
		{"-2", -2},
		{"abc", defaultTraceNoteTurns},
		{"1.5", defaultTraceNoteTurns},
	}
	for _, c := range cases {
		t.Setenv(envTraceNoteTurns, c.raw)
		if got := traceNoteTurns(); got != c.want {
			t.Errorf("AGENT_TRACE_NOTE_TURNS=%q → %d, want %d", c.raw, got, c.want)
		}
	}
}

// 跨轮注入：只注入最近 N 轮的轨迹，且用 role=system（不冒充 user）。
func TestRebuildLLMHistoryInjectsRecentTraceNotes(t *testing.T) {
	t.Setenv(envTraceNoteTurns, "2")

	history := []model.Message{
		{Id: 1, Role: "user", Content: "第一问"},
		{Id: 2, Role: roleToolTrace, Content: mustTraceJSON(t, agentTraceEntry{Name: "read", OK: true, Summary: "读取了 a.go"})},
		{Id: 3, Role: "assistant", Content: "答一"},
		{Id: 4, Role: "user", Content: "第二问"},
		{Id: 5, Role: roleToolTrace, Content: mustTraceJSON(t, agentTraceEntry{Name: "edit", OK: true, Summary: "改了 b.go"})},
		{Id: 6, Role: "assistant", Content: "答二"},
		{Id: 7, Role: "user", Content: "第三问"},
		{Id: 8, Role: roleToolTrace, Content: mustTraceJSON(t, agentTraceEntry{Name: "write", OK: true, Summary: "写了 c.go"})},
		{Id: 9, Role: "assistant", Content: "答三"},
	}

	got := rebuildLLMHistory(history)

	want := []llm.Message{
		{Role: llm.RoleUser, Content: "第一问"},
		{Role: llm.RoleAssistant, Content: "答一"},
		{Role: llm.RoleUser, Content: "第二问"},
		{Role: llm.RoleSystem, Content: traceNoteHeader + "\n- edit（成功）：改了 b.go"},
		{Role: llm.RoleAssistant, Content: "答二"},
		{Role: llm.RoleUser, Content: "第三问"},
		{Role: llm.RoleSystem, Content: traceNoteHeader + "\n- write（成功）：写了 c.go"},
		{Role: llm.RoleAssistant, Content: "答三"},
	}
	if len(got) != len(want) {
		t.Fatalf("长度 = %d, want %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if !sameMessage(got[i], want[i]) {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 注记绝不能伪装成用户消息。
	for _, m := range got {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, traceNoteHeader) {
			t.Fatalf("轨迹注记不得用 role=user：%+v", m)
		}
	}
}

// 默认 6 轮：第 7 条（最早的那条）不再注入。
func TestRebuildLLMHistoryDefaultTraceTurnsIsSix(t *testing.T) {
	t.Setenv(envTraceNoteTurns, "")

	history := make([]model.Message, 0, 14)
	for i := 1; i <= 7; i++ {
		history = append(history,
			model.Message{Id: uint(2*i - 1), Role: "user", Content: "问" + string(rune('0'+i))},
			model.Message{Id: uint(2 * i), Role: roleToolTrace, Content: mustTraceJSON(t,
				agentTraceEntry{Name: "edit", OK: true, Summary: "改动" + string(rune('0'+i))})},
		)
	}

	got := rebuildLLMHistory(history)
	if n := strings.Count(joinContents(got), traceNoteHeader); n != defaultTraceNoteTurns {
		t.Fatalf("默认应注入 %d 条注记，实际 %d", defaultTraceNoteTurns, n)
	}
	joined := joinContents(got)
	if strings.Contains(joined, "改动1") {
		t.Fatalf("最早的一条轨迹不该注入：%q", joined)
	}
	if !strings.Contains(joined, "改动7") {
		t.Fatalf("最新的一条轨迹应注入：%q", joined)
	}
}

// N<=0 → 一条注记都不注入，等价于改造前的行为。
func TestRebuildLLMHistoryTraceNotesDisabledByEnv(t *testing.T) {
	t.Setenv(envTraceNoteTurns, "0")

	history := []model.Message{
		{Id: 1, Role: "user", Content: "你好"},
		{Id: 2, Role: roleToolTrace, Content: mustTraceJSON(t, agentTraceEntry{Name: "read", OK: true, Summary: "读取了 a.go"})},
		{Id: 3, Role: "assistant", Content: "你好，有什么可以帮你？"},
	}

	got := rebuildLLMHistory(history)
	want := []llm.Message{
		{Role: llm.RoleUser, Content: "你好"},
		{Role: llm.RoleAssistant, Content: "你好，有什么可以帮你？"},
	}
	if len(got) != len(want) {
		t.Fatalf("长度 = %d, want %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if !sameMessage(got[i], want[i]) {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func joinContents(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// sameMessage 只比角色与正文：llm.Message 含切片字段，不可直接用 == 比较。
func sameMessage(a, b llm.Message) bool {
	return a.Role == b.Role && a.Content == b.Content
}
