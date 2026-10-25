package mcps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTP 传输（MCP Streamable HTTP）。
//
// 与 stdio 的区别只在「消息怎么走」：stdio 是一条长连接上的行分隔 JSON，
// HTTP 是「一个请求一 POST」。Client 仍然按**行分隔**读消息，所以这里把
// 每次 POST 的响应体整理成若干行塞进内部缓冲，Read 从缓冲里取——上层读循环
// 与 stdio 完全一致，不需要为 HTTP 开特例。
//
// 覆盖的两种响应形态（MCP 规范允许服务器二选一）：
//   - application/json：整个响应体就是一条 JSON-RPC 消息；
//   - text/event-stream：SSE，每条 `data:` 是一行 JSON-RPC 消息。服务器可能先推
//     notifications/tools/list_changed 之类的通知，再推真正响应；这里全部入缓冲，
//     读到匹配本次请求 id 的响应就停止，避免被长连接的 keep-alive 拖住。
//
// 会话：服务器可以在 initialize 响应里用 Mcp-Session-Id 头建立会话，后续请求必须带上。
// 这里自动记住并回传。通知类请求（无 id）通常返回 202，不需要入缓冲。

// httpTransport 把「POST + 响应」适配成 io.ReadWriteCloser。
type httpTransport struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration
	protocol string

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	closed  bool
	err     error
	session string
}

func newHTTPTransport(endpoint string, cfg ServerConfig) (*httpTransport, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("mcps: %s 的 url 无法解析：%w", cfg.Name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mcps: %s 的 url 只支持 http/https，收到 %q", cfg.Name, endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mcps: %s 的 url 缺少主机名", cfg.Name)
	}
	timeout := cfg.CallTimeout()
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	t := &httpTransport{
		endpoint: u.String(),
		client:   &http.Client{},
		timeout:  timeout,
		protocol: "2024-11-05",
	}
	t.cond = sync.NewCond(&t.mu)
	return t, nil
}

func (t *httpTransport) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed {
		t.cond.Wait()
	}
	if len(t.buf) > 0 {
		n := copy(p, t.buf)
		t.buf = t.buf[n:]
		return n, nil
	}
	if t.err != nil {
		return 0, t.err
	}
	return 0, io.EOF
}

// Write 发一个 JSON-RPC 请求/通知，并把响应消息放进读缓冲。
// 调用方（Client.write）已经用 writeGate 串行化，这里不需要额外的并发保护。
func (t *httpTransport) Write(p []byte) (int, error) {
	body := bytes.TrimSpace(p)
	if len(body) == 0 {
		return len(p), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("mcps: 构造 HTTP 请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", t.protocol)
	t.mu.Lock()
	session := t.session
	t.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("mcps: HTTP 请求失败：%w", err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("mcps: HTTP %d：%s", resp.StatusCode, clip(strings.TrimSpace(string(snippet)), 300))
	}
	// 通知（notifications/*）通常是 202/204 + 空体，没有消息可入缓冲。
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted {
		return len(p), nil
	}

	wantID, hasID := requestID(body)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		if err := t.readSSE(resp.Body, wantID, hasID); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err != nil {
		return 0, fmt.Errorf("mcps: 读取 HTTP 响应失败：%w", err)
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		// Client 的读循环按行切消息：这里必须补一个换行，否则 Scanner 会一直
		// 等「这一行还没结束」，看起来就像握手超时。
		if raw[len(raw)-1] != '\n' {
			raw = append(raw, '\n')
		}
		t.enqueue(raw)
	}
	return len(p), nil
}

// readSSE 逐条读 `data:` 消息入缓冲。读到与本次请求 id 匹配的响应就停止：
// 有些服务器不会主动结束事件流，一直等会白等到超时。
func (t *httpTransport) readSSE(body io.Reader, wantID int64, hasID bool) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var probe struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal([]byte(data), &probe); err != nil {
			continue
		}
		t.enqueue([]byte(data + "\n"))
		if !hasID || probe.ID == nil {
			// 通知或服务器请求：继续读，等真正的响应。
			continue
		}
		if *probe.ID == wantID && probe.Method == "" {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcps: 读取 SSE 失败：%w", err)
	}
	return nil
}

func (t *httpTransport) enqueue(b []byte) {
	t.mu.Lock()
	if !t.closed {
		t.buf = append(t.buf, b...)
	}
	t.mu.Unlock()
	t.cond.Broadcast()
}

func (t *httpTransport) Close() error {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		t.err = ErrClosed
	}
	t.mu.Unlock()
	t.cond.Broadcast()
	return nil
}

// requestID 从请求行里取 id；通知（无 id）返回 hasID=false。
func requestID(body []byte) (int64, bool) {
	var probe struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.ID == nil {
		return 0, false
	}
	return *probe.ID, true
}

// StartHTTP 连接一个 Streamable HTTP 的 MCP 服务器并完成握手。
func StartHTTP(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("mcps: 服务器 %q 缺少 url", cfg.Name)
	}
	t, err := newHTTPTransport(cfg.URL, cfg)
	if err != nil {
		return nil, err
	}
	c := NewClient(t, ClientOptions{Name: cfg.Name, InitTimeout: cfg.InitTimeout(), CallTimeout: cfg.CallTimeout()})
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Start 按配置选择传输：有 command 走 stdio，只有 url 走 HTTP。
// 两者都配时以 command 为准（与配置校验的提示顺序一致）。
func Start(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Command) != "" {
		return StartStdio(ctx, cfg)
	}
	if strings.TrimSpace(cfg.URL) != "" {
		return StartHTTP(ctx, cfg)
	}
	return nil, fmt.Errorf("mcps: 服务器 %q 缺少 command 或 url", cfg.Name)
}
