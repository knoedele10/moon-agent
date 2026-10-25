package toolkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/agent"
)

const (
	gitOutputLimit = 64 * 1024
	gitTimeout     = 20 * time.Second
)

// GitStatusTool reports the working tree without invoking a shell.
type GitStatusTool struct{}

func (GitStatusTool) Name() string { return "git_status" }
func (GitStatusTool) Description() string {
	return "查看工作区的 Git 分支与未提交改动；只读，不会修改仓库。"
}
func (GitStatusTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string", "description": "工作区内的相对目录，默认工作区根目录"}}}
}
func (GitStatusTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	cwd, err := gitCWD(raw, ex)
	if err != nil {
		return agent.Result{}, err
	}
	out, err := runGit(ctx, cwd, "status", "--short", "--branch")
	if err != nil {
		return gitFailure("git status", out, err), nil
	}
	return agent.Result{Content: out, Display: map[string]any{"cwd": cwd, "read_only": true}}, nil
}

// GitDiffTool returns either the index diff or the unstaged diff. It never
// accepts arbitrary git options, keeping the tool shell-free and predictable.
type GitDiffTool struct{}

func (GitDiffTool) Name() string { return "git_diff" }
func (GitDiffTool) Description() string {
	return "查看 Git unified diff；默认查看未暂存改动，可指定 staged 或文件路径。"
}
func (GitDiffTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"cwd": map[string]any{"type": "string"}, "staged": map[string]any{"type": "boolean"},
		"path": map[string]any{"type": "string", "description": "工作区内的文件或目录"},
	}}
}
func (GitDiffTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD    string `json:"cwd"`
		Staged bool   `json:"staged"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("git_diff 参数不是合法 JSON: %w", err)
	}
	cwd, err := gitCWDValue(a.CWD, ex)
	if err != nil {
		return agent.Result{}, err
	}
	args := []string{"diff", "--no-ext-diff", "--no-color"}
	if a.Staged {
		args = append(args, "--cached")
	}
	if strings.TrimSpace(a.Path) != "" {
		rel, err := gitRelativePath(cwd, ex.Root, a.Path)
		if err != nil {
			return agent.Result{}, err
		}
		args = append(args, "--", rel)
	}
	out, runErr := runGit(ctx, cwd, args...)
	if runErr != nil {
		return gitFailure("git diff", out, runErr), nil
	}
	return agent.Result{Content: emptyGitOutput(out, "没有检测到改动。"), Display: map[string]any{"staged": a.Staged, "path": a.Path, "read_only": true}}, nil
}

// GitLogTool returns a bounded recent history.
type GitLogTool struct{}

func (GitLogTool) Name() string { return "git_log" }
func (GitLogTool) Description() string {
	return "查看最近的 Git 提交记录；只读并限制返回数量。"
}
func (GitLogTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "description": "1-50，默认 10"}}}
}
func (GitLogTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD   string `json:"cwd"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("git_log 参数不是合法 JSON: %w", err)
	}
	cwd, err := gitCWDValue(a.CWD, ex)
	if err != nil {
		return agent.Result{}, err
	}
	if a.Limit <= 0 {
		a.Limit = 10
	}
	if a.Limit > 50 {
		a.Limit = 50
	}
	out, runErr := runGit(ctx, cwd, "log", "--no-decorate", "--date=iso", "--pretty=format:%h%x09%ad%x09%an%x09%s", "-n", strconv.Itoa(a.Limit))
	if runErr != nil {
		return gitFailure("git log", out, runErr), nil
	}
	return agent.Result{Content: emptyGitOutput(out, "仓库还没有提交记录。"), Display: map[string]any{"limit": a.Limit, "read_only": true}}, nil
}

// GitShowTool displays one commit or ref, optionally restricted to a path.
type GitShowTool struct{}

func (GitShowTool) Name() string { return "git_show" }
func (GitShowTool) Description() string {
	return "查看指定 Git 提交或引用的内容；只读。"
}
func (GitShowTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}, "ref": map[string]any{"type": "string", "description": "提交、分支或标签，默认 HEAD"}, "path": map[string]any{"type": "string"}}}
}
func (GitShowTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD  string `json:"cwd"`
		Ref  string `json:"ref"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("git_show 参数不是合法 JSON: %w", err)
	}
	cwd, err := gitCWDValue(a.CWD, ex)
	if err != nil {
		return agent.Result{}, err
	}
	ref := strings.TrimSpace(a.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	if strings.ContainsAny(ref, "\r\n") || strings.HasPrefix(ref, "-") {
		return agent.Result{}, errors.New("git_show ref 不能包含换行或以 - 开头")
	}
	args := []string{"show", "--no-ext-diff", "--no-color", ref}
	if strings.TrimSpace(a.Path) != "" {
		rel, err := gitRelativePath(cwd, ex.Root, a.Path)
		if err != nil {
			return agent.Result{}, err
		}
		args = append(args, "--", rel)
	}
	out, runErr := runGit(ctx, cwd, args...)
	if runErr != nil {
		return gitFailure("git show", out, runErr), nil
	}
	return agent.Result{Content: out, Display: map[string]any{"ref": ref, "path": a.Path, "read_only": true}}, nil
}

// GitAddTool stages explicitly named paths. The confirm wrapper in web is the
// authorization boundary; this tool itself never stages the whole repository.
type GitAddTool struct{}

func (GitAddTool) Name() string { return "git_add" }
func (GitAddTool) Description() string {
	return "把指定工作区文件加入 Git 暂存区；需要确认，绝不执行 push。"
}
func (GitAddTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}, "paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "工作区内一个或多个文件路径"}}, "required": []string{"paths"}}
}
func (GitAddTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD   string   `json:"cwd"`
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("git_add 参数不是合法 JSON: %w", err)
	}
	return runGitPathMutation(ctx, "git add", a.CWD, a.Paths, ex, "add")
}

// GitCommitTool creates a local commit. There is intentionally no push tool.
type GitCommitTool struct{}

func (GitCommitTool) Name() string { return "git_commit" }
func (GitCommitTool) Description() string {
	return "用指定说明创建本地 Git 提交；需要确认，不会 push 或 force-push。"
}
func (GitCommitTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}, "message": map[string]any{"type": "string", "description": "提交说明"}}, "required": []string{"message"}}
}
func (GitCommitTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD     string `json:"cwd"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("git_commit 参数不是合法 JSON: %w", err)
	}
	cwd, err := gitCWDValue(a.CWD, ex)
	if err != nil {
		return agent.Result{}, err
	}
	message := strings.TrimSpace(a.Message)
	if message == "" {
		return agent.Result{}, errors.New("git_commit message 不能为空")
	}
	if strings.ContainsAny(message, "\r\n") {
		return agent.Result{}, errors.New("git_commit message 不能包含换行")
	}
	out, runErr := runGit(ctx, cwd, "commit", "-m", message)
	if runErr != nil {
		return gitFailure("git commit", out, runErr), nil
	}
	return agent.Result{Content: out, Display: map[string]any{"message": message, "push": false}}, nil
}

