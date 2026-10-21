package mcps

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"blog-my/internal/agent"
)

// ServerStatus 是单个服务器的连接状态，供 /api/agent/evolution 这类状态接口展示。
type ServerStatus struct {
	Name          string `json:"name"`
	Connected     bool   `json:"connected"`
	ToolCount     int    `json:"toolCount"`
	ServerName    string `json:"serverName,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Manager 负责一组 MCP 服务器的生命周期：启动、列工具、关进程。
//
// 一个 Manager 对应一个进程（web 层全局一份），不给每次对话重复起进程：
// npx 起服务器的代价是秒级，每轮对话都起会把体验拖垮。
type Manager struct {
	mu      sync.Mutex
	clients []*Client
	tools   []agent.Tool
	status  []ServerStatus
	closed  bool

	// start 可替换（测试注入假服务器）；nil 时用 StartStdio。
	start func(ctx context.Context, cfg ServerConfig) (*Client, error)
}

// NewManager 建一个空管理器（不启动任何进程）。
func NewManager() *Manager { return &Manager{} }

// SetStarter 替换服务器启动函数，仅用于测试。
func (m *Manager) SetStarter(fn func(ctx context.Context, cfg ServerConfig) (*Client, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.start = fn
}

func (m *Manager) starter() func(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if m.start != nil {
		return m.start
	}
	return StartStdio
}

// LoadConfigFile 读配置文件；文件不存在时返回 (nil, nil)，让「没配 = 不启用」成立。
func LoadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mcps: 读取配置 %s 失败：%w", path, err)
	}
	return ParseConfig(data)
}

// ConnectAll 启动所有启用的服务器并收集工具。
//
// 单个服务器失败不致命：记录下来继续连下一个，Tools() 只返回成功的工具。
// 这条取舍是为了让「一个 MCP 服务器坏了」不要拖垮整轮对话。
func (m *Manager) ConnectAll(ctx context.Context, cfgs []ServerConfig) []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	start := m.starter()
	for _, cfg := range cfgs {
		st := ServerStatus{Name: cfg.Name}
		if !cfg.Enabled() {
			st.Error = "已禁用"
			m.status = append(m.status, st)
			continue
		}
		client, err := start(ctx, cfg)
		if err != nil {
			st.Error = err.Error()
			m.status = append(m.status, st)
			continue
		}
		specs, err := client.ListTools(ctx)
		if err != nil {
			st.Error = err.Error()
			_ = client.Close()
			m.status = append(m.status, st)
			continue
		}
		info := client.ServerInfo()
		st.Connected = true
		st.ServerName = info.Name
		st.ServerVersion = info.Version
		st.ToolCount = len(specs)
		m.clients = append(m.clients, client)
		for _, spec := range specs {
			m.tools = append(m.tools, WrapTool(client, cfg.Name, spec))
		}
		m.status = append(m.status, st)
	}
	sort.SliceStable(m.tools, func(i, j int) bool { return m.tools[i].Name() < m.tools[j].Name() })
	return append([]ServerStatus(nil), m.status...)
}

// Tools 返回所有已连接服务器暴露的工具（按名字稳定排序）。
func (m *Manager) Tools() []agent.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agent.Tool(nil), m.tools...)
}

// Status 返回连接状态快照。
func (m *Manager) Status() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ServerStatus(nil), m.status...)
}

// Close 关掉所有子进程。幂等，可重复调用。
func (m *Manager) Close() {
	m.mu.Lock()
	clients := m.clients
	m.clients = nil
	m.tools = nil
	m.closed = true
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}
