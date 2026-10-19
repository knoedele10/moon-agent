package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillFile 在临时目录里造一个 SKILL.md（连同父目录）。
// 全部落在 t.TempDir()：测试不依赖真实技能库，也不该往用户目录里写东西。
func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func itemNames(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// TestScanDirDedupesAndSkips 一次钉住扫描的几条核心规则：
// 隐藏目录跳过并计数、node_modules 跳过、同名只留层级最浅的一份、缺名字用目录名兜底。
// 这些规则错了，导入结果会随目录结构悄悄变化（旧副本覆盖新技能），很难排查。
func TestScanDirDedupesAndSkips(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "SKILL.md"), "---\nname: alpha\ndescription: 顶层\n---\n顶层正文\n")
	writeSkillFile(t, filepath.Join(root, "sub", "SKILL.md"), "---\nname: alpha\ndescription: 深层副本\n---\n深层正文\n")
	writeSkillFile(t, filepath.Join(root, "sub", "beta", "SKILL.md"), "---\nname: beta\n---\nbeta 正文\n")
	// 隐藏目录里是工具自己同步出来的旧副本，扫进去只会用旧副本覆盖同名技能。
	writeSkillFile(t, filepath.Join(root, ".hidden", "SKILL.md"), "---\nname: hidden\n---\n隐藏正文\n")
	// node_modules 里不可能有真技能，递归进去只是白扫一堆文件。
	writeSkillFile(t, filepath.Join(root, "node_modules", "pkg", "SKILL.md"), "---\nname: nodemod\n---\n依赖正文\n")
	// 只有 frontmatter、没有 name：必须用目录名兜底，不能留空。
	writeSkillFile(t, filepath.Join(root, "gamma", "SKILL.md"), "---\ndescription: 没有名字\n---\n")

	rep, err := ScanDir(root, ScanOptions{})
	if err != nil {
		t.Fatalf("扫描不该失败: %v", err)
	}
	if rep.Files != 4 {
		t.Fatalf("Files 应只数真正走到的 SKILL.md（隐藏目录与 node_modules 不计）: got %d", rep.Files)
	}
	if rep.SkippedHidden != 1 {
		t.Fatalf("SkippedHidden 应只统计被跳过的隐藏目录: got %d", rep.SkippedHidden)
	}
	if len(rep.Items) != 3 {
		t.Fatalf("同名技能应去重: got %d 条 (%v)", len(rep.Items), itemNames(rep.Items))
	}

	byName := map[string]Item{}
	for _, it := range rep.Items {
		byName[it.Name] = it
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("缺少技能 %q, 实际有 %v", want, itemNames(rep.Items))
		}
	}
	if _, ok := byName["hidden"]; ok {
		t.Fatal("隐藏目录里的技能不该被扫进来")
	}
	if _, ok := byName["nodemod"]; ok {
		t.Fatal("node_modules 里的技能不该被扫进来")
	}
	// 去重保留层级最浅的一份，而不是最后遍历到的那份。
	if got := byName["alpha"].Source; got != filepath.Join(root, "SKILL.md") {
		t.Fatalf("alpha 应保留最浅的一份: got %q", got)
	}
	if len(rep.Duplicates) != 1 || !strings.Contains(rep.Duplicates[0], "alpha") {
		t.Fatalf("Duplicates 应记录被丢掉的 alpha: %v", rep.Duplicates)
	}
	if rep.Items[0].Name != "alpha" {
		t.Fatalf("层级最浅的应排在最前: got %q", rep.Items[0].Name)
	}
}

// 同层同名时取字典序小的路径：否则同一份技能库里“谁赢”会随文件系统遍历顺序变化。
func TestScanDirTieBreaksLexicographically(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "SKILL.md")
	b := filepath.Join(root, "b", "SKILL.md")
	writeSkillFile(t, a, "---\nname: same\n---\nA\n")
	writeSkillFile(t, b, "---\nname: same\n---\nB\n")

	rep, err := ScanDir(root, ScanOptions{})
	if err != nil {
		t.Fatalf("扫描不该失败: %v", err)
	}
	if len(rep.Items) != 1 {
		t.Fatalf("同名只应留一条: %v", itemNames(rep.Items))
	}
	if rep.Items[0].Source != a {
		t.Fatalf("同层应取字典序小的路径: got %q want %q", rep.Items[0].Source, a)
	}
	if len(rep.Duplicates) != 1 {
		t.Fatalf("应记录一条重复: %v", rep.Duplicates)
	}
}

// 两个开关型选项：IncludeHidden 影响扫描范围，MaxBodyRunes 防止超大技能撑爆数据库。
func TestScanDirOptions(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, ".hidden", "SKILL.md"), "---\nname: hidden\n---\n")
	writeSkillFile(t, filepath.Join(root, "big", "SKILL.md"), "---\nname: big\n---\n"+strings.Repeat("字", 50)+"\n")

	rep, err := ScanDir(root, ScanOptions{IncludeHidden: true})
	if err != nil {
		t.Fatalf("扫描不该失败: %v", err)
	}
	if rep.SkippedHidden != 0 {
		t.Fatalf("IncludeHidden 时不该统计跳过: got %d", rep.SkippedHidden)
	}
	if len(rep.Items) != 2 {
		t.Fatalf("IncludeHidden 时应扫到隐藏技能: %v", itemNames(rep.Items))
	}

	rep, err = ScanDir(root, ScanOptions{MaxBodyRunes: 10})
	if err != nil {
		t.Fatalf("扫描不该失败: %v", err)
	}
	var big *Item
	for i := range rep.Items {
		if rep.Items[i].Name == "big" {
			big = &rep.Items[i]
		}
	}
	if big == nil {
		t.Fatalf("没找到 big: %v", itemNames(rep.Items))
	}
	if got := len([]rune(big.Body)); got != 10 {
		t.Fatalf("MaxBodyRunes 应截断正文: got %d 个字符", got)
	}
}

// root 不存在或是文件时必须报错，而不是返回一个空报告：
// 调用方要靠这个错误告诉用户“目录填错了”，空报告会被当成“这个库是空的”。
func TestScanDirRejectsBadRoot(t *testing.T) {
	if _, err := ScanDir(filepath.Join(t.TempDir(), "不存在的目录"), ScanOptions{}); err == nil {
		t.Fatal("不存在的目录应当报错")
	}
	file := filepath.Join(t.TempDir(), "notadir.txt")
	writeSkillFile(t, file, "x")
	if _, err := ScanDir(file, ScanOptions{}); err == nil {
		t.Fatal("指向文件应当报错")
	}
}
