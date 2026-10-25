package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/agent/mcps"
	"blog-my/internal/agent/toolkit/browser"

	"github.com/gin-gonic/gin"
)

// MCP 接线：把外部 MCP 服务器暴露的工具接进工具目录。
//
// 开关就是配置文件本身——没配 AGENT_MCP_CONFIG / AGENT_MCP_JSON 就没有任何 MCP 工具，
// 不起进程、不联网、不改行为（与契约「默认关闭」一致）。
//
// 生命周期：一个进程一份 Manager，首次用到时连一次，之后复用。
// 不按对话起进程：npx 起服务器是秒级开销，每轮对话都起会把体验拖垮。

var (
	mcpOnce    sync.Once
	mcpManager *mcps.Manager
	mcpErr     error
	mcpMu      sync.RWMutex
	mcpClosed  bool
)

// mcpConfigSource 返回配置来源：优先 inline JSON，其次文件路径。
func mcpConfigSource() (inline []byte, path string, ok bool) {
	if raw := strings.TrimSpace(os.Getenv("AGENT_MCP_JSON")); raw != "" {
		return []byte(raw), "", true
	}
	if p := strings.TrimSpace(os.Getenv("AGENT_MCP_CONFIG")); p != "" {
		return nil, p, true
	}
	return nil, "", false
}

// mcpManagerInstance 惰性构建（并连接）全局 Manager。失败只记一次，不阻塞对话。
func mcpManagerInstance() (*mcps.Manager, error) {
	mcpOnce.Do(func() {
		inline, path, ok := mcpConfigSource()
		if !ok {
			return
		}
		var cfg *mcps.Config
		var err error
		if inline != nil {
			cfg, err = mcps.ParseConfig(inline)
		} else {
			cfg, err = mcps.LoadConfigFile(path)
		}
		if err != nil {
			mcpMu.Lock()
			mcpErr = err
			mcpMu.Unlock()
			return
		}
		if cfg == nil || len(cfg.Servers) == 0 {
			return
		}
		m := mcps.NewManager()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		m.ConnectAll(ctx, cfg.Servers)
		mcpMu.Lock()
		if mcpClosed {
			mcpMu.Unlock()
			m.Close()
			return
		}
		mcpManager = m
		mcpMu.Unlock()
	})
	mcpMu.RLock()
	defer mcpMu.RUnlock()
	return mcpManager, mcpErr
}

// CloseAgentRuntime 由应用退出路径调用；关闭外部工具进程，不让它们遗留在后台。
// 与首次连接交错时，刚完成连接的 Manager 也会立即关闭。
func CloseAgentRuntime() {
	mcpMu.Lock()
	mcpClosed = true
	m := mcpManager
	mcpMu.Unlock()
	if m != nil {
		m.Close()
	}
	// 浏览器会话是进程级单例，退出时一起关掉，别留下半开的 WebSocket。
	browser.Close()
}

func currentMCPManager() *mcps.Manager {
	mcpMu.RLock()
	defer mcpMu.RUnlock()
	return mcpManager
}

// mcpTools 返回本次运行可用的 MCP 工具；没配置或全失败时返回 nil。
func mcpTools() []agent.Tool {
	m, err := mcpManagerInstance()
	if err != nil || m == nil {
		return nil
	}
	return m.Tools()
}

// mcpStatus 返回 MCP 连接状态，供 /api/agent/evolution 展示。
// 注意：这里**不**触发连接——状态接口不该有副作用。
func mcpStatus() []mcps.ServerStatus {
	m := currentMCPManager()
	if m == nil {
		return nil
	}
	return m.Status()
}

// getMCPHealth 列出已配置的 MCP，并对带 url 的条目做一次 HTTP 探测。
// 不启动 stdio 进程。完整的 Streamable HTTP 会话不在这里建立。
func getMCPHealth(c *gin.Context) {
	inline, path, ok := mcpConfigSource()
	var cfg *mcps.Config
	var cfgErr error
	if ok {
		if inline != nil {
			cfg, cfgErr = mcps.ParseConfig(inline)
		} else {
			cfg, cfgErr = mcps.LoadConfigFile(path)
		}
	}
	connected := map[string]mcps.ServerStatus{}
	for _, st := range mcpStatus() {
		connected[st.Name] = st
	}
	type row struct {
		Name       string `json:"name"`
		Transport  string `json:"transport"`
		Permission string `json:"permission,omitempty"`
		Connected  bool   `json:"connected"`
		HTTPStatus int    `json:"http_status,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	var rows []row
	if cfg != nil {
		for _, srv := range cfg.Servers {
			item := row{Name: srv.Name, Transport: srv.Transport(), Permission: strings.TrimSpace(srv.Permission)}
			if st, hit := connected[srv.Name]; hit {
				item.Connected = st.Connected
				item.Error = st.Error
			}
			if srv.Transport() == "http" {
				code, err := mcps.ProbeHTTP(c.Request.Context(), srv.URL)
				item.HTTPStatus = code
				if err != nil {
					item.Error = err.Error()
				}
			}
			rows = append(rows, item)
		}
	}
	if rows == nil {
		rows = []row{}
	}
	payload := gin.H{"configured": ok, "servers": rows}
	if cfgErr != nil {
		payload["error"] = cfgErr.Error()
	}
	c.JSON(http.StatusOK, payload)
}

// mcpSummary 是状态接口用的一行摘要（未配置时为空串）。
// 与 mcpStatus 一样不触发连接：状态接口不该起进程。
func mcpSummary() string {
	m := currentMCPManager()
	if m == nil {
		if _, _, ok := mcpConfigSource(); ok {
			return "MCP 已配置，尚未连接（首次对话时连接）"
		}
		return ""
	}
	servers := 0
	for _, st := range m.Status() {
		if st.Connected {
			servers++
		}
	}
	return fmt.Sprintf("已连接 %d 个 MCP 服务器，暴露 %d 个工具", servers, len(m.Tools()))
}
