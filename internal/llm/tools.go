package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/model"
	einoschema "github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// ToolCall 是一次工具调用（契约 §2）。
//
// Arguments 是模型给出的**原始 JSON 字符串**，本包不预先解析：
// 由工具自己解析并校验参数（契约 agent §3），解析失败的责任在工具侧。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef 是调用方暴露给模型的工具定义；Parameters 是 JSON Schema object。
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	// ErrToolsUnsupported：req.Tools 非空，但该后端没有实现 model.ToolCallingChatModel。
	// 契约 §2 规定的错误；Stream/Generate 都会在**开流之前**返回它。
	ErrToolsUnsupported = errors.New("llm: 该后端不支持工具调用")

	// ErrToolDefInvalid：ToolDef 本身非法（缺 name，或 parameters 不是合法 JSON Schema）。
	// 这是契约 §2 之外的新增错误码（Lead 2026-10-12 已批准，待同步进契约）。
	ErrToolDefInvalid = errors.New("llm: 工具定义非法")
)

// toToolInfos 把本包的 ToolDef 映射成 Eino 的 ToolInfo。
//
// Parameters 直接按 JSON Schema 传给 Eino（NewParamsOneOfByJSONSchema），
// 不走 ParameterInfo 转换——那是有损的：fs 工具的参数里有嵌套对象、enum、
// additionalProperties，只有完整 JSON Schema 才表达得下。
func toToolInfos(defs []ToolDef) ([]*einoschema.ToolInfo, error) {
	out := make([]*einoschema.ToolInfo, 0, len(defs))
	for i, d := range defs {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: 第 %d 个工具缺少 name", ErrToolDefInvalid, i)
		}
		info := &einoschema.ToolInfo{Name: name, Desc: d.Description}
		if len(d.Parameters) > 0 {
			raw, err := json.Marshal(d.Parameters)
			if err != nil {
				return nil, fmt.Errorf("%w: 工具 %s 的 parameters 无法序列化: %v", ErrToolDefInvalid, name, err)
			}
			spec := &jsonschema.Schema{}
			if err := json.Unmarshal(raw, spec); err != nil {
				return nil, fmt.Errorf("%w: 工具 %s 的 parameters 不是合法 JSON Schema: %v", ErrToolDefInvalid, name, err)
			}
			info.ParamsOneOf = einoschema.NewParamsOneOfByJSONSchema(spec)
		}
		out = append(out, info)
	}
	return out, nil
}

// ToToolInfos lets Eino's ToolsNode use the project's existing tool schemas.
func ToToolInfos(defs []ToolDef) ([]*einoschema.ToolInfo, error) { return toToolInfos(defs) }

// withTools 在 req.Tools 非空时把工具绑到模型上并返回**新实例**。
//
// 用 Eino v0.9.21 的 model.ToolCallingChatModel.WithTools：返回新实例，并发请求
// 各持一份，互不影响。**绝不用已废弃的 model.ChatModel.BindTools**——它就地改写
// 实例，两个并发请求会互相覆盖工具集（Eino 接口注释里明确写了这个 race）。
//
// req.Tools 为空时原样返回 cm：这是一条热路径，也是 CUA 依赖的那条老路径。
func withTools(cm model.BaseChatModel, req ChatRequest) (model.BaseChatModel, error) {
	if len(req.Tools) == 0 {
		return cm, nil
	}
	tc, ok := cm.(model.ToolCallingChatModel)
	if !ok {
		return nil, ErrToolsUnsupported
	}
	infos, err := toToolInfos(req.Tools)
	if err != nil {
		return nil, err
	}
	next, err := tc.WithTools(infos)
	if err != nil {
		return nil, fmt.Errorf("llm: 绑定工具失败: %w", err)
	}
	if next == nil {
		return nil, ErrToolsUnsupported
	}
	return next, nil
}

// toolCallChunk 是从 eino schema.ToolCall 归一出来的一个流式分片。
// 单独抽出来的目的：归并逻辑可以脱离 eino 类型做单元测试与负向对照。
type toolCallChunk struct {
	Index     *int
	ID        string
	Name      string
	Arguments string
}

// toolCallChunkFromSchema 把 eino 的 tool_call 分片归一成本包的中间形态。
func toolCallChunkFromSchema(tc einoschema.ToolCall) toolCallChunk {
	return toolCallChunk{
		Index:     tc.Index,
		ID:        tc.ID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	}
}

// toolCallAccumulator 把流式到达的 tool_call 分片按 Index 归并成整条调用。
//
// 归并规则（契约 §2「按 ToolCall.Index 归并后再发」）：
//   - 带 Index：同一 Index 的所有分片拼成一条；ID/Name 取第一次出现的非空值，
//     Arguments 按到达顺序拼接。输出时按 Index 升序——Index 就是模型给出的位次，
//     即使分片乱序到达（负向对照用例），发出的顺序也是确定的。
//   - 不带 Index：分片自带 ID 或 Name 时视为一条独立调用，按到达顺序追加；
//     只有 Arguments 的裸续片拼到「最近一条」上，避免丢数据。
//   - ID/Name/Arguments 全空的分片（哪怕带 Index）：丢弃。这类分片是 keep-alive 噪声，
//     放进去会造出"没有名字没有参数"的幽灵调用；就算它真是某条调用的开场，
//     后续带参数的分片仍会凭同一个 Index 把这条补出来，不会丢数据。
type toolCallAccumulator struct {
	indexed map[int]*ToolCall
	order   []int // 各 Index 首次出现的顺序；输出前排序以保证确定性
	loose   []*ToolCall
}

func (a *toolCallAccumulator) add(c toolCallChunk) {
	if c.ID == "" && c.Name == "" && c.Arguments == "" {
		return
	}

	if c.Index == nil {
		if c.ID != "" || c.Name != "" {
			a.loose = append(a.loose, &ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
			return
		}
		if n := len(a.loose); n > 0 {
			a.loose[n-1].Arguments += c.Arguments
			return
		}
		if last := a.lastIndexed(); last != nil {
			last.Arguments += c.Arguments
			return
		}
		a.loose = append(a.loose, &ToolCall{Arguments: c.Arguments})
		return
	}

	idx := *c.Index
	if a.indexed == nil {
		a.indexed = make(map[int]*ToolCall)
	}
	cur := a.indexed[idx]
	if cur == nil {
		a.indexed[idx] = &ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments}
		a.order = append(a.order, idx)
		return
	}
	if cur.ID == "" {
		cur.ID = c.ID
	}
	if cur.Name == "" {
		cur.Name = c.Name
	}
	cur.Arguments += c.Arguments
}

// lastIndexed 返回"最近一次出现的带 Index 的调用"，供裸续片拼接。
func (a *toolCallAccumulator) lastIndexed() *ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	return a.indexed[a.order[len(a.order)-1]]
}

// merged 返回归并后的完整调用列表：带 Index 的按 Index 升序在前，
// 无 Index 的按到达顺序在后。没有任何分片时返回 nil。
func (a *toolCallAccumulator) merged() []ToolCall {
	if len(a.indexed) == 0 && len(a.loose) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.indexed)+len(a.loose))
	idxs := append([]int(nil), a.order...)
	sort.Ints(idxs)
	for _, i := range idxs {
		out = append(out, *a.indexed[i])
	}
	for _, c := range a.loose {
		out = append(out, *c)
	}
	return out
}
