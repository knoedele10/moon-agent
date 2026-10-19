package web

import (
	"net/http"
	"strings"
	"testing"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// ---------- 纯逻辑：校验 ----------

func TestNormalizePromptTitleTrimsAndTruncates(t *testing.T) {
	got, err := normalizePromptTitle("  周报模板  ")
	if err != nil || got != "周报模板" {
		t.Fatalf("应当去空格: got=%q err=%v", got, err)
	}

	long := strings.Repeat("标", 80)
	got, err = normalizePromptTitle(long)
	if err != nil {
		t.Fatalf("过长标题应当截断: %v", err)
	}
	if len([]rune(got)) != promptTitleMaxRunes {
		t.Fatalf("应当截到 %d 个 rune: %d", promptTitleMaxRunes, len([]rune(got)))
	}

	if _, err := normalizePromptTitle(" \t "); err == nil {
		t.Fatal("空标题应当被拒")
	}
}

// ---------- 纯逻辑：三态更新 ----------

func TestApplyPromptPatchTriState(t *testing.T) {
	base := model.AgentPrompt{Id: 9, UserID: "u1", Title: "旧标题", Content: "旧内容"}

	// 都不传 → 不改。
	p := base
	if err := applyPromptPatch(&p, nil, nil); err != nil {
		t.Fatalf("空补丁不该报错: %v", err)
	}
	if p != base {
		t.Fatalf("空补丁不该改动记录: %+v", p)
	}

	// 只传标题 → 内容保持原样。
	title := "新标题"
	p = base
	if err := applyPromptPatch(&p, &title, nil); err != nil {
		t.Fatalf("只改标题不该报错: %v", err)
	}
	if p.Title != title || p.Content != base.Content {
		t.Fatalf("三态更新不对: %+v", p)
	}

	// 只传内容 → 标题保持原样。
	content := "新内容"
	p = base
	if err := applyPromptPatch(&p, nil, &content); err != nil {
		t.Fatalf("只改内容不该报错: %v", err)
	}
	if p.Content != content || p.Title != base.Title {
		t.Fatalf("三态更新不对: %+v", p)
	}
}

func TestApplyPromptPatchRejectsAtomically(t *testing.T) {
	base := model.AgentPrompt{Id: 2, Title: "原题", Content: "原内容"}
	empty := "   "
	p := base
	if err := applyPromptPatch(&p, &empty, nil); err == nil {
		t.Fatal("空标题应当被拒")
	}
	if p != base {
		t.Fatalf("校验失败不该改动记录: %+v", p)
	}

	p = base
	if err := applyPromptPatch(&p, nil, &empty); err == nil {
		t.Fatal("空内容应当被拒")
	}
	if p != base {
		t.Fatalf("校验失败不该改动记录: %+v", p)
	}
}

// ---------- handler：DB 未初始化 ----------

func TestPromptHandlersWithoutDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = old })

	cases := []struct {
		name   string
		call   func(*gin.Context)
		method string
	}{
		{"list", listPrompts, http.MethodGet},
		{"create", createPrompt, http.MethodPost},
		{"update", updatePrompt, http.MethodPatch},
		{"delete", deletePrompt, http.MethodDelete},
	}
	for _, tc := range cases {
		c, w := newTestContext(tc.method, "/api/prompts/1", "")
		c.Params = gin.Params{{Key: "id", Value: "1"}}
		tc.call(c) // 必须不 panic
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: DB 未初始化应返回 500，实际 %d", tc.name, w.Code)
		}
	}
}
