// Package browser 通过 Chrome DevTools Protocol 操作**用户已经登录的浏览器**。
//
// 定位（docs/agent/14-market-comparison.md P1-7）：
//   - 静态正文 → web_reader；
//   - 需要登录、点击、填表 → 本包；
//   - 桌面应用 → CUA。
//
// 传输只用 CDP 的 WebSocket（JSON-RPC 风格），不额外引入浏览器自动化框架：
// 依赖面小，出错时返回的消息也能直接给模型看。
//
// 安全边界：
//   - 只允许 http/https；默认拒绝本机/内网地址，除非 AGENT_BROWSER_ALLOW 显式列出；
//   - 快照不读取密码框内容；填写时拒绝密码/支付/验证码字段；
//   - 点击与填写在 toolsys 里登记为需要确认的风险操作。
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// defaultEndpoint 是 Chrome 的远程调试入口。
	defaultEndpoint = "http://127.0.0.1:9222"
	// callTimeout 是单条 CDP 命令的上限。导航另算，见 open 工具。
	callTimeout = 20 * time.Second
	// maxEvent 是网络事件环形缓冲的条数。
	maxEvent = 40
)

// target 是 /json/list 返回的一个调试目标。
type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("CDP 错误 %d: %s", e.Code, e.Message)
}

type message struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// netEvent 是网络面板里的一行。
type netEvent struct {
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	URL       string `json:"url"`
	Type      string `json:"type,omitempty"`
	Status    int    `json:"status,omitempty"`
}

// conn 是一条到页面目标的 CDP 连接。
type conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex
	nextID  int64

	mu      sync.Mutex
	pending map[int64]chan message
	closed  bool
	err     error
	closeCh chan struct{}

	eventMu sync.Mutex
	events  []netEvent

	// opMu 串行化「会话级操作」：CDP 本身允许并发命令，但导航/点击/填写
	// 之间有先后语义，并发跑会得到谁也说不清的状态。
	opMu sync.Mutex
}

func endpoint() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_BROWSER_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultEndpoint
}

// dial 连接一个页面目标。优先复用已有的 page，没有再新开一个空白页。
func dial(ctx context.Context) (*conn, error) {
	base := endpoint()
	t, err := pickTarget(ctx, base)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, t.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接浏览器调试端口失败（%s）：%w；"+
			"请确认 Chrome 用 --remote-debugging-port=9222 启动，或用 AGENT_BROWSER_URL 指定地址", base, err)
	}
	c := &conn{
		ws:      ws,
		pending: make(map[int64]chan message),
		closeCh: make(chan struct{}),
	}
	go c.readLoop()
	// 打开网络监听，供 browser_network 使用；失败不影响其它工具。
	_, _ = c.call(ctx, "Network.enable", nil)
	return c, nil
}

func pickTarget(ctx context.Context, base string) (target, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	targets, err := listTargets(ctx, client, base)
	if err != nil {
		return target{}, err
	}
	for _, t := range targets {
		if t.Type == "page" && strings.TrimSpace(t.WebSocketDebuggerURL) != "" {
			return t, nil
		}
	}
	// /json/new 在新版本 Chrome 里要求 PUT。
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		req, err := http.NewRequestWithContext(ctx, method, base+"/json/new?about:blank", nil)
		if err != nil {
			return target{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var created target
		decodeErr := json.NewDecoder(resp.Body).Decode(&created)
		_ = resp.Body.Close()
		if decodeErr == nil && created.WebSocketDebuggerURL != "" {
			return created, nil
		}
	}
	return target{}, errors.New("浏览器没有可用的页面目标；请先打开一个标签页，" +
		"或确认 Chrome 以 --remote-debugging-port=9222 启动")
}

func listTargets(ctx context.Context, client *http.Client, base string) ([]target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("浏览器调试端口不可达（%s）：%w；请用 --remote-debugging-port=9222 启动 Chrome", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("浏览器调试端口返回 HTTP %d", resp.StatusCode)
	}
	var out []target
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("浏览器目标列表无法解析：%w", err)
	}
	return out, nil
}

func (c *conn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(fmt.Errorf("浏览器连接已断开：%w", err))
			return
		}
		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.mu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}
		if msg.Method != "" {
			c.recordEvent(msg)
		}
	}
}

