package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/model"
)

// 工作区是用户能直接改的东西，校验必须挡住三类会出事的值：
// 相对路径（跟着 CWD 漂移）、盘符根（整块盘交出去）、指向文件的路径。
func TestValidateWorkspaceRejectsBadInput(t *testing.T) {
	if _, err := validateWorkspace("   "); err == nil {
		t.Fatal("空路径应当被拒")
	}
	if _, err := validateWorkspace("relative/workspace"); err == nil {
		t.Fatal("相对路径应当被拒")
	}

	// 盘符根（Windows 上 VolumeName 才有值；非 Windows 上这条会被 IsAbs 先拦）。
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if _, err := validateWorkspace(root); err == nil {
		t.Fatalf("盘符根 %q 应当被拒", root)
	}

	// 指向文件而不是目录。
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateWorkspace(file); err == nil {
		t.Fatal("指向文件的路径应当被拒")
	}
}

func TestValidateWorkspaceAcceptsAndCreatesDir(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "deep", "workspace")

	got, err := validateWorkspace(missing)
	if err != nil {
		t.Fatalf("不存在的目录应当自动创建: %v", err)
	}
	if got != filepath.Clean(missing) {
		t.Fatalf("应返回规范化路径: got=%q want=%q", got, filepath.Clean(missing))
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("目录应当已被创建: err=%v", err)
	}
}

// 没设过工作区时按「本机主人」回落环境变量；非主账号/未登录不回落。
//
// 这条策略来自审计 P1-3：AGENT_FS_ROOT 是本机全局目录，不能因为
// 任何人都能连上 127.0.0.1 就分发给每一个账号。
func TestAgentFSRootForFallsBackToEnv(t *testing.T) {
	oldDB := model.DB
	model.DB = nil // 固定成「没有账号」的单机场景，避免受同包其它用例影响
	t.Cleanup(func() { model.DB = oldDB })

	t.Setenv("AGENT_FS_ROOT", `D:\fallback-workspace`)
	if got := agentFSRootFor("u1"); got != `D:\fallback-workspace` {
		t.Fatalf("主账号应当回落到 AGENT_FS_ROOT: %q", got)
	}
	if got := agentFSRootFor(""); got != "" {
		t.Fatalf("未登录不该拿到回落工作区: %q", got)
	}

	t.Setenv("AGENT_FS_ROOT", "  ")
	if got := agentFSRootFor("u1"); got != "" {
		t.Fatalf("环境变量为空时应返回空串: %q", got)
	}
}

// 工作区路径里允许中文与空格（用户目录常见），不能被误拒。
func TestValidateWorkspaceAcceptsUnicodeAndSpaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "我的 工作区")
	got, err := validateWorkspace(p)
	if err != nil {
		t.Fatalf("中文/空格目录应当被接受: %v", err)
	}
	if !strings.HasSuffix(got, "我的 工作区") {
		t.Fatalf("路径被改坏了: %q", got)
	}
}
