package toolkit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserOpenClickTypeAndHidePassword(t *testing.T) {
	old := allowPrivateHosts
	allowPrivateHosts = true
	t.Cleanup(func() { allowPrivateHosts = old })

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>搜索</title>
			<form><input name="q" type="text" value=""><input name="pw" type="password" value="secret"></form>
			<a href="/item">详情</a>`))
	})
	mux.HandleFunc("/item", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>详情</title><p>价格 12</p>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	open, err := BrowserOpenTool{}.Execute(t.Context(), []byte(`{"url":"`+srv.URL+`/search"}`), nil)
	if err != nil || open.IsError {
		t.Fatalf("open: %v %s", err, open.Content)
	}
	if strings.Contains(open.Content, "secret") {
		t.Fatalf("password leaked: %s", open.Content)
	}
	if !strings.Contains(open.Content, "type=password") || !strings.Contains(open.Content, "搜索") {
		t.Fatalf("snapshot missing fields: %s", open.Content)
	}
	id, _ := open.Display["session_id"].(string)
	typed, err := BrowserTypeTool{}.Execute(t.Context(), []byte(`{"session_id":"`+id+`","selector":"q","text":"moon"}`), nil)
	if err != nil || typed.IsError || !strings.Contains(typed.Content, "moon") {
		t.Fatalf("type: %v %s", err, typed.Content)
	}
	denied, err := BrowserTypeTool{}.Execute(t.Context(), []byte(`{"session_id":"`+id+`","selector":"pw","text":"nope"}`), nil)
	if err != nil || !denied.IsError || !strings.Contains(denied.Content, "密码") {
		t.Fatalf("password type should fail: %v %s", err, denied.Content)
	}
	clicked, err := BrowserClickTool{}.Execute(t.Context(), []byte(`{"session_id":"`+id+`","target":"详情"}`), nil)
	if err != nil || clicked.IsError || !strings.Contains(clicked.Content, "价格 12") {
		t.Fatalf("click: %v %s", err, clicked.Content)
	}
	shot, err := BrowserScreenshotTool{}.Execute(t.Context(), []byte(`{"session_id":"`+id+`"}`), nil)
	if err != nil || shot.IsError || shot.Display["screenshot"] != false {
		t.Fatalf("screenshot: %v %+v", err, shot)
	}
}
