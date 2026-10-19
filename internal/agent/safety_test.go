package agent

import (
	"strings"
	"testing"
)

// 安全层的单测。这些判错了的后果很重：
// 把非幂等当可重试 → 重复删文件；把 unknown 当 transient → 白重试 3 次；
// 指纹算错 → 拦不住重复副作用。

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		in   string
		want ErrorCategory
	}{
		{"文件不存在", CatNotFound},
		{"no such file or directory", CatNotFound},
		{"permission denied", CatPermission},
		{"工具执行失败：invalid arguments: content 必填", CatInvalidArgs},
		{"请求超时 timeout", CatTimeout},
		{"429 Too Many Requests", CatRateLimited},
		{"connection reset by peer", CatTransient},
		{"503 Service Unavailable", CatTransient},
		// 判不出来 → unknown，这是刻意的保守默认。
		{"TypeError: Cannot read properties of undefined", CatUnknown},
		{"某种我们没见过的新错误", CatUnknown},
	}
	for _, c := range cases {
		if got := classifyToolError(c.in); got != c.want {
			t.Errorf("classifyToolError(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestToolSideEffectFailSafeDefault(t *testing.T) {
	// 登记过的工具按登记走。
	if got := toolSideEffect("read"); got != SideEffectReadOnly {
		t.Errorf("read 应当是只读，实际 %s", got)
	}
	if got := toolSideEffect("write"); got != SideEffectIdempotent {
		t.Errorf("write 应当是幂等修改，实际 %s", got)
	}
	if got := toolSideEffect("delete"); got != SideEffectNonIdempotent {
		t.Errorf("delete 应当是非幂等，实际 %s", got)
	}
	// **关键**：没登记过的工具必须落到 non_idempotent（失败安全默认），
	// 否则将来新加一个 shell / python 工具会被自动重放。
	for _, name := range []string{"shell", "python_repl", "send_email", "随便什么新工具"} {
		if got := toolSideEffect(name); got != SideEffectNonIdempotent {
			t.Errorf("未登记的工具 %q 必须按非幂等处理，实际 %s", name, got)
		}
	}
}

func TestDecideRetryMatrix(t *testing.T) {
	cases := []struct {
		cat  ErrorCategory
		se   SideEffectKind
		want bool
	}{
		// 只读：临时性可重试，unknown 也允许一次，其余不重试。
		{CatTransient, SideEffectReadOnly, true},
		{CatTimeout, SideEffectReadOnly, true},
		{CatRateLimited, SideEffectReadOnly, true},
		{CatUnknown, SideEffectReadOnly, true},
		{CatNotFound, SideEffectReadOnly, false},
		{CatPermission, SideEffectReadOnly, false},
		{CatInvalidArgs, SideEffectReadOnly, false},
		// 幂等修改：只有临时性可重试。
		{CatTransient, SideEffectIdempotent, true},
		{CatUnknown, SideEffectIdempotent, false},
		{CatNotFound, SideEffectIdempotent, false},
		// **非幂等：任何类别都不重试**——这是最重要的一条。
		{CatTransient, SideEffectNonIdempotent, false},
		{CatTimeout, SideEffectNonIdempotent, false},
		{CatRateLimited, SideEffectNonIdempotent, false},
		{CatUnknown, SideEffectNonIdempotent, false},
	}
	for _, c := range cases {
		if got := decideRetry(c.cat, c.se); got != c.want {
			t.Errorf("decideRetry(%s, %s) = %v, want %v", c.cat, c.se, got, c.want)
		}
	}
}

func TestIsUnknownOutcome(t *testing.T) {
	// 只有"非幂等 + 超时/临时性"才算结果未知。
	if !isUnknownOutcome(CatTimeout, SideEffectNonIdempotent) {
		t.Fatal("非幂等 + 超时应当判为结果未知")
	}
	if !isUnknownOutcome(CatTransient, SideEffectNonIdempotent) {
		t.Fatal("非幂等 + 临时性错误应当判为结果未知")
	}
	// 只读/幂等工具即使超时也无所谓——重跑一次没有副作用。
	if isUnknownOutcome(CatTimeout, SideEffectReadOnly) {
		t.Fatal("只读工具超时不该判为结果未知")
	}
	if isUnknownOutcome(CatTimeout, SideEffectIdempotent) {
		t.Fatal("幂等修改超时不该判为结果未知")
	}
	// 明确的失败（参数错、找不到）不算"未知"。
	if isUnknownOutcome(CatInvalidArgs, SideEffectNonIdempotent) {
		t.Fatal("参数错误是明确失败，不是结果未知")
	}
}

func TestToolFingerprint(t *testing.T) {
	a := toolFingerprint("delete", `{"path":"a.txt"}`)
	b := toolFingerprint("delete", ` {"path":"a.txt"} `) // 前后空白不影响
	if a != b {
		t.Fatal("同一调用的指纹应当相同（忽略首尾空白）")
	}
	if a == toolFingerprint("delete", `{"path":"b.txt"}`) {
		t.Fatal("不同参数的指纹应当不同")
	}
	if a == toolFingerprint("move", `{"path":"a.txt"}`) {
		t.Fatal("不同工具的指纹应当不同")
	}
}

func TestFindUncertain(t *testing.T) {
	list := []uncertainEffect{{Fingerprint: "abc", ToolName: "delete"}}
	if findUncertain(list, "abc") == nil {
		t.Fatal("应当找到同指纹的记录")
	}
	if findUncertain(list, "zzz") != nil {
		t.Fatal("不同指纹不该命中")
	}
	if findUncertain(nil, "abc") != nil {
		t.Fatal("空列表应当返回 nil")
	}
}

func TestUncertainFeedback(t *testing.T) {
	if got := uncertainFeedback(nil); got != "" {
		t.Fatalf("没有未确认副作用时应当返回空串，实际 %q", got)
	}
	got := uncertainFeedback([]uncertainEffect{{ToolName: "delete", Message: "超时"}})
	for _, want := range []string{"delete", "超时", "不能把它们当作已经完成", "不要再重复执行"} {
		if !strings.Contains(got, want) {
			t.Errorf("反馈里应当包含 %q，实际 %q", want, got)
		}
	}
}