// recordEvent 只关心网络请求，其它事件忽略。
func (c *conn) recordEvent(msg message) {
	switch msg.Method {
	case "Network.requestWillBeSent":
		var p struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
			Request   struct {
				URL    string `json:"url"`
				Method string `json:"method"`
			} `json:"request"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		c.eventMu.Lock()
		defer c.eventMu.Unlock()
		c.events = append(c.events, netEvent{RequestID: p.RequestID, Method: p.Request.Method, URL: p.Request.URL, Type: p.Type})
		if len(c.events) > maxEvent {
			c.events = c.events[len(c.events)-maxEvent:]
		}
	case "Network.responseReceived":
		var p struct {
			RequestID string `json:"requestId"`
			Response  struct {
				URL    string `json:"url"`
				Status int    `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		c.eventMu.Lock()
		defer c.eventMu.Unlock()
		for i := len(c.events) - 1; i >= 0; i-- {
			if c.events[i].RequestID == p.RequestID {
				c.events[i].Status = p.Response.Status
				if c.events[i].URL == "" {
					c.events[i].URL = p.Response.URL
				}
				return
			}
		}
	}
}

func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = make(map[int64]chan message)
	c.mu.Unlock()
	close(c.closeCh)
	for _, ch := range pending {
		select {
		case ch <- message{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

// call 发一条 CDP 命令并等响应。
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	c.nextID++
	id := c.nextID
	c.writeMu.Unlock()

	ch := make(chan message, 1)
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
		return nil, fmt.Errorf("发送 CDP 命令失败：%w", err)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("CDP 命令 %s 超时：%w", method, ctx.Err())
	case <-c.closeCh:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = errors.New("浏览器连接已关闭")
		}
		return nil, err
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// evaluate 在页面里跑一段表达式，返回字符串结果（按值返回）。
func (c *conn) evaluate(ctx context.Context, expr string) (string, error) {
	raw, err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("页面脚本返回无法解析：%w", err)
	}
	if res.ExceptionDetails != nil {
		return "", fmt.Errorf("页面脚本报错：%s", res.ExceptionDetails.Text)
	}
	if len(res.Result.Value) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(res.Result.Value, &s); err == nil {
		return s, nil
	}
	return string(res.Result.Value), nil
}

func (c *conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = errors.New("浏览器会话已关闭")
	close(c.closeCh)
	c.mu.Unlock()
	_ = c.ws.Close()
}

// ---- 共享会话：一个进程一份，避免每次工具调用都重连 ----

var (
	sessMu sync.Mutex
	sess   *conn
)

// shared 返回可用的共享连接；连不上时返回可自纠的错误说明。
func shared(ctx context.Context) (*conn, error) {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sess != nil {
		sess.mu.Lock()
		dead := sess.closed
		sess.mu.Unlock()
		if !dead {
			return sess, nil
		}
	}
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	sess = c
	return c, nil
}

// Close 结束共享会话（应用退出时调用）。
func Close() {
	sessMu.Lock()
	c := sess
	sess = nil
	sessMu.Unlock()
	if c != nil {
		c.close()
	}
}

// ---- 域名与路径校验 ----

func allowList() []string {
	raw := strings.TrimSpace(os.Getenv("AGENT_BROWSER_ALLOW"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkURL 只放行 http/https；没有 allowlist 时拒绝本机与内网。
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("需要完整的 http/https 网址，收到 %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	list := allowList()
	if len(list) == 0 {
		if isLocalHost(host) {
			return nil, fmt.Errorf("默认拒绝本机/内网地址（%s）；确实要访问本地页面时，"+
				"设置 AGENT_BROWSER_ALLOW=%s 后重试", host, host)
		}
		return u, nil
	}
	if !hostAllowed(host, list) {
		return nil, fmt.Errorf("域名 %s 不在 AGENT_BROWSER_ALLOW 白名单里；"+
			"把需要的域名加进去再试（当前：%s）", host, strings.Join(list, ","))
	}
	return u, nil
}

func hostAllowed(host string, list []string) bool {
	for _, entry := range list {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

func isLocalHost(host string) bool {
	if host == "localhost" || host == "::1" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := parseIPv4(host)
	if ip == nil {
		return false
	}
	switch {
	case ip[0] == 10, ip[0] == 127:
		return true
	case ip[0] == 169 && ip[1] == 254:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	}
	return false
}

func parseIPv4(host string) []byte {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return nil
	}
	out := make([]byte, 4)
	for i, p := range parts {
		n := 0
		if p == "" || len(p) > 3 {
			return nil
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil
			}
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return nil
		}
		out[i] = byte(n)
	}
	return out
}
