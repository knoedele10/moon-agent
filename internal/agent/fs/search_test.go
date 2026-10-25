package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchFixture 建一棵有代表性的工作区树：
//   - 普通源码/文档（应被检索到）
//   - node_modules / .git / build（默认跳过）
//   - 二进制文件（含 NUL，应跳过）
//   - 子目录（验证 ** 与相对展示路径）
func searchFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc Hello() string {\n\treturn \"hello world\"\n}\n")
	mustWriteFile(t, filepath.Join(root, "sub", "deep", "util.go"), "package sub\n\n// hello from deep\nfunc Deep() {}\n")
	mustWriteFile(t, filepath.Join(root, "README.md"), "# Title\n\nSome Hello Text\n")
	mustWriteFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "hello dependency\n")
	mustWriteFile(t, filepath.Join(root, ".git", "config"), "hello git\n")
	mustWriteFile(t, filepath.Join(root, "build", "out.txt"), "hello build\n")
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte("hello\x00world"), 0o644); err != nil {
		t.Fatalf("写二进制文件失败: %v", err)
	}
	return root
}

func TestGrepBasicCaseInsensitive(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// 默认不区分大小写：main.go / util.go / README.md 都要命中，依赖与二进制不能出现。
	for _, want := range []string{"main.go:4:", "sub/deep/util.go:3:", "README.md:3:"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("缺少命中 %q：\n%s", want, res.Content)
		}
	}
	for _, banned := range []string{"node_modules", ".git/", "build/out.txt", "binary.dat"} {
		if strings.Contains(res.Content, banned) {
			t.Errorf("不应搜索到忽略目录/二进制 %q：\n%s", banned, res.Content)
		}
	}
}

func TestGrepCaseSensitive(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "hello", "case_sensitive": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "main.go:4:") {
		t.Fatalf("应命中全小写 hello：%s", res.Content)
	}
	if strings.Contains(res.Content, "README.md") {
		t.Fatalf("区分大小写时不应命中 Hello：%s", res.Content)
	}
}

func TestGrepFixedStrings(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "a.b\naxb\n")
	ex := execFor(t, root)

	regexRes, err := call(GrepTool{}, ex, map[string]any{"pattern": "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(regexRes.Content, "a.txt:2:") {
		t.Fatalf("正则 a.b 应命中 axb：%s", regexRes.Content)
	}
	literalRes, err := call(GrepTool{}, ex, map[string]any{"pattern": "a.b", "fixed_strings": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(literalRes.Content, "a.txt:2:") {
		t.Fatalf("字面量 a.b 不应命中 axb：%s", literalRes.Content)
	}
	if !strings.Contains(literalRes.Content, "a.txt:1:") {
		t.Fatalf("字面量 a.b 应命中第一行：%s", literalRes.Content)
	}
}

func TestGrepFileGlob(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "hello", "glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "main.go") || !strings.Contains(res.Content, "sub/deep/util.go") {
		t.Fatalf("*.go 应命中所有层级的 Go 文件：%s", res.Content)
	}
	if strings.Contains(res.Content, "README.md") {
		t.Fatalf("glob 应排除 README.md：%s", res.Content)
	}
}

func TestGrepContextLines(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "l1\nl2\nmatch\nl4\nl5\n")
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "match", "context": 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.txt:2- l2", "a.txt:3: match", "a.txt:4- l4"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("上下文输出缺少 %q：\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "a.txt:1-") || strings.Contains(res.Content, "a.txt:5-") {
		t.Errorf("上下文不应越界多带：\n%s", res.Content)
	}
}

func TestGrepLimitAndTruncationNotice(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("hit line\n")
	}
	mustWriteFile(t, filepath.Join(root, "a.txt"), b.String())
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "hit", "limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content, "a.txt:"); n != 3 {
		t.Fatalf("limit=3 应只返回 3 条命中，实际 %d：\n%s", n, res.Content)
	}
	if !strings.Contains(res.Content, "还有更多匹配") {
		t.Fatalf("截断时应提示收窄范围：\n%s", res.Content)
	}
}