func runGitPathMutation(ctx context.Context, label, rawCWD string, paths []string, ex *agent.Exec, command string) (agent.Result, error) {
	cwd, err := gitCWDValue(rawCWD, ex)
	if err != nil {
		return agent.Result{}, err
	}
	if len(paths) == 0 {
		return agent.Result{}, errors.New(label + " 至少需要一个路径")
	}
	args := []string{command}
	for _, path := range paths {
		rel, err := gitRelativePath(cwd, ex.Root, path)
		if err != nil {
			return agent.Result{}, err
		}
		args = append(args, rel)
	}
	args = append([]string{args[0]}, append([]string{"--"}, args[1:]...)...)
	out, runErr := runGit(ctx, cwd, args...)
	if runErr != nil {
		return gitFailure(label, out, runErr), nil
	}
	return agent.Result{Content: emptyGitOutput(out, "操作完成。"), Display: map[string]any{"paths": paths}}, nil
}

func gitCWD(raw json.RawMessage, ex *agent.Exec) (string, error) {
	var a struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("git_status 参数不是合法 JSON: %w", err)
	}
	return gitCWDValue(a.CWD, ex)
}

func gitCWDValue(raw string, ex *agent.Exec) (string, error) {
	if ex == nil || strings.TrimSpace(ex.Root) == "" {
		return "", errors.New("Git 工具需要工作区根目录")
	}
	root, err := filepath.Abs(ex.Root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if raw == "" {
		return root, nil
	}
	return safeWorkspacePath(root, raw)
}

func gitRelativePath(cwd, root, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("Git 路径不能为空")
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(raw)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("路径必须位于工作区内: %s", raw)
	}
	return filepath.ToSlash(rel), nil
}

func safeWorkspacePath(root, raw string) (string, error) {
	full, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(raw)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("cwd 必须位于工作区内: %s", raw)
	}
	if info, statErr := os.Stat(full); statErr == nil && !info.IsDir() {
		return "", fmt.Errorf("cwd 不是目录: %s", raw)
	}
	return full, nil
}

func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if runCtx.Err() != nil {
		return truncateGitOutput(stderr.String()), runCtx.Err()
	}
	out := strings.TrimSpace(stdout.String())
	if stderr.Len() > 0 {
		out = strings.TrimSpace(out + "\n" + stderr.String())
	}
	return truncateGitOutput(out), err
}

func gitFailure(operation, output string, err error) agent.Result {
	message := fmt.Sprintf("%s 失败: %v", operation, err)
	if strings.TrimSpace(output) != "" {
		message += "\n" + output
	}
	return agent.Result{IsError: true, Content: truncateGitOutput(message), Display: map[string]any{"operation": operation}}
}

func emptyGitOutput(out, fallback string) string {
	if strings.TrimSpace(out) == "" {
		return fallback
	}
	return out
}

// UnifiedDiff 返回工作区未暂存与已暂存的 unified diff，供审阅接口使用。
// 不是 git 仓库时返回错误，调用方决定怎么展示。
func UnifiedDiff(ctx context.Context, root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("工作区为空")
	}
	unstaged, err1 := runGit(ctx, root, "diff", "--no-ext-diff", "--no-color")
	staged, err2 := runGit(ctx, root, "diff", "--no-ext-diff", "--no-color", "--cached")
	if err1 != nil && err2 != nil {
		return "", err1
	}
	var b strings.Builder
	if strings.TrimSpace(unstaged) != "" {
		b.WriteString(unstaged)
		b.WriteByte('\n')
	}
	if strings.TrimSpace(staged) != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(staged)
	}
	if b.Len() == 0 {
		return "没有检测到改动。", nil
	}
	return b.String(), nil
}

func truncateGitOutput(s string) string {
	if len(s) <= gitOutputLimit {
		return s
	}
	return s[:gitOutputLimit] + "\n... output truncated ..."
}
