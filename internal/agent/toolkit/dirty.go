package toolkit

import (
	"context"
	"os"
	"strings"
)

// DirtyWorkspaceNote 在工作区是脏 git 仓库时返回一句提醒。
// 不是仓库、目录不存在或工作区干净时返回空串，调用方就不要改提示词。
func DirtyWorkspaceNote(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ""
	}
	out, err := runGit(context.Background(), root, "status", "--porcelain")
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 8 {
		lines = append(lines[:8], "...")
	}
	return "【未提交改动】工作区不是干净的 git 仓库。大改之前先用 git_status 看一眼，必要时让用户确认或先建检查点。\n" + strings.Join(lines, "\n") + "\n"
}
