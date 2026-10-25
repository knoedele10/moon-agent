package mcps

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPServerConfigAndProbe(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"servers":[{"name":"remote","url":"https://example.com/mcp","permission":"read"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Enabled() {
		t.Fatal("只有 url 的服务器不应作为 stdio 启动")
	}
	if cfg.Servers[0].Transport() != "http" || cfg.Servers[0].Permission != "read" {
		t.Fatalf("transport/permission = %s %s", cfg.Servers[0].Transport(), cfg.Servers[0].Permission)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	code, err := ProbeHTTP(t.Context(), srv.URL)
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("probe = %d %v", code, err)
	}
	if _, err := ProbeHTTP(t.Context(), "file:///etc/passwd"); err == nil {
		t.Fatal("非 http url 应拒绝")
	}
}
