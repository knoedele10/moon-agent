package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// ---------- 纯逻辑：校验 ----------

func TestNormalizeAssistantNameTrimsAndTruncates(t *testing.T) {
	got, err := normalizeAssistantName("  小助手  ")
	if err != nil {
		t.Fatalf("正常名字不该报错: %v", err)
	}
	if got != "小助手" {
		t.Fatalf("应当去掉首尾空格: %q", got)
	}

	// 按 rune 截到 64：70 个汉字要留前 64 个，且不能把字劈成半个。
	long := strings.Repeat("字", 70)
	got, err = normalizeAssistantName(long)
	if err != nil {
		t.Fatalf("过长名字应当截断而不是报错: %v", err)
	}
	if len([]rune(got)) != assistantNameMaxRunes {
		t.Fatalf("应当截到 %d 个 rune: got=%d", assistantNameMaxRunes, len([]rune(got)))
	}
	if got != strings.Repeat("字", assistantNameMaxRunes) {
		t.Fatal("截断结果不对")
	}

	if _, err := normalizeAssistantName("   "); err == nil {
		t.Fatal("空名字应当被拒")
	}
}

func TestNormalizeAssistantMode(t *testing.T) {
	// 空 = 继承全局，是合法值，返回空串。
	for _, in := range []string{"", "   "} {
		got, err := normalizeAssistantMode(in)
		if err != nil || got != "" {
			t.Fatalf("%q 应表示继承全局: got=%q err=%v", in, got, err)
		}
	}
	cases := map[string]string{
		"plan":      ModePlan,
		"PLAN":      ModePlan,
		" confirm ": ModeConfirm,
		"auto":      ModeAuto,
	}
	for in, want := range cases {
		got, err := normalizeAssistantMode(in)
		if err != nil || got != want {
			t.Fatalf("%q 应归一化为 %q: got=%q err=%v", in, want, got, err)
		}
	}
	for _, bad := range []string{"yolo", "planning", "1", "自主"} {
		if _, err := normalizeAssistantMode(bad); err == nil {
			t.Fatalf("%q 是非法档位，应当被拒", bad)
		}
	}
}

func TestAssistantWorkspaceOverrideKeepsOtherActiveWorkspaces(t *testing.T) {
	got := assistantWorkspaceOverride(`E:\assistant-workspace`, assistantContext{
		Root:  `E:\user-workspace`,
		Roots: []string{`E:\second-workspace`, `E:\user-workspace`},
	})
	if got.Root != `E:\assistant-workspace` {
		t.Fatalf("assistant workspace should be primary: %q", got.Root)
	}
	want := []string{`E:\user-workspace`, `E:\second-workspace`}
	if len(got.Roots) != len(want) {
		t.Fatalf("active workspaces were dropped or duplicated: %#v", got.Roots)
	}
	for i := range want {
		if got.Roots[i] != want[i] {
			t.Fatalf("workspace %d = %q, want %q", i+2, got.Roots[i], want[i])
		}
	}
}

// ---------- 纯逻辑：三态更新 ----------

func TestApplyAssistantPatchTriState(t *testing.T) {
	base := model.AgentAssistant{
		Id: 7, UserID: "u1", Name: "旧名字", Description: "旧简介", Model: "m1",
		SoulVoice: "旧说话", SoulWork: "旧做事", Mode: ModeConfirm,
		FSRoot: `D:\old`, Enabled: true,
	}

	// 一个字段都不传 → 整条记录一字不改。
	a := base
	if err := applyAssistantPatch(&a, assistantPatch{}); err != nil {
		t.Fatalf("空补丁不该报错: %v", err)
	}
	if a != base {
		t.Fatalf("空补丁不该改动记录: got=%+v want=%+v", a, base)
	}

	// 只传 description：其它字段保持原样。
	desc := "新简介"
	a = base
	if err := applyAssistantPatch(&a, assistantPatch{Description: &desc}); err != nil {
		t.Fatalf("只改简介不该报错: %v", err)
	}
	if a.Description != desc {
		t.Fatalf("简介应当更新: %q", a.Description)
	}
	if a.Name != base.Name || a.Model != base.Model || a.Mode != base.Mode || a.FSRoot != base.FSRoot {
		t.Fatalf("未传的字段不该被改动: %+v", a)
	}

	// 传空串 = 清掉覆盖、回到继承全局（对模式/灵魂/工作区都成立）。
	empty := ""
	mode := ""
	enabled := false
	a = base
	if err := applyAssistantPatch(&a, assistantPatch{SoulVoice: &empty, SoulWork: &empty, Mode: &mode, FSRoot: &empty, Enabled: &enabled}); err != nil {
		t.Fatalf("清空覆盖不该报错: %v", err)
	}
	if a.SoulVoice != "" || a.SoulWork != "" || a.Mode != "" || a.FSRoot != "" {
		t.Fatalf("空串应当清掉覆盖: %+v", a)
	}
	if a.Enabled {
		t.Fatal("enabled=false 应当生效")
	}
}

// 校验失败时整条记录必须原地不动：不能出现「名字被清空但接口报 400」这种半更新。
func TestApplyAssistantPatchRejectsAtomically(t *testing.T) {
	base := model.AgentAssistant{Id: 3, Name: "原名", Description: "原简介", Mode: ModeAuto}
	bad := "bogus"
	emptyName := ""
	a := base
	if err := applyAssistantPatch(&a, assistantPatch{Name: &emptyName, Mode: &bad}); err == nil {
		t.Fatal("非法 mode 应当被拒")
	}
	if a != base {
		t.Fatalf("校验失败不该改动任何字段: got=%+v want=%+v", a, base)
	}
}

// ---------- handler：DB 未初始化 ----------

func TestAssistantHandlersWithoutDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = old })

	cases := []struct {
		name   string
		call   func(*gin.Context)
		method string
	}{
		{"list", listAssistants, http.MethodGet},
		{"create", createAssistant, http.MethodPost},
		{"update", updateAssistant, http.MethodPatch},
		{"delete", deleteAssistant, http.MethodDelete},
	}
	for _, tc := range cases {
		c, w := newTestContext(tc.method, "/api/assistants/1", "")
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		tc.call(c) // 必须不 panic
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: DB 未初始化应返回 500，实际 %d", tc.name, w.Code)
		}
	}
}

// newTestContext 造一个最小 gin 上下文，用于直接调用 handler 的「无 DB」分支。
func newTestContext(method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	c.Request = req
	return c, w
}
