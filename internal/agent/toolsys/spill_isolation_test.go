package toolsys

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"blog-my/internal/agent"
)

func TestSpillRejectsAnotherSessionAndSanitizedCollision(t *testing.T) {
	policy := SpillPolicy{Dir: t.TempDir(), MaxBytes: 10}
	wrapped := policy.Wrap(&fakeTool{name: "large", exec: func(context.Context, json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: strings.Repeat("保密内容", 100)}, nil
	}})
	owner := &agent.Exec{SessionID: "a/b"}
	res, err := wrapped.Execute(context.Background(), nil, owner)
	if err != nil {
		t.Fatal(err)
	}
	path := spilledPath(t, res)
	args, _ := json.Marshal(map[string]any{"file_path": path})
	for _, session := range []string{"another", "a_b", spillSessionPart(owner.SessionID)} {
		res, err := ReadResultTool(policy).Execute(context.Background(), args, &agent.Exec{SessionID: session})
		if err != nil || !res.IsError || displayCode(res) != "SPILL_SESSION_DENIED" {
			t.Fatalf("会话 %q 不应读取其他会话：%+v %v", session, res, err)
		}
	}
	res, err = ReadResultTool(policy).Execute(context.Background(), args, owner)
	if err != nil || res.IsError {
		t.Fatalf("原会话应可续读：%+v %v", res, err)
	}
}

func TestSpillSingleLongLinePaginatesWithoutLosingUnicode(t *testing.T) {
	content := strings.Repeat("中🙂文", 7000)
	policy := SpillPolicy{Dir: t.TempDir()}
	wrapped := policy.Wrap(&fakeTool{name: "large", exec: func(context.Context, json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: content}, nil
	}})
	ex := &agent.Exec{SessionID: "long-line"}
	res, err := wrapped.Execute(context.Background(), nil, ex)
	if err != nil {
		t.Fatal(err)
	}
	path := spilledPath(t, res)
	var recovered strings.Builder
	offset, charOffset := 1, 0
	for i := 0; i < 10; i++ {
		args, _ := json.Marshal(map[string]any{"file_path": path, "offset": offset, "char_offset": charOffset})
		page, err := ReadResultTool(policy).Execute(context.Background(), args, ex)
		if err != nil || page.IsError {
			t.Fatalf("分页失败：%+v %v", page, err)
		}
		if !utf8.ValidString(page.Content) || len(page.Content) > 32*1024 {
			t.Fatal("分页必须有界且为合法 UTF-8")
		}
		body := strings.SplitN(page.Content, "\n\n", 2)[1]
		body = strings.SplitN(body, "\n\n（", 2)[0]
		recovered.WriteString(body)
		offset = page.Display["next_offset"].(int)
		charOffset = page.Display["next_char_offset"].(int)
		if offset > 1 {
			break
		}
	}
	if recovered.String() != content {
		t.Fatalf("分页后恢复 %d 字符，期望 %d", recovered.Len(), len(content))
	}
}

func TestUnknownRiskCannotBecomeReadOnly(t *testing.T) {
	for _, risk := range []Risk{"", "typo"} {
		if got := normalizeRisk(risk); got == RiskRead {
			t.Fatalf("未知风险 %q 被当作只读", risk)
		}
	}
}
