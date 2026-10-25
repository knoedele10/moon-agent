package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// 本文件把记忆系统暴露成两个 agent 工具（契约 §6）：
//
//	memory_search(query, limit?)  主动翻找记忆
//	memory_write(text, tags?)     记下一条记忆
//
// 自动注入（MemoryPrompt）解决"顺带想起"，工具解决"主动翻找"，两者共存。
//
// 关于 userID：agent.Exec 里只有 SessionID/Root，没有用户身份，
// 所以工具实例在构造时就把 userID 绑进去（每次请求现建一组工具）。

// MemoryBackend 是记忆系统对外需要的能力：既能检索、也能写入。*Ingester 天然满足。
type MemoryBackend interface {
	MemorySearcher
	MemoryWriter
}

// NewMemoryTools 返回这组记忆工具。web 接线层把它和 fs 工具一起塞进 agent.Registry。
func NewMemoryTools(s MemoryBackend, userID string) []agent.Tool {
	return []agent.Tool{
		NewMemorySearchTool(s, userID),
		NewMemoryWriteTool(s, userID),
	}
}

// MemorySearchTool 让模型自己决定"主动翻找记忆"。
type MemorySearchTool struct {
	s      MemorySearcher
	userID string
}

func NewMemorySearchTool(s MemorySearcher, userID string) *MemorySearchTool {
	return &MemorySearchTool{s: s, userID: userID}
}

func (t *MemorySearchTool) Name() string { return "memory_search" }

func (t *MemorySearchTool) Description() string {
	return "检索你的长期记忆（用户的知识库）。当需要回忆用户以前说过的偏好、事实、决定，" +
		"或者对话开头自动注入的 <memory> 片段不够用时，用它主动翻找。" +
		"返回按相关度排序的记忆片段；返回空表示记忆里确实没有相关内容。"
}

func (t *MemorySearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "要查什么，用自然语言或关键词都行，例如「用户喝咖啡的偏好」",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("最多返回几条，默认 %d，最大 %d", DefaultMemoryTopK, MaxMemoryTopK),
				"minimum":     1,
				"maximum":     MaxMemoryTopK,
			},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}

type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// Execute 实现 agent.Tool。args 是模型给的原始 JSON，由本工具自己解析并校验（契约 §3）。
func (t *MemorySearchTool) Execute(ctx context.Context, args json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var in memorySearchArgs
	if err := decodeToolArgs(args, &in); err != nil {
		return agent.Result{Display: memoryErrorDisplay("MEMORY_BAD_ARGS")}, err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return agent.Result{Display: memoryErrorDisplay("MEMORY_EMPTY_QUERY")},
			errors.New("query 不能为空：请给出要检索的内容")
	}

	hits, err := RunMemorySearch(ctx, t.s, t.userID, query, in.Limit)
	if err != nil {
		// 检索失败如实上报：循环会把它折成 is_error 结果回灌给模型（契约 §3.5），
		// 模型可以换个词再试，或者老实说"想不起来"。对话本身不会断。
		return agent.Result{Display: memoryErrorDisplay("MEMORY_SEARCH_FAILED")},
			fmt.Errorf("记忆检索失败：%w", err)
	}

	display := map[string]any{"count": len(hits), "query": query, "hits": hitDigests(hits)}
	if len(hits) == 0 {
		return agent.Result{
			Content: fmt.Sprintf("记忆里没有找到与「%s」相关的内容。", query),
			Display: display,
		}, nil
	}
	return agent.Result{
		Content: fmt.Sprintf("记忆里找到 %d 条与「%s」相关的内容：\n\n%s", len(hits), query, BuildContext(hits, memoryMaxRunes)),
		Display: display,
	}, nil
}

// MemoryWriteTool 让模型自己决定"把这条信息记下来"。
type MemoryWriteTool struct {
	w      MemoryWriter
	userID string
}

func NewMemoryWriteTool(w MemoryWriter, userID string) *MemoryWriteTool {
	return &MemoryWriteTool{w: w, userID: userID}
}

