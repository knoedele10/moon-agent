package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 技能库目录是用户手填的，校验必须挡住三类会出事的值：
// 相对路径（跟着 CWD 漂移）、盘符根（等于让服务端递归扫整块盘）、指向文件的路径。
func TestValidateSkillImportDirRejectsBadInput(t *testing.T) {
	if _, err := validateSkillImportDir("   "); err == nil {
		t.Fatal("空路径应当被拒")
	}
	if _, err := validateSkillImportDir("relative/skills"); err == nil {
		t.Fatal("相对路径应当被拒")
	}

	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if _, err := validateSkillImportDir(root); err == nil {
		t.Fatalf("盘符根 %q 应当被拒", root)
	}

	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSkillImportDir(file); err == nil {
		t.Fatal("指向文件的路径应当被拒")
	}

	missing := filepath.Join(t.TempDir(), "not-exist")
	if _, err := validateSkillImportDir(missing); err == nil {
		t.Fatal("不存在的目录应当被拒（这里刻意不自动创建：填错目录应该报错而不是造一个空目录）")
	}
}

func TestValidateSkillImportDirAcceptsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "我的 技能库")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := validateSkillImportDir(dir)
	if err != nil {
		t.Fatalf("中文/空格目录应当被接受: %v", err)
	}
	if got != filepath.Clean(dir) {
		t.Fatalf("应返回规范化路径: got=%q want=%q", got, filepath.Clean(dir))
	}
	if !strings.HasSuffix(got, "我的 技能库") {
		t.Fatalf("路径被改坏了: %q", got)
	}
}
