package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"blog-my/internal/agent"

	"github.com/gorilla/websocket"
)

// fakeState 记录假 Chrome 收到的命令，便于断言脚本内容。
type fakeState struct {
	methods []string
	exprs   []string
}

func fakeChrome(t *testing.T) (*httptest.Server, *fakeState) {
	t.Helper()
	state := &fakeState{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/page/1"
		_ = json.NewEncoder(w).Encode([]target{{Type: "page", URL: "http://127.0.0.1/", WebSocketDebuggerURL: wsURL}})
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			respondTo(ws, req.ID, req.Method, req.Params, state, t)
		}
	})
	srv = httptest.NewServer(mux)
	return srv, state
}

func respondTo(ws *websocket.Conn, id int64, method string, params json.RawMessage, state *fakeState, t *testing.T) {
	write := func(result any) {
		payload, _ := json.Marshal(map[string]any{"id": id, "result": result})
		if err := ws.WriteMessage(websocket.TextMessage, payload); err != nil {
			t.Logf("write: %v", err)
		}
	}
	state.methods = append(state.methods, method)
	switch method {
	case "Network.enable":
		// 先推两条网络事件，再回响应：验证客户端把事件与响应分开处理。
		ev1, _ := json.Marshal(map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{
				"requestId": "r1", "type": "Document",
				"request": map[string]any{"url": "http://127.0.0.1/", "method": "GET"},
			},
		})
		ev2, _ := json.Marshal(map[string]any{
			"method": "Network.responseReceived",
			"params": map[string]any{
				"requestId": "r1",
				"response":  map[string]any{"url": "http://127.0.0.1/", "status": 200},
			},
		})
		_ = ws.WriteMessage(websocket.TextMessage, ev1)
		_ = ws.WriteMessage(websocket.TextMessage, ev2)
		write(map[string]any{})
	case "Page.navigate":
		write(map[string]any{"frameId": "F1"})
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(params, &p)
		state.exprs = append(state.exprs, p.Expression)
		value := "ok"
		switch {
		case strings.Contains(p.Expression, "readyState"):
			value = "complete"
		case strings.Contains(p.Expression, "document.querySelectorAll"):
			value = `{"title":"Fake","url":"http://127.0.0.1/","text":"hello world","elements":[` +
				`{"index":0,"tag":"input","type":"text","placeholder":"搜索"},` +
				`{"index":1,"tag":"input","type":"password","redacted":true}]}`
		case strings.Contains(p.Expression, "document.title"):
			value = `{"title":"Fake","url":"http://127.0.0.1/"}`
		case strings.Contains(p.Expression, "el.click"):
			value = "OK button 提交"
		case strings.Contains(p.Expression, "requestSubmit"):
			value = "SUBMITTED"
		case strings.Contains(p.Expression, "el.value"):
			value = "OK"
		}
		write(map[string]any{"result": map[string]any{"type": "string", "value": value}})
	case "Page.captureScreenshot":
		write(map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("PNGDATA"))})
	default:
		write(map[string]any{})
	}
}

func useFakeChrome(t *testing.T) *fakeState {
	t.Helper()
	Close()
	srv, state := fakeChrome(t)
	t.Cleanup(func() {
		Close()
		srv.Close()
	})
	t.Setenv("AGENT_BROWSER_URL", srv.URL)
	t.Setenv("AGENT_BROWSER_ALLOW", "127.0.0.1")
	return state
}

