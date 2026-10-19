// Package agent 是 AI 自主工具调用的核心：工具接口、工具注册表与「模型 ⇄ 工具」主循环。
//
// 边界（契约 docs/agent/00-contract.md §3）：
//   - 本包只管「调度」：把历史发给模型、收 tool_calls、执行工具、把结果回灌、决定何时结束。
//   - 本包不实现任何具体工具（read/write/edit/read_image 在 agent/fs），也不认识 HTTP/SSE（在 web）。
//   - 模型调用通过包级 StreamFunc 注入（见 stream.go），因此主循环可以用假模型做确定性单测。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"blog-my/internal/llm"
)

// Tool 是一个可被模型调用的工具。
//
// 参数是**原始 JSON**（llm.ToolCall.Arguments 不做预解析）：由工具自己解析并校验，
// 这样每个工具能给出自己领域内最准确的报错（例如 fs 的 FS_STALE_VERSION）。
type Tool interface {
	Name() string
	Description() string
	// Parameters 返回 JSON Schema object，直接交给模型。
	Parameters() map[string]any
	// Execute 执行一次调用。返回 error 表示这次调用失败——
	// 注意：失败**不是**循环的终点，循环会把它作为 is_error=true 的结果回灌给模型（契约 §3.5）。
	Execute(ctx context.Context, args json.RawMessage, ex *Exec) (Result, error)
}

// Result 是一次工具执行的结果。
type Result struct {
	// Content 是给模型看的正文（工具结果的文本）。失败时这里是给模型看的错误说明。
	Content string `json:"content"`
	// Display 是给前端看的结构化摘要（可选）。
	Display map[string]any `json:"display,omitempty"`
	// IsError 为 true 表示这次调用失败。模型据此决定重试、改参数还是换思路。
	IsError bool `json:"is_error"`
	// Image 非空表示工具产出了图片（read_image）。循环会把它作为一条独立的 user 消息追加——
	// tool 角色的消息带不了图片，所以只能这样回灌。
	Image *ImagePart `json:"-"`
	// AwaitUser 非空表示工具需要用户补充输入（ask_user）：循环会立即停下并把这句话交给
	// 前端展示，用户的回答作为下一条消息进入历史。工具不能替用户回答自己的问题。
	AwaitUser string `json:"-"`
}

// ImagePart 是一张随工具结果回灌给模型的图片。
type ImagePart struct {
	MIME string
	Data []byte
}

// Exec 是一次运行的执行上下文：文件工具的沙箱根目录从这里取。
type Exec struct {
	SessionID string
	Root      string   // 工具可操作的根目录（绝对路径）
	Roots     []string // 同一工作区中其余可操作目录；路径通过 @workspaceN/... 选择。
	// AllowOutside 为 true 时，文件工具额外放行工作区之外的**绝对路径**（Clean 归一化，
	// 系统目录仍然永远拒绝）。默认 false：与改造前完全一致，工作区之外的路径一律拒绝。
	// 写类工具在非 auto 档会先由 web 层的外部路径确认壳问过用户，才走到这里。
	AllowOutside bool
	// ReadRoot 是一个**只能读**的额外根目录（技能目录），供 read/list 查阅技能说明与脚本。
	// 写类工具（write/edit/move/delete/office_edit）一律不看它——技能目录对模型是只读的。
	ReadRoot string
	Signal   context.Context
}

// Registry 是工具注册表：既给循环提供「按名字查工具」，也给模型提供工具定义清单。
type Registry struct {
	mu    sync.RWMutex
	order []string
	tools map[string]Tool
}

// NewRegistry 建一个注册表。传入的工具按顺序登记（顺序即暴露给模型的顺序，稳定可预期）。
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		_ = r.Register(t) // 重复/非法名字在构造时静默跳过，避免让 NewRegistry 变成必须处理错误的 API
	}
	return r
}

// Register 登记一个工具。名称为空或重名都会报错（重名会让模型调用指向不确定的工具）。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("agent: 不能注册 nil 工具")
	}
	name := strings.TrimSpace(t.Name())
	if name == "" {
		return fmt.Errorf("agent: 工具名不能为空")
	}
	if r == nil {
		return fmt.Errorf("agent: 注册表未初始化")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("agent: 工具 %q 重复注册", name)
	}
	r.tools[name] = t
	r.order = append(r.order, name)
	return nil
}

// Lookup 按名字取工具。
func (r *Registry) Lookup(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回已登记的工具名（登记顺序）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Len 返回工具数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Defs 返回交给模型的工具定义清单（契约 §2 的 llm.ToolDef）。
//
// Parameters 为 nil 时补一个空 object schema：部分后端要求 parameters 必须是对象，
// 缺字段会被上游拒掉。
func (r *Registry) Defs() []llm.ToolDef {
	names := r.Names()
	if len(names) == 0 {
		return nil
	}
	out := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		t, ok := r.Lookup(name)
		if !ok {
			continue
		}
		params := t.Parameters()
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, llm.ToolDef{
			Name:        name,
			Description: t.Description(),
			Parameters:  params,
		})
	}
	return out
}

// sortedNames 用于错误文案里给出「可用工具」清单。
func (r *Registry) sortedNames() string {
	names := r.Names()
	if len(names) == 0 {
		return "（当前没有可用工具）"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
