package mcps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProbeHTTP 对声明了 url 的 MCP 做一次短 GET。成功只表示端口有响应，不建立 MCP 会话。
func ProbeHTTP(ctx context.Context, rawURL string) (int, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return 0, fmt.Errorf("mcps: url 为空")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return 0, fmt.Errorf("mcps: url 必须是 http 或 https")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}
