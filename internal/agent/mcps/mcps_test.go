package mcps

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer 是一个最小的 MCP 服务器：只实现 initialize / tools/list / tools/call。
// 用 net.Pipe 而不是起子进程：单测不依赖 node/python，跑得快也没有僵尸进程。
type fakeServer struct {
	conn net.Conn
	mu   sync.Mutex
	seen []string
}

func (f *fakeServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func newFakePair(t *testing.T) (*Client, *fakeServer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	srv := &fakeServer{conn: serverConn}
	go srv.serve()
	c := NewClient(clientConn, ClientOptions{Name: "fake", InitTimeout: 2 * time.Second, CallTimeout: 2 * time.Second})
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	return c, srv
}

func (f *fakeServer) serve() {
	sc := bufio.NewScanner(f.conn)
	for sc.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		f.mu.Lock()
		f.seen = append(f.seen, req.Method)
		f.mu.Unlock()
		if req.ID == 0 {
			continue // 通知（notifications/initialized）不需要回
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake", "version": "9.9"},
				"capabilities":    map[string]any{},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "回显输入",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"},
					},
				},
				{
					"name":        "boom",
					"description": "总是失败",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
				{
					"name":        "pic",
					"description": "返回一张图",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "echo":
				result = map[string]any{"content": []map[string]any{
					{"type": "text", "text": "echo: " + strOf(p.Arguments["text"])},
				}}
			case "boom":
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": "炸了"}},
					"isError": true,
				}
			case "pic":
				result = map[string]any{"content": []map[string]any{
					{"type": "image", "data": base64.StdEncoding.EncodeToString([]byte("png-bytes")), "mimeType": "image/png"},
				}}
			}
		}
		if result == nil {
			writeLine(f.conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "未知方法"}})
			continue
		}
		writeLine(f.conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func writeLine(c net.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = c.Write(append(b, '\n'))
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func TestClientInitializeAndListTools(t *testing.T) {
	ctx := context.Background()
	c, srv := newFakePair(t)
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("握手失败：%v", err)
	}
	if got := c.ServerInfo(); got.Name != "fake" || got.Version != "9.9" {
		t.Fatalf("serverInfo = %+v", got)
	}
	specs, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("列工具失败：%v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("工具数 = %d，想要 3", len(specs))
	}
	// initialized 通知必须真的发出去，否则很多服务器会拒绝后续请求。
	var hasInitNotify bool
	for _, m := range srv.methods() {
		if m == "notifications/initialized" {
			hasInitNotify = true
		}
	}
	if !hasInitNotify {
		t.Fatal("没有发送 notifications/initialized")
	}
}

func TestToolAdapterExecuteAndError(t *testing.T) {
	ctx := context.Background()
	c, _ := newFakePair(t)
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("握手失败：%v", err)
	}
	specs, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("列工具失败：%v", err)
	}
	byName := map[string]ToolSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	echo := WrapTool(c, "demo", byName["echo"])
	if echo.Name() != "mcp__demo__echo" {
		t.Fatalf("工具名 = %q", echo.Name())
	}
	if !strings.Contains(echo.Description(), "[MCP:demo]") {
		t.Fatalf("描述缺少 MCP 前缀：%q", echo.Description())
	}
	params := echo.Parameters()
	if params["type"] != "object" {
		t.Fatalf("schema 不是 object：%v", params)
	}
	res, err := echo.Execute(ctx, json.RawMessage(`{"text":"你好"}`), nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "echo: 你好") {
		t.Fatalf("结果不对：%+v", res)
	}

	boom := WrapTool(c, "demo", byName["boom"])
	res, err = boom.Execute(ctx, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "炸了") {
		t.Fatalf("isError 没有透传：%+v", res)
	}

	pic := WrapTool(c, "demo", byName["pic"])
	res, err = pic.Execute(ctx, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if res.Image == nil || string(res.Image.Data) != "png-bytes" || res.Image.MIME != "image/png" {
		t.Fatalf("图片没有解出来：%+v", res.Image)
	}
}

func TestToolNameSanitization(t *testing.T) {
	if got := ToolName("demo", "echo"); got != "mcp__demo__echo" {
		t.Fatalf("ToolName = %q", got)
	}
	if got := ToolName("my server", "read.file"); got != "mcp__my_server__read_file" {
		t.Fatalf("非法字符没有归一化：%q", got)
	}
	if got := ToolName("", ""); got != "mcp__tool__tool" {
		t.Fatalf("空名兜底错误：%q", got)
	}
	if got := ToolName("服务器一", "echo"); got != "mcp__tool__echo" {
		t.Fatalf("非 ASCII 名字应兜底成 tool：%q", got)
	}
}

func TestParseConfig(t *testing.T) {
	ok, err := ParseConfig([]byte(`{"servers":[
		{"name":"fs","command":"npx","args":["-y","server-fs"]},
		{"name":"off","command":"x","disabled":true},
		{"name":"slow","command":"uvx","callTimeoutSeconds":5}
	]}`))
	if err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
	if len(ok.Servers) != 3 {
		t.Fatalf("服务器数 = %d", len(ok.Servers))
	}
	if ok.Servers[1].Enabled() {
		t.Fatal("disabled 的被当成启用")
	}
	if got := ok.Servers[2].CallTimeout(); got != 5*time.Second {
		t.Fatalf("超时 = %v", got)
	}

	bad := []string{
		`{"servers":[{"command":"x"}]}`,
		`{"servers":[{"name":"a","command":"x"},{"name":"a","command":"y"}]}`,
		`{"servers":[{"name":"a"}]}`,
	}
	for _, s := range bad {
		if _, err := ParseConfig([]byte(s)); err == nil {
			t.Fatalf("坏配置没有报错：%s", s)
		}
	}
}

func TestManagerConnectAllWithFakeStarter(t *testing.T) {
	ctx := context.Background()
	m := NewManager()
	m.SetStarter(func(ctx context.Context, cfg ServerConfig) (*Client, error) {
		c, _ := newFakePair(t)
		if err := c.Initialize(ctx); err != nil {
			return nil, err
		}
		return c, nil
	})
	status := m.ConnectAll(ctx, []ServerConfig{
		{Name: "demo", Command: "fake"},
		{Name: "dead", Command: "nope", Disabled: true},
	})
	if len(status) != 2 || !status[0].Connected || status[1].Error == "" {
		t.Fatalf("状态不对：%+v", status)
	}
	tools := m.Tools()
	if len(tools) != 3 {
		t.Fatalf("工具数 = %d", len(tools))
	}
	// 工具名必须稳定排序，否则提示词每轮都变、缓存全废。
	for i := 1; i < len(tools); i++ {
		if tools[i-1].Name() > tools[i].Name() {
			t.Fatalf("工具没有排序：%s > %s", tools[i-1].Name(), tools[i].Name())
		}
	}
	m.Close()
	if len(m.Tools()) != 0 {
		t.Fatal("Close 之后还有工具")
	}
}

// 超时必须真的返回，而不是把调用方挂死。
func TestCallTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	// 服务器只读不回：模拟卡死的 MCP 服务器。
	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			// 故意不回
		}
	}()
	c := NewClient(clientConn, ClientOptions{Name: "silent", CallTimeout: 100 * time.Millisecond})
	_, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("期望超时错误，实际：%v", err)
	}
}
