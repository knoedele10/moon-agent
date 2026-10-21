package mcps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// toolNamePrefix 与 hermes 的命名约定一致：mcp__<server>__<tool>。
// 前缀既避免与内置工具重名，也让模型一眼看出这是外部工具。
const toolNamePrefix = "mcp__"

// safeName 把服务器名/工具名压成模型与日志都友好的形式：
// 只保留字母数字下划线横线，其余换下划线；超长截断（工具名有长度上限）。
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "tool"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// ToolName 返回 MCP 工具在 Moon 里的稳定全名。
func ToolName(server, tool string) string {
	return toolNamePrefix + safeName(server) + "__" + safeName(tool)
}

// toolAdapter 把一个 MCP 工具包成 agent.Tool。
//
// 参数 Schema 原样透传：MCP 的 inputSchema 就是 JSON Schema，转一遍只会丢信息；
// 只在缺失或非法时兜底成空 object。
type toolAdapter struct {
	client  *Client
	server  string
	spec    ToolSpec
	schema  map[string]any
	display string
}

// WrapTool 把 tools/list 的一条定义包成 agent.Tool。
func WrapTool(client *Client, server string, spec ToolSpec) agent.Tool {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	if len(spec.InputSchema) > 0 {
		var parsed map[string]any
		if err := json.Unmarshal(spec.InputSchema, &parsed); err == nil && len(parsed) > 0 {
			if _, hasType := parsed["type"]; !hasType {
				parsed["type"] = "object"
			}
			schema = parsed
		}
	}
	desc := strings.TrimSpace(spec.Description)
	if desc == "" {
		desc = "（该 MCP 工具没有提供描述）"
	}
	return &toolAdapter{
		client:  client,
		server:  server,
		spec:    spec,
		schema:  schema,
		display: server + "/" + spec.Name,
	}
}

func (t *toolAdapter) Name() string { return ToolName(t.server, t.spec.Name) }

func (t *toolAdapter) Description() string {
	return fmt.Sprintf("[MCP:%s] %s", t.server, t.spec.Description)
}

func (t *toolAdapter) Parameters() map[string]any { return t.schema }

// Execute 调 MCP tools/call，并把 content blocks 折成一段文本。
//
// 与内置工具的约定保持一致（契约 §3.5）：失败不返回 error，而是 IsError=true 的
// 结果，让模型自己决定重试/改参数/换思路。
func (t *toolAdapter) Execute(ctx context.Context, args json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	res, err := t.client.CallTool(ctx, t.spec.Name, args)
	if err != nil {
		return agent.Result{
			IsError: true,
			Content: fmt.Sprintf("MCP 工具 %s 调用失败：%v", t.display, err),
			Display: map[string]any{"mcp": t.server, "tool": t.spec.Name, "error": true},
		}, nil
	}

	var text strings.Builder
	var image *agent.ImagePart
	blocks := 0
	for _, b := range res.Content {
		blocks++
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(b.Text)
		case "image":
			mime := b.MIMEType
			if mime == "" {
				mime = b.Mime
			}
			data, derr := base64.StdEncoding.DecodeString(b.Data)
			if derr == nil && len(data) > 0 && image == nil {
				image = &agent.ImagePart{MIME: mime, Data: data}
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(fmt.Sprintf("（图片已返回，类型 %s）", mime))
		case "resource":
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString("（资源：" + b.URI + "）")
		default:
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(fmt.Sprintf("（%s 内容）", b.Type))
		}
	}
	content := text.String()
	if strings.TrimSpace(content) == "" {
		content = "（MCP 工具没有返回文本内容）"
	}

	out := agent.Result{
		Content: content,
		IsError: res.IsError,
		Image:   image,
		Display: map[string]any{"mcp": t.server, "tool": t.spec.Name, "blocks": blocks},
	}
	if res.IsError {
		out.Display["error"] = true
	}
	return out, nil
}
