package web

import (
	"context"
	"encoding/json"
	"testing"

	"blog-my/internal/model"
)

// 技能名会被写进提示词清单、也会被 skill_search 当关键词匹配，
// 所以「同一件事的不同写法要收敛成同一个名字」必须钉住——
// 否则 "Build Installer" 与 "build-installer" 会变成两条技能。

func TestNormalizeSkillName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Build Installer", "build-installer"},
		{"build_installer", "build-installer"},
		{"  build--installer  ", "build-installer"},
		{"a//b", "a-b"},
		{"...leading", "leading"},
		{"trailing...", "trailing"},
		{"Already-Kebab", "already-kebab"},
		{"with.dots", "with-dots"},
		{"", ""},
		{"   ", ""},
		{"!!!", ""},
		{"中文技能名", "中文技能名"},
	}
	for _, c := range cases {
		if got := normalizeSkillName(c.in); got != c.want {
			t.Errorf("normalizeSkillName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeSkillNameTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	got := normalizeSkillName(long)
	if n := len([]rune(got)); n != skillNameMaxLen {
		t.Fatalf("超长名字应截到 %d 个字符，实际 %d", skillNameMaxLen, n)
	}
}

// 没有数据库时不能假装保存成功——必须明确报错。
func TestSkillWriteToolNoDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	defer func() { model.DB = old }()

	raw, _ := json.Marshal(map[string]any{
		"name": "build-installer", "description": "打包 Windows 安装包。", "body": "# 标题\n内容",
	})
	_, err := skillWriteTool{userID: "u1"}.Execute(context.Background(), raw, nil)
	if err == nil {
		t.Fatal("数据库未初始化时应当返回错误")
	}
}

// 参数校验：空名字 / 空正文都要被挡下来，而不是写一条空技能进库。
func TestSkillWriteToolValidation(t *testing.T) {
	old := model.DB
	model.DB = nil // 校验发生在访问数据库之前
	defer func() { model.DB = old }()

	tool := skillWriteTool{userID: "u1"}
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"名字为空", map[string]any{"name": "  ", "description": "d", "body": "b"}},
		{"名字全是标点", map[string]any{"name": "!!!", "description": "d", "body": "b"}},
		{"正文为空", map[string]any{"name": "ok-name", "description": "d", "body": "   "}},
	} {
		raw, _ := json.Marshal(c.body)
		if _, err := tool.Execute(context.Background(), raw, nil); err == nil {
			t.Errorf("%s：应当报错，实际通过了", c.name)
		}
	}
}

// 参数不是合法 JSON 时要给出清楚的错误，而不是 panic。
func TestSkillWriteToolBadJSON(t *testing.T) {
	old := model.DB
	model.DB = nil
	defer func() { model.DB = old }()

	if _, err := (skillWriteTool{userID: "u1"}).Execute(context.Background(), json.RawMessage("{not json"), nil); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}
