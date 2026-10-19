package rag

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// longChineseText 造一段没有任何空白、句子以「。」收尾的中文长文，
// 这样切块只会在句末标点处断开，便于断言重叠。
func longChineseText(minRunes int) string {
	unit := "这是一段用来验证切块的中文长文。"
	return strings.Repeat(unit, minRunes/utf8.RuneCountInString(unit)+2)
}

func stripSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func assertWithinLimit(t *testing.T, chunks []string, max int) {
	t.Helper()
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > max {
			t.Fatalf("第 %d 块长度 %d 超过上限 %d: %q", i, n, max, c)
		}
	}
}

// assertOverlap 要求第 i 块以第 i-1 块末尾的 OverlapRunes 个字符开头。
func assertOverlap(t *testing.T, chunks []string, overlap int) {
	t.Helper()
	for i := 1; i < len(chunks); i++ {
		prev, cur := []rune(chunks[i-1]), []rune(chunks[i])
		k := overlap
		if k > len(prev) {
			k = len(prev)
		}
		want := string(prev[len(prev)-k:])
		if !strings.HasPrefix(chunks[i], want) {
			t.Fatalf("第 %d 块开头不是第 %d 块末尾 %d 个字符\n尾部: %q\n开头: %q", i, i-1, k, want, string(cur[:min(k, len(cur))]))
		}
	}
}

func TestChunkTextBasic(t *testing.T) {
	opt := DefaultChunkOptions()
	cases := []struct {
		name  string
		text  string
		opt   ChunkOptions
		check func(t *testing.T, chunks []string)
	}{
		{
			name: "空字符串返回 nil",
			text: "", opt: opt,
			check: func(t *testing.T, chunks []string) {
				if chunks != nil {
					t.Fatalf("期望 nil，实际 %q", chunks)
				}
			},
		},
		{
			name: "纯空白返回 nil",
			text: " \n\t\r\n  ", opt: opt,
			check: func(t *testing.T, chunks []string) {
				if chunks != nil {
					t.Fatalf("期望 nil，实际 %q", chunks)
				}
			},
		},
		{
			name: "长度刚好等于上限只返回一块",
			text: strings.Repeat("字", opt.MaxRunes), opt: opt,
			check: func(t *testing.T, chunks []string) {
				if len(chunks) != 1 || chunks[0] != strings.Repeat("字", opt.MaxRunes) {
					t.Fatalf("期望恰好 1 块且内容不变，实际 %d 块", len(chunks))
				}
			},
		},
		{
			name: "超长无标点单行走硬切",
			text: strings.Repeat("长", opt.MaxRunes*3+7), opt: opt,
			check: func(t *testing.T, chunks []string) {
				if len(chunks) < 2 {
					t.Fatalf("期望切成多块，实际 %d 块", len(chunks))
				}
				assertWithinLimit(t, chunks, opt.MaxRunes)
				assertOverlap(t, chunks, opt.OverlapRunes)
			},
		},
		{
			name: "非法参数退回默认值",
			text: longChineseText(300), opt: ChunkOptions{MaxRunes: 0, OverlapRunes: -1},
			check: func(t *testing.T, chunks []string) {
				if len(chunks) == 0 {
					t.Fatal("不该返回空")
				}
				assertWithinLimit(t, chunks, opt.MaxRunes)
			},
		},
		{
			name: "重叠不小于上限也不死循环",
			text: longChineseText(300), opt: ChunkOptions{MaxRunes: 50, OverlapRunes: 50},
			check: func(t *testing.T, chunks []string) {
				if len(chunks) == 0 {
					t.Fatal("不该返回空")
				}
				assertWithinLimit(t, chunks, 50)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, ChunkText(tc.text, tc.opt))
		})
	}
}

func TestChunkTextLongChinese(t *testing.T) {
	opt := DefaultChunkOptions()
	text := longChineseText(2000)
	if utf8.RuneCountInString(text) <= 2000 {
		t.Fatalf("测试文本不够长: %d", utf8.RuneCountInString(text))
	}

	chunks := ChunkText(text, opt)
	if len(chunks) < 2 {
		t.Fatalf("长文应切成多块，实际 %d 块", len(chunks))
	}
	assertWithinLimit(t, chunks, opt.MaxRunes)
	assertOverlap(t, chunks, opt.OverlapRunes)

	// 去掉每块与上一块重叠的部分再拼回去，应还原原文。
	var b strings.Builder
	b.WriteString(chunks[0])
	for i := 1; i < len(chunks); i++ {
		prev, cur := []rune(chunks[i-1]), []rune(chunks[i])
		k := min(opt.OverlapRunes, len(prev))
		b.WriteString(string(cur[min(k, len(cur)):]))
	}
	if got, want := stripSpaces(b.String()), stripSpaces(text); got != want {
		t.Fatalf("拼回后与原文不一致\n拼回长度: %d\n原文长度: %d", len([]rune(got)), len([]rune(want)))
	}
}

func TestChunkTextMarkdown(t *testing.T) {
	opt := DefaultChunkOptions()
	var b strings.Builder
	b.WriteString("# 切块测试\n\n## 第一节\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString("这里是正文段落，讲一些和切块有关的细节，需要保证段落被完整保留下来。\n\n")
	}
	b.WriteString("- 列表项一\n- 列表项二\n- 列表项三\n\n```go\nfunc main() {\n\tprintln(\"hello\")\n}\n```\n\n结尾段落，同样要完整。\n")
	text := b.String()

	chunks := ChunkText(text, opt)
	if len(chunks) < 2 {
		t.Fatalf("应切成多块，实际 %d 块", len(chunks))
	}
	assertWithinLimit(t, chunks, opt.MaxRunes)

	// 每块都必须来自原文，不能凭空造内容。
	for i, c := range chunks {
		if !strings.Contains(text, c) {
			t.Fatalf("第 %d 块不是原文子串: %q", i, c)
		}
	}
	// 每个非空行都应完整出现在某一块里，不能丢内容。
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		found := false
		for _, c := range chunks {
			if strings.Contains(c, line) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("这一行没出现在任何块里: %q", line)
		}
	}
}

func TestChunkOptionsDefaults(t *testing.T) {
	if got := DefaultChunkOptions(); got.MaxRunes != 600 || got.OverlapRunes != 80 {
		t.Fatalf("默认值不对: %+v", got)
	}
}
