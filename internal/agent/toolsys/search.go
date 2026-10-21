package toolsys

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// searchToolName 是渐进披露入口的固定工具名。
const searchToolName = "tool_search"

// SearchTool 返回 tool_search 工具。
//
// 模型先用它按关键词找工具，再决定调用什么；命名/别名/描述都参与打分。
// activate=true（默认）会把命中的工具真的激活，下一轮请求就会带上它们的定义。
func SearchTool(c *Catalog) agent.Tool {
	return searchTool{catalog: c}
}

// searchTool 是 tool_search 的实现。
type searchTool struct {
	catalog *Catalog
}

// Name 返回工具名。
func (s searchTool) Name() string { return searchToolName }

// Description 返回工具描述（写给模型看，强调先搜再猜）。
func (s searchTool) Description() string {
	return "搜索本会话可用的工具。先用它按关键词找工具，不要凭记忆猜工具名或参数；" +
		"命中后默认会自动激活，下一轮就可以直接调用。query 用「动作 + 对象」这类关键词（如「读取 文件」「记忆 搜索」），" +
		"limit 缺省 5、最多 12。若结果里说未激活，把 activate 设为 true 再搜一次。"
}

// Parameters 返回 JSON Schema。
func (s searchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "要查找的能力关键词，如「读取 文件」「发送 邮件」",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "最多返回几条，缺省 5，上限 12",
			},
			"activate": map[string]any{
				"type":        "boolean",
				"description": "是否激活命中的工具（默认 true）；true 时下一轮可直接调用",
			},
		},
		"required": []string{"query"},
	}
}

// searchArgs 是 tool_search 的参数。
type searchArgs struct {
	Query    string `json:"query"`
	Limit    int    `json:"limit"`
	Activate *bool  `json:"activate"` // 指针区分「没传」与「显式 false」
}

// Execute 搜索并（按需）激活工具。
func (s searchTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var args searchArgs
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return failure("TOOL_SEARCH_BAD_ARGS", "参数不是合法 JSON："+err.Error()+`。请传 {"query": "关键词"}。`), nil
		}
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return failure("TOOL_SEARCH_QUERY_MISSING",
			`缺少必填参数 "query"：请填要查找的能力关键词，例如 {"query": "读取 文件"}。`), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	activate := true
	if args.Activate != nil {
		activate = *args.Activate
	}
	if s.catalog == nil {
		return failure("TOOL_SEARCH_UNAVAILABLE", "工具目录未初始化，无法搜索工具。请直接使用已激活的工具。"), nil
	}

	matches := s.catalog.Search(query, limit)
	if len(matches) == 0 {
		total, active := s.catalog.Len()
		return agent.Result{
			Content: fmt.Sprintf("没有找到与 %q 匹配的工具（目录共 %d 个工具，已激活 %d 个）。"+
				"请换用更通用的关键词（如「文件」「记忆」「网络」）再搜一次；不要凭空调用不存在的工具。",
				query, total, active),
			Display: map[string]any{"code": "TOOL_SEARCH_EMPTY", "query": query, "count": 0},
		}, nil
	}

	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.Name)
	}
	var activated []string
	if activate {
		activated = s.catalog.Activate(names...)
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "工具搜索 %q：命中 %d 个（limit=%d）：\n", query, len(matches), limit)
	for _, match := range matches {
		state := "已激活"
		if activate {
			if containsString(activated, match.Name) {
				state = "本次新激活"
			}
		} else if match.Active {
			state = "已激活"
		} else {
			state = "未激活"
		}
		summary := match.Description
		if summary == "" {
			summary = "（无描述）"
		}
		fmt.Fprintf(&builder, "- %s（toolset=%s，风险=%s，%s）：%s\n",
			match.Name, match.Toolset, match.Risk, state, summary)
	}
	switch {
	case activate && len(activated) > 0:
		builder.WriteString("以上标为「本次新激活」的工具已经激活，下一轮可直接调用，无需再搜索；调用前请按工具定义传参。\n")
	case activate:
		builder.WriteString("以上工具此前都已激活，下一轮可直接调用。\n")
	default:
		builder.WriteString("本次未激活任何工具（activate=false）。要调用它们，请把 activate 设为 true 重新搜索，或先调用已激活的工具。\n")
	}

	return agent.Result{
		Content: builder.String(),
		Display: map[string]any{
			"code":      "TOOL_SEARCH_OK",
			"query":     query,
			"count":     len(matches),
			"matches":   names,
			"activated": nonNilStrings(activated),
			"summary":   fmt.Sprintf("搜索到 %d 个工具", len(matches)),
		},
	}, nil
}

// nonNilStrings 保证 JSON 里 activated 是 [] 而不是 null。
func nonNilStrings(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}
