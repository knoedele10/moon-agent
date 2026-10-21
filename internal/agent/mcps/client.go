// Package mcps 是 Moon Agent 的 MCP（Model Context Protocol）客户端。
//
// 对标 hermes 的 tools/mcp_tool_*.py 与 tools/connectors/ 网关：把外部 MCP 服务器
// 暴露的工具当成**普通 agent.Tool** 注入工具目录，因此选择、并发、落盘、审批
// 全部复用既有机制，不需要在循环里为 MCP 开特例。
//
// 边界（契约 docs/agent/11-hermes-parity.md「MCP 式扩展」）：
//   - 本包只管「说 JSON-RPC」：握手、列工具、调工具、超时与进程生命周期。
//   - 不做安全策略：MCP 工具的风险等级、白名单与确认由 web 层决定（它们是外部代码，
//     默认按写类处理，不进入只读并发白名单）。
//   - 默认不启用：没有配置时 Manager 不启动任何进程，也就没有任何工具。
//
// 传输只实现 stdio（newline-delimited JSON-RPC 2.0）：这是 MCP 最通用的一层，
// npx/uvx/python 起的服务器都走它；HTTP 传输留出 Transport 接口，后续可加。
package mcps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// 典型超时：握手要快（进程没起来就是没起来），工具调用可以慢（MCP 工具常做真活）。
const (
	DefaultInitTimeout = 20 * time.Second
	DefaultCallTimeout = 120 * time.Second
	// maxMessageBytes 是单条 JSON-RPC 消息的上限，防止服务器发疯把内存吃光。
	maxMessageBytes = 8 << 20
)

// ErrClosed 表示客户端已经关闭，不能再发请求。
var ErrClosed = errors.New("mcps: 客户端已关闭")

// Transport 是一条双向 JSON-RPC 消息通道。stdio 是最常用的实现；
// 测试用 net.Pipe，未来加 HTTP/SSE 时也只需再实现一次。
type Transport interface {
	io.ReadWriteCloser
}

// rpcError 是 JSON-RPC 的 error 对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("mcps: JSON-RPC 错误 %d", e.Code)
	}
	return fmt.Sprintf("mcps: %s（code=%d）", e.Message, e.Code)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Method 非空表示这是一条服务器推来的通知（例如 notifications/tools/list_changed）。
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Client 是单个 MCP 服务器的连接：一个进程、一条 stdin/stdout、一张 pending 表。
type Client struct {
	name      string
	transport Transport
	initTO    time.Duration
	callTO    time.Duration

	writeGate chan struct{}
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
	nextID    int64
	pending   map[int64]chan rpcResponse
	closed    bool
	closeCh   chan struct{}
	err       error

	// onNotify 处理服务器主动通知（目前只用来记录/触发工具表刷新，可为 nil）。
	onNotify func(method string, params json.RawMessage)

	// 握手结果，供调试与状态接口展示。
	serverInfo  ServerInfo
	initialized bool
}

// ServerInfo 是 initialize 返回的 serverInfo（名字与版本）。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientOptions 是可调参数；零值用默认。
type ClientOptions struct {
	Name          string
	InitTimeout   time.Duration
	CallTimeout   time.Duration
	OnNotify      func(method string, params json.RawMessage)
	ProtocolLevel string
}

// NewClient 在给定传输上建客户端并启动读循环（不做握手，握手在 Initialize）。
func NewClient(t Transport, opts ClientOptions) *Client {
	if opts.Name == "" {
		opts.Name = "mcp"
	}
	if opts.InitTimeout <= 0 {
		opts.InitTimeout = DefaultInitTimeout
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	c := &Client{
		name:      opts.Name,
		transport: t,
		initTO:    opts.InitTimeout,
		callTO:    opts.CallTimeout,
		pending:   make(map[int64]chan rpcResponse),
		closeCh:   make(chan struct{}),
		writeGate: make(chan struct{}, 1),
		onNotify:  opts.OnNotify,
	}
	go c.readLoop()
	return c
}

// Name 返回这条连接的名字（配置文件里的 server 名）。
func (c *Client) Name() string { return c.name }

// ServerInfo 返回握手得到的服务器信息（未握手时为零值）。
func (c *Client) ServerInfo() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

// readLoop 逐行读响应并唤醒等待者；服务器退出时让所有等待者立即失败。
func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.transport)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var msg rpcResponse
		if err := json.Unmarshal(line, &msg); err != nil {
			// 一条坏消息不该杀死整条连接：记录后继续读。
			continue
		}
		if msg.Method != "" && msg.ID == 0 {
			if c.onNotify != nil {
				func() {
					defer func() { _ = recover() }()
					c.onNotify(msg.Method, msg.Params)
				}()
			}
			continue
		}
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
	}
	err := sc.Err()
	c.fail(fmt.Errorf("mcps: 连接结束：%v", err))
}

