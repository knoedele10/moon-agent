package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
)

const (
	runCommandMaxSteps       = 5
	runCommandDefaultTimeout = 10 * time.Minute
	runCommandMaxTimeout     = 15 * time.Minute
	runCommandMaxOutputChars = 30000
)

// runCommandTool executes a short sequence of explicit argv commands in the workspace.
// It never accepts shell syntax; each step is launched as a separate process.
type runCommandTool struct{}

type runCommandStep struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
}

func (runCommandTool) Name() string { return "run_command" }

func (runCommandTool) Description() string {
	return "在当前工作区运行开发/构建命令（例如 npm install、npm run build）。" +
		"只允许 npm、npx、go、node、python；不要调用 cmd、PowerShell、dir 等 shell 命令，查看文件和目录请使用 read/list 工具。" +
		"Go 版本查询使用 go version。命令以 program 和 args 分开传入，不支持 shell 管道、重定向或命令拼接；" +
		"最多连续运行 5 步，工作目录必须位于活动工作区内（可用 @workspaceN/子目录 选择其他目录），并受超时和输出长度限制。"
}

func (runCommandTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cwd": map[string]any{"type": "string", "description": "主工作区内的相对目录，默认 .；其他目录用 @workspace2/子目录、@workspace3/子目录选择"},
			"commands": map[string]any{
				"type": "array", "minItems": 1, "maxItems": runCommandMaxSteps,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"program": map[string]any{"type": "string", "description": "命令名，例如 npm、go、node 或 python"},
						"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"program"},
				},
			},
			"timeout_sec": map[string]any{"type": "integer", "description": "总超时秒数，默认 600，最多 900"},
		},
		"required": []string{"commands"},
	}
}

