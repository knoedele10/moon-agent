package rules

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var verifyOnce sync.Map

// ResetVerifyState 清空「这一轮已经跑过验证」的记录，测试用。
func ResetVerifyState() { verifyOnce = sync.Map{} }

var verifyAllow = map[string]bool{
	"go": true, "npm": true, "npx": true, "node": true,
	"python": true, "python3": true, "make": true,
}

// VerifyCommands 从工作区规则里取出验证命令。更近的 AGENTS.md 覆盖更远的。
func VerifyCommands(root string) ([][]string, error) {
	res, err := Load(root)
	if err != nil {
		return nil, err
	}
	var cmds [][]string
	for _, doc := range res.Documents {
		if parsed := parseVerify(doc.Content); len(parsed) > 0 {
			cmds = parsed
		}
	}
	return cmds, nil
}

func parseVerify(content string) [][]string {
	var lines []string
	if block, ok := commentBlock(content, "moon:verify"); ok {
		lines = strings.Split(block, "\n")
	}
	var cmds [][]string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		cmds = append(cmds, fields)
	}
	return cmds
}

func commentBlock(content, name string) (string, bool) {
	marker := "<!-- " + name
	start := strings.Index(content, marker)
	if start < 0 {
		marker = "<!--" + name
		start = strings.Index(content, marker)
		if start < 0 {
			return "", false
		}
	}
	rest := content[start+len(marker):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// MaybeVerify 在一次成功的 write/edit 之后，最多自动跑一轮 AGENTS.md 里的验证命令。
// 没有声明、或这一轮已经跑过时返回空串。
func MaybeVerify(ctx context.Context, root, turnID, tool string, failed bool) string {
	if failed || (tool != "write" && tool != "edit") || strings.TrimSpace(root) == "" {
		return ""
	}
	cmds, err := VerifyCommands(root)
	if err != nil || len(cmds) == 0 {
		return ""
	}
	key := root + "\x00" + turnID
	if _, loaded := verifyOnce.LoadOrStore(key, true); loaded {
		return ""
	}
	return runVerify(ctx, root, cmds)
}

func runVerify(ctx context.Context, root string, cmds [][]string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	var b strings.Builder
	b.WriteString("[verify] 按 AGENTS.md 自动验证（本轮只跑一次）：")
	for _, cmd := range cmds {
		base := strings.ToLower(filepath.Base(cmd[0]))
		if !verifyAllow[base] {
			fmt.Fprintf(&b, "\n- 跳过 %s：验证命令只允许 go/npm/npx/node/python/python3/make", strings.Join(cmd, " "))
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c := exec.CommandContext(runCtx, cmd[0], cmd[1:]...)
		c.Dir = root
		var out bytes.Buffer
		c.Stdout = &out
		c.Stderr = &out
		err := c.Run()
		cancel()
		text := strings.TrimSpace(out.String())
		if len(text) > 2000 {
			text = text[:2000] + "\n... truncated ..."
		}
		if err != nil {
			fmt.Fprintf(&b, "\n- 失败 %s：%v", strings.Join(cmd, " "), err)
			if text != "" {
				fmt.Fprintf(&b, "\n%s", text)
			}
			b.WriteString("\n请根据上面的失败修一次。不要重复运行同一条验证命令。")
			return b.String()
		}
		fmt.Fprintf(&b, "\n- 通过 %s", strings.Join(cmd, " "))
	}
	return b.String()
}