func (t *MemoryWriteTool) Name() string { return "memory_write" }

func (t *MemoryWriteTool) Description() string {
	return "把一条值得长期记住的信息写进你的记忆（用户明确表达的偏好、事实、决定、约定等）。" +
		"写进去之后，以后每轮对话都可能自动想起它，也能用 memory_search 翻出来。" +
		"只记真正有长期价值的内容，不要把寒暄、临时问题和 AI 自己的推测写进去；内容完全相同的记忆不会重复写入。"
}

func (t *MemoryWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "要记住的内容，写成一句自足的话（脱离上下文也能看懂），例如「用户偏好美式咖啡，不加糖」",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "可选标签，便于以后归类与检索，例如 [偏好, 饮食]",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "可选来源，例如文件路径、网页或用户原话。会写进记忆正文开头，便于以后引用。",
			},
		},
		"required":             []string{"text"},
		"additionalProperties": false,
	}
}

type memoryWriteArgs struct {
	Text   string   `json:"text"`
	Tags   []string `json:"tags"`
	Source string   `json:"source"`
}

// Execute 实现 agent.Tool。
func (t *MemoryWriteTool) Execute(ctx context.Context, args json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var in memoryWriteArgs
	if err := decodeToolArgs(args, &in); err != nil {
		return agent.Result{Display: memoryErrorDisplay("MEMORY_BAD_ARGS")}, err
	}
	text := citeMemorySource(strings.TrimSpace(in.Text), in.Source)
	if strings.TrimSpace(in.Text) == "" {
		return agent.Result{Display: memoryErrorDisplay("MEMORY_EMPTY_TEXT")},
			errors.New("text 不能为空：请给出要记住的内容")
	}

	res, err := RunMemoryWrite(ctx, t.w, t.userID, text, in.Tags)
	if err != nil {
		return agent.Result{Display: memoryErrorDisplay("MEMORY_WRITE_FAILED")},
			fmt.Errorf("写入记忆失败：%w", err)
	}

	display := map[string]any{
		"id": res.DocumentID, "kind": res.Kind, "title": res.Title,
		"chunk_count": res.ChunkCount, "duplicated": res.Duplicated,
	}
	if res.Duplicated {
		return agent.Result{
			Content: fmt.Sprintf("这条记忆已经存在（记忆 id=%d，标题《%s》），没有重复写入。", res.DocumentID, res.Title),
			Display: display,
		}, nil
	}
	return agent.Result{
		Content: fmt.Sprintf("已记住（记忆 id=%d，标题《%s》，共 %d 个片段）。", res.DocumentID, res.Title, res.ChunkCount),
		Display: display,
	}, nil
}

// ---------- 共用小工具 ----------

// decodeToolArgs 解析模型给的原始 JSON 参数。
// 契约 §3 明确不做预解析：报错要说人话，模型据此改参数重试。
func decodeToolArgs(args json.RawMessage, v any) error {
	if len(strings.TrimSpace(string(args))) == 0 {
		return errors.New("缺少参数：需要一个 JSON 对象")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("参数不是合法 JSON：%v", err)
	}
	return nil
}

func citeMemorySource(text, source string) string {
	source = strings.Join(strings.Fields(source), " ")
	if source == "" {
		return text
	}
	if len([]rune(source)) > 200 {
		source = string([]rune(source)[:200])
	}
	return "[来源: " + source + "]\n" + text
}

func memoryErrorDisplay(code string) map[string]any {
	return map[string]any{"code": code}
}

// hitDigests 把命中压成前端友好的摘要：正文走 Content 给模型，
// Display 只给 id/标题/相关度/前若干字，避免 SSE 负载过大。
func hitDigests(hits []Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"chunk_id":    h.ChunkID,
			"document_id": h.DocumentID,
			"title":       h.Title,
			"kind":        h.Kind,
			"score":       h.Score,
			"preview":     clipRunes(strings.TrimSpace(h.Text), 80),
		})
	}
	return out
}