// fail 关闭所有等待者；重复调用安全。
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.mu.Unlock()
	close(c.closeCh)
	for _, ch := range pending {
		select {
		case ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

// Call 发一条请求并等响应（带超时）。
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.callWithTimeout(ctx, method, params, c.callTO)
}

func (c *Client) callWithTimeout(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	callError := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("mcps: %s 超时（%s）：%w", method, timeout, err)
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return nil, callError(err)
	}
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer c.forget(id)

	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcps: 序列化请求失败：%w", err)
	}
	payload = append(payload, '\n')
	if err := c.write(ctx, payload); err != nil {
		return nil, callError(err)
	}

	select {
	case <-ctx.Done():
		return nil, callError(ctx.Err())
	case <-c.closeCh:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return nil, err
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// write 的排队与实际写入都受请求期限约束。管道写入没有 context 接口，
// 写到一半取消时必须关闭连接，避免残留半条 JSON-RPC 或永久挂住的写协程。
func (c *Client) write(ctx context.Context, payload []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return ErrClosed
	case c.writeGate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-c.writeGate
		return err
	}
	select {
	case <-c.closeCh:
		<-c.writeGate
		return ErrClosed
	default:
	}
	written := make(chan error, 1)
	go func() {
		defer func() { <-c.writeGate }()
		n, err := c.transport.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		written <- err
	}()
	select {
	case <-ctx.Done():
		c.fail(ctx.Err())
		go func() { _ = c.Close() }()
		return ctx.Err()
	case <-c.closeCh:
		return ErrClosed
	case err := <-written:
		if err != nil {
			c.fail(err)
			go func() { _ = c.Close() }()
			return fmt.Errorf("mcps: 写入失败：%w", err)
		}
		return nil
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify 发一条通知（无 id、无响应）。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return c.write(ctx, payload)
}

// initializeParams 是 MCP 的握手参数。protocolVersion 用 2024-11-05：
// 这是绝大多数实现的默认版本，[兼容性] 比追求最新更重要。
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      map[string]any `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ServerInfo      ServerInfo `json:"serverInfo"`
	Capabilities    any        `json:"capabilities"`
}

// Initialize 走 MCP 握手：initialize → notifications/initialized。
func (c *Client) Initialize(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.initTO)
	defer cancel()
	params := initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo:      map[string]any{"name": "moon-agent", "version": "1.0"},
	}
	raw, err := c.callWithTimeout(ctx, "initialize", params, c.initTO)
	if err != nil {
		return fmt.Errorf("mcps: %s 握手失败：%w", c.name, err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("mcps: %s 握手响应无法解析：%w", c.name, err)
	}
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("mcps: %s 发送 initialized 通知失败：%w", c.name, err)
	}
	c.mu.Lock()
	c.serverInfo = res.ServerInfo
	c.initialized = true
	c.mu.Unlock()
	return nil
}

// ToolSpec 是 tools/list 返回的一个工具定义。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListTools 列工具。MCP 支持 nextCursor 分页，这里跟随直到取完。
func (c *Client) ListTools(ctx context.Context) ([]ToolSpec, error) {
	var out []ToolSpec
	cursor := ""
	for i := 0; i < 20; i++ { // 防御：分页最多 20 页
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("mcps: %s 列工具失败：%w", c.name, err)
		}
		var page struct {
			Tools      []ToolSpec `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("mcps: %s 工具列表无法解析：%w", c.name, err)
		}
		out = append(out, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ContentBlock 是 tools/call 结果里的一段内容。
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Mime     string `json:"mime,omitempty"`
}

// CallToolResult 是 tools/call 的结果。
type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// CallTool 调一个工具。arguments 是原始 JSON 对象（可以为 nil）。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*CallToolResult, error) {
	var args any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return nil, fmt.Errorf("mcps: 参数不是合法 JSON 对象：%w", err)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcps: %s 的返回无法解析：%w", c.name, err)
	}
	return &res, nil
}

// Close 关闭连接（关传输 → 读循环自然结束）。幂等。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		c.closeErr = c.transport.Close()
	})
	return c.closeErr
}
