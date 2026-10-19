package rag

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParseFactsTolerant(t *testing.T) {
	cases := []string{
		`{"facts":[{"text":"用户的项目代号是蓝鲸","entities":["蓝鲸"]}]}`,
		"```json\n{\"facts\":[{\"text\":\"用户的项目代号是蓝鲸\",\"entities\":[\"蓝鲸\"]}]}\n```",
		"好的，结果如下：\n{\"facts\":[{\"text\":\"用户的项目代号是蓝鲸\",\"entities\":[\"蓝鲸\"]}]}\n以上。",
	}
	for _, raw := range cases {
		facts, err := parseFacts(raw)
		if err != nil {
			t.Fatalf("应当能解析: %v（输入 %q）", err, raw)
		}
		if len(facts) != 1 || facts[0].Text != "用户的项目代号是蓝鲸" {
			t.Fatalf("解析结果不对: %+v", facts)
		}
		if len(facts[0].Entities) != 1 || facts[0].Entities[0] != "蓝鲸" {
			t.Fatalf("实体应当保留: %+v", facts[0])
		}
	}
}

func TestParseFactsRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "没有 JSON", `{"facts": "不是数组"}`, `{"foo":1}`} {
		if _, err := parseFacts(raw); err == nil {
			t.Fatalf("%q 应当解析失败", raw)
		}
	}
}

func TestParseFactsSkipsEmptyText(t *testing.T) {
	facts, err := parseFacts(`{"facts":[{"text":"  "},{"text":"有用的"},{"text":"  有实体  ","entities":[" X "]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("空文本应当被跳过，实际 %+v", facts)
	}
	if facts[1].Entities[0] != "X" {
		t.Fatalf("实体应当 trim: %+v", facts[1])
	}
}

func TestExtractFactsUsesCallerAndClamps(t *testing.T) {
	// 造 12 条，验证被截到 MaxFactsPerTurn。
	var sb strings.Builder
	sb.WriteString(`{"facts":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"text":"事实`)
		sb.WriteString(string(rune('A' + i)))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`]}`)

	var gotSystem, gotUser string
	call := func(_ context.Context, system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return sb.String(), nil
	}

	facts, err := ExtractFacts(context.Background(), call, "用户说的话", "AI 的回复", "- write（成功）：Created a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != MaxFactsPerTurn {
		t.Fatalf("应当被截到 %d 条，实际 %d", MaxFactsPerTurn, len(facts))
	}
	if !strings.Contains(gotSystem, "记忆抽取器") {
		t.Fatalf("system 提示词不对: %q", gotSystem)
	}
	for _, want := range []string{"用户说的话", "AI 的回复", "write（成功）"} {
		if !strings.Contains(gotUser, want) {
			t.Fatalf("喂给模型的输入应当包含 %q，实际 %q", want, gotUser)
		}
	}
}

func TestParseFactsScopeImportanceEmotion(t *testing.T) {
	// 新增字段：scope / importance / emotion 要能解析；缺省时落到兼容默认值（""/0.5/0）。
	raw := `{"facts":[
		{"text":"用户喜欢喝美式咖啡","entities":["咖啡"],"scope":"life","importance":0.8,"emotion":0.6},
		{"text":"项目的部署端口是 8300","scope":"work","importance":1},
		{"text":"随口一提的天气","scope":"不存在的分类","importance":0.1,"emotion":2},
		{"text":"没有附加字段"}
	]}`
	facts, err := parseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 {
		t.Fatalf("应当解析 4 条，实际 %d", len(facts))
	}
	// importance 0.8 就近归到 1；emotion 原样保留。
	if facts[0].Scope != ScopeLife || facts[0].Importance != 1 || math.Abs(facts[0].Emotion-0.6) > 1e-9 {
		t.Fatalf("第 1 条解析不对：%+v", facts[0])
	}
	// 缺省 emotion → 0。
	if facts[1].Scope != ScopeWork || facts[1].Importance != 1 || facts[1].Emotion != 0 {
		t.Fatalf("第 2 条解析不对：%+v", facts[1])
	}
	// 非法 scope → ""；importance 0.1 → 0；emotion 2 夹到 1。
	if facts[2].Scope != "" || facts[2].Importance != 0 || facts[2].Emotion != 1 {
		t.Fatalf("第 3 条应被归一：%+v", facts[2])
	}
	// 完全没有附加字段：落到兼容默认值。
	if facts[3].Scope != "" || facts[3].Importance != 0.5 || facts[3].Emotion != 0 {
		t.Fatalf("缺省字段应落到默认值：%+v", facts[3])
	}
}

func TestExtractFactsPropagatesErrors(t *testing.T) {
	fail := func(context.Context, string, string) (string, error) { return "", errors.New("模型不可用") }
	if _, err := ExtractFacts(context.Background(), fail, "u", "a", ""); err == nil {
		t.Fatal("模型失败应当把错误抛给上层（由上层降级）")
	}
	if _, err := ExtractFacts(context.Background(), nil, "u", "a", ""); err == nil {
		t.Fatal("空 caller 应当报错")
	}
	if _, err := ExtractFacts(context.Background(), fail, "", "", ""); err == nil {
		t.Fatal("没有任何内容时应当报错")
	}
}
