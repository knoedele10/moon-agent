package mcps

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ServerConfig 是一个 MCP 服务器的配置。
type ServerConfig struct {
	// Name 是工具前缀名：工具会以 mcp__<name>__<tool> 出现。
	Name string `json:"name"`
	// Command/Args/Dir/Env 决定怎么起这个 stdio 服务器。
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Dir      string            `json:"dir"`
	Env      map[string]string `json:"env"`
	Disabled bool              `json:"disabled"`
	// URL 非空且没有 command 时，这是一个待探测的 HTTP MCP，本进程不会把它当 stdio 拉起。
	URL string `json:"url,omitempty"`
	// Permission 是给用户看的权限声明：read、write 或 network。不参与执行。
	Permission string `json:"permission,omitempty"`

	// 可选的超时（秒）；0 用默认值。
	InitTimeoutSeconds int `json:"initTimeoutSeconds"`
	CallTimeoutSeconds int `json:"callTimeoutSeconds"`
}

// Config 是 MCP 配置文件的内容。
type Config struct {
	Servers []ServerConfig `json:"servers"`
}

// ParseConfig 解析并校验配置：名字必填且唯一，启用项必须有 command。
// 未知字段直接忽略（前向兼容），校验失败一次性报全，便于用户一次改完。
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(cfg.Servers))
	var problems []string
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			problems = append(problems, "第 "+strconv.Itoa(i+1)+" 个服务器缺少 name")
			continue
		}
		if seen[s.Name] {
			problems = append(problems, "服务器名重复："+s.Name)
			continue
		}
		seen[s.Name] = true
		if s.Disabled {
			continue
		}
		if strings.TrimSpace(s.Command) == "" && strings.TrimSpace(s.URL) == "" {
			problems = append(problems, "服务器 "+s.Name+" 缺少 command 或 url")
		}
	}
	if len(problems) > 0 {
		return nil, &ConfigError{Problems: problems}
	}
	return &cfg, nil
}

// ConfigError 汇总所有配置问题。
type ConfigError struct {
	Problems []string
}

func (e *ConfigError) Error() string {
	return "mcps: 配置有误：" + strings.Join(e.Problems, "；")
}

// InitTimeout 返回握手超时（0 → 默认）。
func (s ServerConfig) InitTimeout() time.Duration {
	if s.InitTimeoutSeconds > 0 {
		return time.Duration(s.InitTimeoutSeconds) * time.Second
	}
	return DefaultInitTimeout
}

// CallTimeout 返回单次调用超时（0 → 默认）。
func (s ServerConfig) CallTimeout() time.Duration {
	if s.CallTimeoutSeconds > 0 {
		return time.Duration(s.CallTimeoutSeconds) * time.Second
	}
	return DefaultCallTimeout
}

// Enabled 返回本次是否应该启动这个 stdio 服务器。只有 URL 的条目不在这里启动。
func (s ServerConfig) Enabled() bool {
	return !s.Disabled && strings.TrimSpace(s.Command) != ""
}

// Transport 是 stdio 或 http。http 只做健康探测，不在本进程里建立 MCP 会话。
func (s ServerConfig) Transport() string {
	if strings.TrimSpace(s.Command) == "" && strings.TrimSpace(s.URL) != "" {
		return "http"
	}
	return "stdio"
}
