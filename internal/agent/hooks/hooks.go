// Package hooks 加载工作区里的 .moon/hooks.json，在工具前后和轮次结束时执行。
//
// 没有这个文件时所有函数都是空操作，不改变现有循环。
// PreToolUse 退出码非 0 会拒绝这次工具调用；PostToolUse / Stop 只记结果，不回滚。
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const fileName = "hooks.json"

type Hook struct {
	Phase   string   `json:"phase"`
	Command []string `json:"command"`
	Tools   []string `json:"tools,omitempty"`
}

type File struct {
	Hooks []Hook `json:"hooks"`
}

type Decision struct {
	Allow  bool
	Reason string
}

func Load(root string) (File, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return File{}, nil
	}
	path := filepath.Join(root, ".moon", fileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

func PreToolUse(ctx context.Context, root, tool, args string) Decision {
	return run(ctx, root, "PreToolUse", tool, args, true)
}

func PostToolUse(ctx context.Context, root, tool string, failed bool, output string) {
	payload, _ := json.Marshal(map[string]any{"ok": !failed, "output": clip(output, 1000)})
	_ = run(ctx, root, "PostToolUse", tool, string(payload), false)
}

func Stop(ctx context.Context, root string) {
	_ = run(ctx, root, "Stop", "", "", false)
}

func run(ctx context.Context, root, phase, tool, payload string, denyOnFail bool) Decision {
	file, err := Load(root)
	if err != nil {
		if denyOnFail {
			return Decision{Allow: false, Reason: "读取 hooks 失败: " + err.Error()}
		}
		return Decision{Allow: true}
	}
	for _, hook := range file.Hooks {
		if !strings.EqualFold(hook.Phase, phase) {
			continue
		}
		if len(hook.Tools) > 0 && tool != "" && !contains(hook.Tools, tool) {
			continue
		}
		if len(hook.Command) == 0 {
			continue
		}
		reason, err := execHook(ctx, root, hook.Command, phase, tool, payload)
		if err != nil {
			if denyOnFail {
				if reason == "" {
					reason = err.Error()
				}
				return Decision{Allow: false, Reason: "PreToolUse 拒绝了 " + tool + ": " + reason}
			}
		}
	}
	return Decision{Allow: true}
}

func execHook(ctx context.Context, root string, argv []string, phase, tool, payload string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = root
	body, _ := json.Marshal(map[string]string{"phase": phase, "tool": tool, "payload": payload})
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	reason := strings.TrimSpace(stderr.String())
	if reason == "" {
		reason = strings.TrimSpace(stdout.String())
	}
	return clip(reason, 500), err
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
