package skill

import (
	"strings"
	"testing"
)

// TestParseMarkdownFrontmatter 覆盖最常走的路径：--- 包起来的 name / description。
// 技能库里几乎都是这个形状，解析错一个字段，界面上整条技能就是坏的。
func TestParseMarkdownFrontmatter(t *testing.T) {
	raw := "---\nname: caveman\ndescription: 极简沟通模式\n---\n把话说短，能省则省。\n"
	name, desc, body := ParseMarkdown(raw)
	if name != "caveman" {
		t.Fatalf("name = %q, want caveman", name)
	}
	if desc != "极简沟通模式" {
		t.Fatalf("desc = %q, want 极简沟通模式", desc)
	}
	if !strings.Contains(body, "把话说短") {
		t.Fatalf("body 丢了正文: %q", body)
	}
	if strings.Contains(body, "name:") || strings.Contains(body, "---") {
		t.Fatalf("body 不应包含 frontmatter: %q", body)
	}
}

// TestParseMarkdownBlockScalarFolded 是回归测试。
//
// description 用 YAML 折叠标量（一个大于号）时，早期版本把这个符号本身当成了描述内容
// （界面上显示成一个竖线），后续缩进行整段丢失。gstack 那批技能全是这个写法，
// 漏掉等于整批技能没有简介——而简介正是模型挑技能的唯一依据。
func TestParseMarkdownBlockScalarFolded(t *testing.T) {
	raw := "---\nname: gstack\ndescription: >\n  MUST USE when the user asks to\n  第二行说明\n---\n正文\n"
	_, desc, _ := ParseMarkdown(raw)
	if desc == "" || desc == ">" || strings.HasPrefix(desc, ">") {
		t.Fatalf("描述里不能残留块标量符号: %q", desc)
	}
	if !strings.Contains(desc, "MUST USE when the user asks to") {
		t.Fatalf("第一行块内容丢失: %q", desc)
	}
	if !strings.Contains(desc, "第二行说明") {
		t.Fatalf("后续缩进行丢失（折叠标量必须跨行拼接）: %q", desc)
	}
}

// 字面标量（一个竖线）同样要跨行拼接。实现里它与折叠标量走同一条分支（按空格连接），
// 这里钉住的是“内容不丢”，而不是换行是否原样保留。
func TestParseMarkdownBlockScalarLiteral(t *testing.T) {
	raw := "---\nname: officecli\ndescription: |\n  第一行\n  第二行\n---\n正文\n"
	_, desc, _ := ParseMarkdown(raw)
	if desc == "|" || strings.HasPrefix(desc, "|") {
		t.Fatalf("描述里不能残留块标量符号: %q", desc)
	}
	if !strings.Contains(desc, "第一行") || !strings.Contains(desc, "第二行") {
		t.Fatalf("字面标量内容丢失: %q", desc)
	}
}

// 用户手写的 frontmatter 常带引号，入库前必须剥掉，否则技能名里会多两个引号，
// 模型按名字点名时就对不上。
func TestParseMarkdownQuotedValues(t *testing.T) {
	raw := "---\nname: \"my-skill\"\ndescription: '带引号的说明'\n---\n正文\n"
	name, desc, _ := ParseMarkdown(raw)
	if name != "my-skill" {
		t.Fatalf("双引号应被剥掉: name = %q", name)
	}
	if desc != "带引号的说明" {
		t.Fatalf("单引号应被剥掉: desc = %q", desc)
	}
}

// 没有 frontmatter 时取一级标题当名字：这类手写技能文件很常见。
func TestParseMarkdownFallsBackToHeading(t *testing.T) {
	raw := "# 我的技能\n\n这里是说明。\n"
	name, _, _ := ParseMarkdown(raw)
	if name != "我的技能" {
		t.Fatalf("没有 frontmatter 时应取一级标题: name = %q", name)
	}
}

// TestParseMarkdownNoNameNeverYieldsSeparator 是回归测试。
//
// 有 frontmatter 但没有 name 字段时，早期版本取“第一个非空行”，于是技能名变成三个横线。
// 技能名是模型挑选技能的钥匙，变成分隔符等于这条技能永远检索不到。
func TestParseMarkdownNoNameNeverYieldsSeparator(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"正文是普通行", "---\ndescription: 只有简介\n---\n正文\n", "正文"},
		{"正文有一级标题", "---\ndescription: 只有简介\n---\n\n# 实际标题\n", "实际标题"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, _, _ := ParseMarkdown(tc.raw)
			if name == "---" {
				t.Fatal("技能名不能是 frontmatter 分隔符")
			}
			if name != tc.want {
				t.Fatalf("name = %q, want %q", name, tc.want)
			}
		})
	}
}

// 名字/简介超长要截断：技能名会进数据库唯一索引（128 字节），简介会进提示词，
// 不截断会撑爆上下文或让入库失败。
func TestParseMarkdownTruncatesLongFields(t *testing.T) {
	longName := strings.Repeat("技", 200)
	longDesc := strings.Repeat("说", 1500)
	raw := "---\nname: " + longName + "\ndescription: " + longDesc + "\n---\n正文\n"
	name, desc, _ := ParseMarkdown(raw)
	if got := len([]rune(name)); got != 120 {
		t.Fatalf("name 应截断到 120 个字符, got %d", got)
	}
	if got := len([]rune(desc)); got != 1000 {
		t.Fatalf("desc 应截断到 1000 个字符, got %d", got)
	}
}

// Windows 上编辑出来的技能文件是 CRLF，解析前必须归一化，否则块标量判断会错位。
func TestParseMarkdownHandlesCRLF(t *testing.T) {
	raw := "---\r\nname: crlf\r\ndescription: 换行符\r\n---\r\n正文\r\n"
	name, desc, _ := ParseMarkdown(raw)
	if name != "crlf" {
		t.Fatalf("name = %q, want crlf", name)
	}
	if desc != "换行符" {
		t.Fatalf("desc = %q, want 换行符", desc)
	}
}
