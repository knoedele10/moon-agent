package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
)

// 技能脚本执行：让模型能跑“技能自带”的脚本（比如 ppt-master 的 301 个 Python 脚本）。
//
// 这是本应用里**唯一**能执行外部程序的地方，所以边界收得很紧：
//   - 只能跑技能目录（AGENT_SKILLS_DIR）里的 .py 文件，路径经沙箱解析，越界直接拒；
//   - 不经过 shell：参数以数组形式传给解释器，注入不了管道/重定向/&&；
//   - 工作目录固定为当前工作区，脚本产出的文件自然落在工作区里；
//   - 环境变量清洗过：只保留运行必需的最小集合 + SKILL_DIR/AGENT_WORKSPACE，
//     不把 API Key 之类的机密顺手交给技能脚本；
//   - 有超时与输出上限，跑飞了不会拖死对话。
//
// 它在「风险操作先问」档位下会被确认壳拦住——执行代码属于风险操作。

func agentSkillsDir() string {
	return strings.TrimSpace(os.Getenv("AGENT_SKILLS_DIR"))
}

const (
	skillRunDefaultTimeout = 120 * time.Second
	skillRunMaxTimeout     = 600 * time.Second
	skillRunMaxOutputChars = 20000
)

type skillRunTool struct{ userID string }

func (t skillRunTool) Name() string { return "skill_run" }

func (t skillRunTool) Description() string {
	return "运行技能目录里的 Python 脚本（例如 ppt-master 的生成脚本）。" +
		"只能执行技能目录内的 .py 文件，工作目录就是你当前的工作区，有超时与输出上限。" +
		"script 用相对技能目录的路径，例如 ppt-master/scripts/xxx.py。"
}

func (t skillRunTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"script":      map[string]any{"type": "string", "description": "相对技能目录的脚本路径，必须以 .py 结尾"},
			"args":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "传给脚本的参数"},
			"timeout_sec": map[string]any{"type": "integer", "description": "超时秒数（默认 120，最多 600）"},
		},
		"required": []string{"script"},
	}
}

