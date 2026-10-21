package mcps

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stdioTransport 把一个子进程的 stdin/stdout 当成 JSON-RPC 通道。
//
// stderr 单独收集：MCP 服务器大多把日志写 stderr，混进 stdout 会让 JSON 解析失败；
// 出问题时也能把最后几行 stderr 带进错误信息，省掉「连不上但不知道为什么」。
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	closeOnce sync.Once
	closeErr  error
}

const stderrTailBytes = 8 << 10

// tailBuffer 只保留最后 N 字节的输出。
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

// String 返回当前尾部内容。
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

func (t *stdioTransport) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *stdioTransport) Write(p []byte) (int, error) { return t.stdin.Write(p) }

func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		// 先关 stdin：MCP 服务器的正常退出信号就是 EOF。
		if err := t.stdin.Close(); err != nil {
			t.closeErr = err
		}
		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil && t.closeErr == nil {
				t.closeErr = err
			}
		case <-time.After(3 * time.Second):
			// 优雅退出失败才杀进程：正常服务器收到 EOF 会自己走。
			_ = t.cmd.Process.Kill()
			<-done
		}
		_ = t.stdout.Close()
	})
	return t.closeErr
}

// StartStdio 按配置启动一个 MCP 服务器进程并完成握手，返回可用客户端。
//
// 这是唯一会「启动外部程序」的入口，调用方（web 层）负责开关与白名单：
// 没有显式配置就不会有进程。
func StartStdio(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("mcps: 服务器 %q 缺少 command", cfg.Name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcps: %s 创建 stdin 失败：%w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcps: %s 创建 stdout 失败：%w", cfg.Name, err)
	}
	stderr := newTailBuffer(stderrTailBytes)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcps: %s 启动 %q 失败：%w", cfg.Name, cfg.Command, err)
	}

	t := &stdioTransport{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}
	c := NewClient(t, ClientOptions{Name: cfg.Name, InitTimeout: cfg.InitTimeout(), CallTimeout: cfg.CallTimeout()})
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		if tail := stderr.String(); tail != "" {
			return nil, fmt.Errorf("%w（stderr 尾部：%s）", err, clip(tail, 400))
		}
		return nil, err
	}
	return c, nil
}

// clip 把字符串截到 n 个字符（按 rune，避免切坏中文）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
