package web

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/rag"
)

// ---------- 记忆系统（契约 §5 / §6）：知识库并入对话后对外只暴露这两个入口 ----------

// kbMemoryPrompt 是「记忆自动注入」的唯一入口，供 /api/chat 的 agent 分支调用。
//
// 契约 §5：用用户这条消息检索记忆 top-k，拼成 <memory>…</memory> 块拼进 system prompt；
// **检索失败不得阻断对话**。这里有两道保险：
//  1. rag.MemoryPrompt 内部已把检索失败降级为「无记忆」（空串）；
//  2. 这里再 recover 一次 —— 例如 DB 未初始化时 kb() 的懒加载可能在检索途中 panic。
//
// 返回值直接拼到 system prompt 后面即可；空串表示本轮没有记忆可用。
// 第二个返回值是**降级原因**（非空表示这轮没用上记忆），由调用方如实显示给用户。
//
// 为什么要有这个返回值：以前失败只写一行服务端日志，用户看到的是“助手突然失忆”
// 却不知道为什么。注意它用的是严格版 MemoryBlock：嵌入不可用时会先走关键词兜底
// （见 rag/store_lexical.go），所以这里报错通常意味着**连关键词检索也失败了**（比如数据库不通）。
// kbMemoryPrompt 拼本轮要注入的记忆块。scope 是当前会话模式对应的记忆类别
// （life / work / ""）——同 scope 的记忆会加权：陪你聊天时优先想起生活类事实，
// 干活时优先想起项目类事实（scope 定义见 web/chat_kind.go）。
func kbMemoryPrompt(ctx context.Context, userID, query, scope string) (block string, degrade string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[memory] 注入记忆时发生意外，本轮降级为无记忆：%v", r)
			block, degrade = "", "记忆检索出意外（本轮没有用上记忆）"
		}
	}()
	b, err := rag.MemoryBlockWithOptions(ctx, kb(), userID, query, rag.MemoryQueryOptions{
		TopK:  rag.DefaultMemoryTopK,
		Scope: scope,
	})
	if err != nil {
		log.Printf("[memory] 注入失败，本轮降级为无记忆：%v", err)
		return "", "记忆检索失败（本轮没有用上记忆）" + err.Error()
	}
	return b, ""
}

// kbMemoryTools 返回要注册给 agent 的两个记忆工具（契约 §6）。
// userID 在构造时绑定：agent.Exec 里只有 SessionID/Root，没有用户身份。
func kbMemoryTools(userID string) []agent.Tool {
	return rag.NewMemoryTools(kb(), userID)
}

// jsonMustMarshal 只用于 SSE 负载：这些结构一定可序列化，出错时给个兜底字符串即可，
// 不值得为此把整个响应流中断掉。
func jsonMustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":"内部序列化失败"}`)
	}
	return b
}

// kbSourceFilter 归一化 /api/kb/documents 的 source 查询参数。
// 契约（前端分段控件）：
//
//	all      —— 全部（缺省）
//	memory   —— 只返回 source='memory'（对话里自动抽取的记忆）
//	document —— 只返回 source <> 'memory'（用户上传/粘贴的资料 + 系统生成的「用户画像」）
//
// 其它值一律当 all —— 参数写错不该让整个列表报错。
func kbSourceFilter(raw string) string {
	switch strings.TrimSpace(raw) {
	case "memory":
		return "memory"
	case "document":
		return "document"
	default:
		return "all"
	}
}

// kbScopeFilter 归一化 /api/kb/documents 的 scope 查询参数（陪伴/工作记忆分开）：
//
// all          —— 全部（缺省）
// life         —— 只返回 scope='life'（生活/陪伴）
// work         —— 只返回 scope='work'（项目/技术）
// unclassified —— scope 为空（旧数据/未分类，含 NULL）
//
// 其它值一律当 all —— 参数写错不该让整个列表报错。
func kbScopeFilter(raw string) string {
	switch strings.TrimSpace(raw) {
	case "life":
		return "life"
	case "work":
		return "work"
	case "unclassified":
		return "unclassified"
	default:
		return "all"
	}
}