func (t skillRunTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Script     string   `json:"script"`
		Args       []string `json:"args"`
		TimeoutSec int      `json:"timeout_sec"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	skillsDir := agentSkillsDir()
	if skillsDir == "" {
		return agent.Result{}, fmt.Errorf("技能目录未配置（AGENT_SKILLS_DIR 为空），无法运行技能脚本")
	}
	script := strings.TrimSpace(a.Script)
	if script == "" {
		return agent.Result{}, fmt.Errorf("script 不能为空")
	}
	if !strings.EqualFold(filepath.Ext(script), ".py") {
		return agent.Result{}, fmt.Errorf("只能运行技能目录里的 .py 脚本（收到 %q）", script)
	}

	// 沙箱解析：..、根外绝对路径、软链接逃逸都会被拒。
	full, err := agentfs.Resolve(skillsDir, script)
	if err != nil {
		return agent.Result{}, fmt.Errorf("脚本路径不合法：%v", err)
	}
	fi, err := os.Stat(full)
	if err != nil {
		return agent.Result{}, fmt.Errorf("脚本不存在：%s", script)
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, fmt.Errorf("目标不是普通文件：%s", script)
	}

	py, preArgs, err := findPython()
	if err != nil {
		return agent.Result{}, err
	}

	timeout := skillRunDefaultTimeout
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec) * time.Second
	}
	if timeout > skillRunMaxTimeout {
		timeout = skillRunMaxTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := append(append([]string{}, preArgs...), full)
	argv = append(argv, a.Args...)
	cmd := exec.CommandContext(runCtx, py, argv...)
	// 工作目录锁定在工作区：技能脚本按相对路径写文件时，落在模型能访问的地方。
	if ex != nil && strings.TrimSpace(ex.Root) != "" {
		cmd.Dir = ex.Root
	}
	cmd.Env = minimalEnv(skillsDir, ex)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	out := buf.String()
	truncated := false
	if len([]rune(out)) > skillRunMaxOutputChars {
		out = string([]rune(out)[:skillRunMaxOutputChars])
		truncated = true
	}
	if strings.TrimSpace(out) == "" {
		out = "(脚本没有输出)"
	}
	if runErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return agent.Result{
				Content: fmt.Sprintf("脚本超时（>%s）已被终止。\n\n输出：\n%s", timeout, out),
				Display: map[string]any{"script": script, "timeout": true},
				IsError: true,
			}, nil
		}
		return agent.Result{
			Content: fmt.Sprintf("脚本执行失败：%v\n\n输出：\n%s", runErr, out),
			Display: map[string]any{"script": script, "error": runErr.Error()},
			IsError: true,
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "已执行技能脚本 %s\n\n输出：\n%s", script, out)
	if truncated {
		b.WriteString("\n\n...（输出过长，已截断）")
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{"script": script, "truncated": truncated},
	}, nil
}

// findPython 找**真正能跑**的 Python 解释器。
//
// 教训（2026-10-12 实测）：Windows 上 `which python3` 找到的可能是
// *Python 安装管理器的 shim*——它会联网尝试自动安装另一个版本，然后因权限失败，
// 表现为“命令在，但完全跑不了”（还会白白耗掉二十秒）。所以不能只看 LookPath，
// 必须真拿它执行一次。候选按可靠性排序，第一个能打印 ok 的胜出；结果缓存。
var (
	pythonOnce sync.Once
	pythonPath string
	pythonPre  []string
	pythonErr  error
)

// pythonProbeTimeout 刻意很短：坏候选要快速排除，不能让一次探测就耗掉二十秒。
const pythonProbeTimeout = 5 * time.Second

func findPython() (string, []string, error) {
	pythonOnce.Do(func() {
		// 候选顺序按“真解释器优先”排：Windows 上 python3 常常是 Microsoft Store 的
		// 占位别名（WindowsApps 目录），而 python 指向实际安装的 Python。
		candidates := [][]string{{"python"}, {"python3"}, {"py", "-3"}}
		var (
			tried        []string
			fallbackPath string
			fallbackPre  []string
		)
		for _, c := range candidates {
			path, err := exec.LookPath(c[0])
			if err != nil {
				continue
			}
			pre := c[1:]
			probeCtx, cancel := context.WithTimeout(context.Background(), pythonProbeTimeout)
			args := append(append([]string{}, pre...), "-c", "print('ok')")
			cmd := exec.CommandContext(probeCtx, path, args...)
			out, perr := cmd.CombinedOutput()
			cancel()
			ok := perr == nil && strings.Contains(string(out), "ok")
			if !ok {
				tried = append(tried, c[0])
				continue
			}
			// WindowsApps 下的可执行文件是应用商店别名：能过探测也可能在真跑时拉安装器，
			// 所以只当备选，先继续找真正的安装。
			if strings.Contains(strings.ToLower(path), "\\windowsapps\\") {
				if fallbackPath == "" {
					fallbackPath, fallbackPre = path, pre
				}
				continue
			}
			pythonPath, pythonPre = path, pre
			return
		}
		if fallbackPath != "" {
			pythonPath, pythonPre = fallbackPath, fallbackPre
			return
		}
		if len(tried) == 0 {
			pythonErr = fmt.Errorf("本机找不到 python3 / python，无法运行技能脚本")
		} else {
			pythonErr = fmt.Errorf("找到了 %s，但它们都无法正常执行（可能是 Python 安装管理器的占位命令）——请装一个可用的 Python，并确认 python -c \"print(1)\" 能跑", strings.Join(tried, " / "))
		}
	})
	return pythonPath, pythonPre, pythonErr
}

// minimalEnv 只给子进程运行必需的环境变量。
//
// 刻意不继承整个 os.Environ()：里面可能有 DEEPSEEK_API_KEY 这类机密，
// 技能脚本（第三方来源）没有理由拿到它们。
func minimalEnv(skillsDir string, ex *agent.Exec) []string {
	keep := []string{"PATH", "PATHEXT", "SystemRoot", "SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "LC_ALL"}
	env := make([]string, 0, len(keep)+4)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"PYTHONIOENCODING=utf-8",
		"PYTHONUNBUFFERED=1",
		"SKILL_DIR="+skillsDir, // ppt-master 这类技能靠它定位自己的 references/scripts
	)
	if ex != nil && strings.TrimSpace(ex.Root) != "" {
		env = append(env, "AGENT_WORKSPACE="+ex.Root)
	}
	return env
}

// skillRunTools 返回技能脚本执行工具（仅非 plan 档位注册）。
func skillRunTools(userID string) []agent.Tool {
	return []agent.Tool{skillRunTool{userID: userID}}
}
