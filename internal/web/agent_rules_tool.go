package web

import (
	"context"
	"encoding/json"
	"fmt"

	"blog-my/internal/agent"
	"blog-my/internal/agent/rules"
)

// initAgentsTool 扫描工作区并写入 AGENTS.md。已存在时不覆盖。
type initAgentsTool struct{}

func (initAgentsTool) Name() string { return "init_agents_md" }
func (initAgentsTool) Description() string {
	return "扫描当前工作区的语言和测试命令，生成一份 AGENTS.md。文件已存在时不会覆盖。"
}
func (initAgentsTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"force": map[string]any{"type": "boolean", "description": "为 true 时覆盖已有 AGENTS.md"},
	}}
}
func (initAgentsTool) Execute(_ context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ex == nil || ex.Root == "" {
		return agent.Result{IsError: true, Content: "没有工作区，不能生成 AGENTS.md。"}, nil
	}
	var a struct {
		Force bool `json:"force"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return agent.Result{}, fmt.Errorf("init_agents_md 参数不是合法 JSON: %w", err)
		}
	}
	path, created, err := rules.Scaffold(ex.Root, a.Force)
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	if !created {
		return agent.Result{Content: "AGENTS.md 已存在，没有覆盖：" + path + "。需要重写时传 force=true。"}, nil
	}
	return agent.Result{Content: "已写入 " + path + "。下一轮对话会读到它。", Display: map[string]any{"path": path, "created": true}}, nil
}