func TestOpenSnapshotClickTypeNetwork(t *testing.T) {
	state := useFakeChrome(t)
	ctx := context.Background()
	ex := &agent.Exec{Root: t.TempDir()}

	res, err := (OpenTool{}).Execute(ctx, json.RawMessage(`{"url":"http://127.0.0.1/page"}`), ex)
	if err != nil || res.IsError {
		t.Fatalf("open: %v %+v", err, res)
	}
	if !strings.Contains(res.Content, "complete") {
		t.Fatalf("open content = %q", res.Content)
	}

	res, err = (SnapshotTool{}).Execute(ctx, json.RawMessage(`{}`), ex)
	if err != nil || res.IsError {
		t.Fatalf("snapshot: %v %+v", err, res)
	}
	if !strings.Contains(res.Content, "hello world") || !strings.Contains(res.Content, "内容已隐藏") {
		t.Fatalf("snapshot content = %q", res.Content)
	}

	res, err = (ClickTool{}).Execute(ctx, json.RawMessage(`{"index":0}`), ex)
	if err != nil || res.IsError || !strings.Contains(res.Content, "已点击") {
		t.Fatalf("click: %v %+v", err, res)
	}

	res, err = (TypeTool{}).Execute(ctx, json.RawMessage(`{"index":0,"text":"你好"}`), ex)
	if err != nil || res.IsError || !strings.Contains(res.Content, "已填写") {
		t.Fatalf("type: %v %+v", err, res)
	}

	res, err = (NetworkTool{}).Execute(ctx, json.RawMessage(`{}`), ex)
	if err != nil || res.IsError || !strings.Contains(res.Content, "127.0.0.1") {
		t.Fatalf("network: %v %+v", err, res)
	}
	if len(state.exprs) == 0 {
		t.Fatal("假 Chrome 没有收到 Runtime.evaluate")
	}
}

func TestScreenshotWritesIntoWorkspace(t *testing.T) {
	useFakeChrome(t)
	root := t.TempDir()
	ex := &agent.Exec{Root: root}
	res, err := (ScreenshotTool{}).Execute(context.Background(), json.RawMessage(`{}`), ex)
	if err != nil || res.IsError {
		t.Fatalf("screenshot: %v %+v", err, res)
	}
	path, _ := res.Display["path"].(string)
	if !strings.HasPrefix(path, root) {
		t.Fatalf("截图应落在工作区，实际 %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("截图文件不存在: %v", err)
	}
	if res.Image == nil || res.Image.MIME != "image/png" {
		t.Fatal("截图应带回图片")
	}

	bad, err := (ScreenshotTool{}).Execute(context.Background(), json.RawMessage(`{"path":"../../evil.png"}`), ex)
	if err != nil || !bad.IsError {
		t.Fatalf("越界路径应被拒绝: %+v", bad)
	}
}

func TestOpenRejectsLocalWithoutAllowList(t *testing.T) {
	Close()
	t.Setenv("AGENT_BROWSER_ALLOW", "")
	res, err := (OpenTool{}).Execute(context.Background(), json.RawMessage(`{"url":"http://127.0.0.1:8080/x"}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("本机地址默认应被拒: %+v", res)
	}
	if !strings.Contains(res.Content, "AGENT_BROWSER_ALLOW") {
		t.Fatalf("错误里应给出修复办法，实际 %q", res.Content)
	}
}

func TestCheckURLAllowList(t *testing.T) {
	t.Setenv("AGENT_BROWSER_ALLOW", "example.com")
	if _, err := checkURL("https://api.example.com/x"); err != nil {
		t.Fatalf("子域应放行: %v", err)
	}
	if _, err := checkURL("https://evil.com/"); err == nil {
		t.Fatal("白名单外应拒绝")
	}
	if _, err := checkURL("file:///C:/Windows/win.ini"); err == nil {
		t.Fatal("file:// 应拒绝")
	}
	if _, err := checkURL("https://example.com.evil.com/"); err == nil {
		t.Fatal("后缀伪装应拒绝")
	}
}

func TestElementLookupEncodesSelector(t *testing.T) {
	idx := 3
	got, err := elementLookup(&idx, `a"b`)
	if err != nil || !strings.Contains(got, `data-moon-idx="3"`) {
		t.Fatalf("index lookup = %q, %v", got, err)
	}
	got, err = elementLookup(nil, `a"b`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `a\"b`) {
		t.Fatalf("selector 应被 JSON 转义，实际 %q", got)
	}
	if _, err := elementLookup(nil, "  "); err == nil {
		t.Fatal("空 selector 应报错")
	}
}
