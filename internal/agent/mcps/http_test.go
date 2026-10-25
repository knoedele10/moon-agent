package mcps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRemoteMCP 起一个最小的 Streamable HTTP MCP 服务器：
// initialize 走 JSON，tools/list 走 SSE（先推一条通知再推响应），tools/call 走 JSON。
func fakeRemoteMCP(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var sessionSeen bool
	var initSeen bool

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求不是合法 JSON: %v", err)
			return
		}
		mu.Lock()
		if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "" {
			sessionSeen = true
		}
		if req.Method == "initialize" {
			initSeen = true
		}
		mu.Unlock()

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"remote-mcp","version":"9.9"},"capabilities":{}}}`, *req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			// 先推一条通知，验证客户端不会把它当成响应。
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"echo\",\"description\":\"回声\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", *req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"pong"}],"isError":false}}`, *req.ID)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
		mu.Lock()
		defer mu.Unlock()
		_ = initSeen
		_ = sessionSeen
	}))
}

func TestHTTPServerEndToEnd(t *testing.T) {
	srv := fakeRemoteMCP(t)
	defer srv.Close()

	cfg := ServerConfig{Name: "remote", URL: srv.URL + "/mcp"}
	c, err := StartHTTP(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	defer c.Close()

	if info := c.ServerInfo(); info.Name != "remote-mcp" || info.Version != "9.9" {
		t.Fatalf("serverInfo = %+v", info)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"ping"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "pong" || res.IsError {
		t.Fatalf("result = %+v", res)
	}
}

func TestHTTPServerRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := StartHTTP(context.Background(), ServerConfig{Name: "bad", URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewHTTPTransportValidatesURL(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com/mcp", "http://", "://nope"} {
		if _, err := newHTTPTransport(raw, ServerConfig{Name: "x"}); err == nil {
			t.Fatalf("%q 应当被拒绝", raw)
		}
	}
	if _, err := newHTTPTransport("https://example.com/mcp", ServerConfig{Name: "x"}); err != nil {
		t.Fatalf("合法 url 被拒绝: %v", err)
	}
}

func TestRequestIDHandlesNotifications(t *testing.T) {
	if _, ok := requestID([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); ok {
		t.Fatal("通知不应有 id")
	}
	id, ok := requestID([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
	if !ok || id != 7 {
		t.Fatalf("id = %d, ok = %v", id, ok)
	}
	// 慢请求：id 为 0 也要能识别（0 是合法 id）。
	id, ok = requestID([]byte(`{"jsonrpc":"2.0","id":0,"method":"ping"}`))
	if !ok || id != 0 {
		t.Fatalf("id 0 应当被识别，ok = %v", ok)
	}
}

func TestStartDispatchesByConfig(t *testing.T) {
	if _, err := Start(context.Background(), ServerConfig{Name: "none"}); err == nil {
		t.Fatal("既没有 command 也没有 url 应当报错")
	}
	// 只有 url 时选 HTTP：指向一个不存在的地址，错误应来自 HTTP 而不是「缺少 command」。
	_, err := Start(context.Background(), ServerConfig{Name: "remote", URL: "http://127.0.0.1:1/mcp", InitTimeoutSeconds: 1})
	if err == nil || strings.Contains(err.Error(), "缺少 command") {
		t.Fatalf("err = %v", err)
	}
}