func (runCommandTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		CWD        string           `json:"cwd"`
		Commands   []runCommandStep `json:"commands"`
		TimeoutSec int              `json:"timeout_sec"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if ex == nil || strings.TrimSpace(ex.Root) == "" {
		return agent.Result{Content: "没有配置工作区，拒绝运行命令。", IsError: true}, nil
	}
	if len(a.Commands) == 0 || len(a.Commands) > runCommandMaxSteps {
		return agent.Result{Content: fmt.Sprintf("commands 必须包含 1 到 %d 步。", runCommandMaxSteps), IsError: true}, nil
	}
	workingDir, err := resolveCommandWorkingDirForRoots(ex.Root, ex.Roots, a.CWD)
	if err != nil {
		return agent.Result{Content: "工作目录无效：" + err.Error(), IsError: true}, nil
	}
	for i, step := range a.Commands {
		if _, err := resolveAllowedProgram(step.Program); err != nil {
			return agent.Result{Content: fmt.Sprintf("第 %d 步命令无效：%v", i+1, err), IsError: true}, nil
		}
		for _, arg := range step.Args {
			if containsCommandShellSyntax(arg) {
				return agent.Result{Content: fmt.Sprintf("第 %d 步参数包含 shell 控制字符；请把每个命令和参数分开传入。", i+1), IsError: true}, nil
			}
		}
	}

	timeout := runCommandDefaultTimeout
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec) * time.Second
	}
	if timeout > runCommandMaxTimeout {
		timeout = runCommandMaxTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output strings.Builder
	for i, step := range a.Commands {
		if runCtx.Err() != nil {
			return commandFailure(output.String(), fmt.Sprintf("总超时（>%s）已终止后续命令。", timeout), true)
		}
		program, _ := resolveAllowedProgram(step.Program)
		fmt.Fprintf(&output, "$ %s %s\n", step.Program, strings.Join(step.Args, " "))
		cmd := commandProcess(runCtx, program, step.Args)
		cmd.Dir = workingDir
		cmd.Env = minimalCommandEnv()
		stepOutput := &boundedCommandBuffer{limit: runCommandMaxOutputChars}
		cmd.Stdout = stepOutput
		cmd.Stderr = stepOutput
		err := cmd.Run()
		if stepOutput.buf.Len() > 0 {
			output.Write(stepOutput.buf.Bytes())
			if !strings.HasSuffix(stepOutput.buf.String(), "\n") {
				output.WriteByte('\n')
			}
		}
		if stepOutput.truncated {
			output.WriteString("\n[本步骤输出已截断]\n")
		}
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				return commandFailure(output.String(), fmt.Sprintf("第 %d 步超时（>%s），进程已终止。", i+1, timeout), true)
			}
			return commandFailure(output.String(), fmt.Sprintf("第 %d 步失败：%v；后续命令未执行。", i+1, err), false)
		}
	}

	content, truncated := clipCommandOutput(output.String())
	if strings.TrimSpace(content) == "" {
		content = "命令已成功完成，没有输出。"
	}
	if truncated {
		content += "\n……输出过长，已截断。"
	}
	return agent.Result{
		Content: content,
		Display: map[string]any{"steps": len(a.Commands), "cwd": workingDir, "truncated": truncated},
	}, nil
}

func resolveCommandWorkingDirForRoots(root string, roots []string, cwd string) (string, error) {
	if filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd 只能是工作区内的相对路径")
	}
	if strings.TrimSpace(cwd) == "" {
		cwd = "."
	}
	resolved, _, err := agentfs.ResolveWorkspaces(root, roots, cwd)
	if err != nil {
		return "", fmt.Errorf("目录不存在或不可访问：%w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd 必须是存在的目录")
	}
	return resolved, nil
}

var allowedCommandPrograms = map[string]bool{
	"npm": true, "npm.cmd": true, "npx": true, "npx.cmd": true,
	"go": true, "node": true, "python": true, "python3": true, "py": true,
}

func resolveAllowedProgram(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || !allowedCommandPrograms[strings.ToLower(name)] {
		return "", fmt.Errorf("只允许调用 npm、npx、go、node 和 python 命令")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("找不到命令 %q：%w", name, err)
	}
	if containsCommandShellSyntax(path) {
		return "", fmt.Errorf("命令路径包含 shell 控制字符，拒绝运行")
	}
	return path, nil
}

func containsCommandShellSyntax(s string) bool {
	for _, r := range s {
		switch r {
		case '&', '|', '<', '>', '^', '%', '!', '"', '\'', '\n', '\r':
			return true
		}
	}
	return false
}

func commandProcess(ctx context.Context, program string, args []string) *exec.Cmd {
	if runtime.GOOS != "windows" || (!strings.EqualFold(filepath.Ext(program), ".cmd") && !strings.EqualFold(filepath.Ext(program), ".bat")) {
		return exec.CommandContext(ctx, program, args...)
	}
	// cmd/bat shims (including npm.cmd) need cmd.exe on Windows. All shell control
	// characters are rejected above, and each argument is quoted before composition.
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	parts := []string{quoteCmdArg(program)}
	for _, arg := range args {
		parts = append(parts, quoteCmdArg(arg))
	}
	line := `"` + strings.Join(parts, " ") + `"`
	return exec.CommandContext(ctx, comspec, "/d", "/s", "/c", line)
}

func quoteCmdArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

func minimalCommandEnv() []string {
	keep := []string{"PATH", "PATHEXT", "SystemRoot", "SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "LANG", "LC_ALL", "GOPATH", "GOROOT"}
	env := make([]string, 0, len(keep))
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func commandFailure(output, reason string, timeout bool) (agent.Result, error) {
	content, truncated := clipCommandOutput(output)
	if strings.TrimSpace(content) == "" {
		content = "(命令没有输出)"
	}
	content = reason + "\n\n输出：\n" + content
	if truncated {
		content += "\n……输出过长，已截断。"
	}
	return agent.Result{Content: content, Display: map[string]any{"error": reason, "timeout": timeout, "truncated": truncated}, IsError: true}, nil
}

func clipCommandOutput(s string) (string, bool) {
	runes := []rune(s)
	if len(runes) <= runCommandMaxOutputChars {
		return s, false
	}
	return string(runes[:runCommandMaxOutputChars]), true
}

type boundedCommandBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	if b.limit <= b.buf.Len() {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func runCommandTools() []agent.Tool { return []agent.Tool{runCommandTool{}} }