func TestGrepNoMatchGivesNextStep(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "nothing here")
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "未找到匹配") || !strings.Contains(res.Content, "glob") {
		t.Fatalf("无命中时应给出收窄/换工具建议：%s", res.Content)
	}
}

func TestGrepInvalidRegexIsRepairable(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)

	_, err := call(GrepTool{}, ex, map[string]any{"pattern": "("})
	mustCode(t, err, CodeInvalidArgs)
	if !strings.Contains(err.Error(), "fixed_strings") {
		t.Fatalf("参数错误应告诉模型怎么改：%v", err)
	}
}

func TestGrepLargeSingleFileIsRejectedWithNextStep(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("a", maxSearchFileBytes+1024)
	mustWriteFile(t, filepath.Join(root, "big.log"), big)
	ex := execFor(t, root)

	_, err := call(GrepTool{}, ex, map[string]any{"pattern": "a", "path": "big.log"})
	mustCode(t, err, CodeInvalidArgs)
	if !strings.Contains(err.Error(), "read") {
		t.Fatalf("应建议改用 read 分段：%v", err)
	}
}

func TestGrepEscapeDenied(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)

	_, err := call(GrepTool{}, ex, map[string]any{"pattern": "x", "path": "../"})
	mustCode(t, err, CodeSandboxDenied)
}

func TestGrepSingleFile(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GrepTool{}, ex, map[string]any{"pattern": "func", "path": "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "main.go:3:") {
		t.Fatalf("单文件搜索应命中函数定义：%s", res.Content)
	}
	if strings.Contains(res.Content, "util.go") {
		t.Fatalf("单文件搜索不应扫到别的文件：%s", res.Content)
	}
}

func TestGlobDoubleStarAndNested(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GlobTool{}, ex, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go", "sub/deep/util.go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("**/*.go 应包含 %q：\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "node_modules") {
		t.Fatalf("应跳过 node_modules：%s", res.Content)
	}
}

func TestGlobBasenamePatternMatchesAnyDepth(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	res, err := call(GlobTool{}, ex, map[string]any{"pattern": "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "README.md") {
		t.Fatalf("*.md 应匹配根目录文件：%s", res.Content)
	}
	// 再放一份深层 md，确认无斜杠 pattern 也匹配 basename。
	mustWriteFile(t, filepath.Join(root, "docs", "guide.md"), "# guide")
	res, err = call(GlobTool{}, ex, map[string]any{"pattern": "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "docs/guide.md") {
		t.Fatalf("*.md 应匹配任意层级的同名文件：%s", res.Content)
	}
}

func TestGlobLimitAndNoMatch(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		mustWriteFile(t, filepath.Join(root, name), "package x")
	}
	ex := execFor(t, root)

	res, err := call(GlobTool{}, ex, map[string]any{"pattern": "*.go", "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content, ".go"); n < 2 || !strings.Contains(res.Content, "共 3 个文件") {
		t.Fatalf("limit 截断提示不对：%s", res.Content)
	}
	res, err = call(GlobTool{}, ex, map[string]any{"pattern": "*.rs"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "没有找到") {
		t.Fatalf("无结果时应给出提示：%s", res.Content)
	}
}

func TestGlobRequiresDirectoryAndRespectsSandbox(t *testing.T) {
	root := searchFixture(t)
	ex := execFor(t, root)

	if _, err := call(GlobTool{}, ex, map[string]any{"pattern": "*.go", "path": "main.go"}); err == nil {
		t.Fatal("glob 搜索根是文件时应报错")
	} else {
		mustCode(t, err, CodeInvalidArgs)
	}
	_, err := call(GlobTool{}, ex, map[string]any{"pattern": "*", "path": "../"})
	mustCode(t, err, CodeSandboxDenied)
}

func TestSearchToolsRegistered(t *testing.T) {
	tools := SearchTools()
	if len(tools) != 2 || tools[0].Name() != "grep" || tools[1].Name() != "glob" {
		t.Fatalf("检索工具清单不对：%v", tools)
	}
	for _, tool := range tools {
		params := tool.Parameters()
		props, _ := params["properties"].(map[string]any)
		if props["pattern"] == nil {
			t.Fatalf("%s 缺少 pattern 参数", tool.Name())
		}
	}
}
