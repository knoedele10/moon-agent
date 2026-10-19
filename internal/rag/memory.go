package rag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// 本文件实现「知识库 = AI 的记忆系统」这一层（契约 §6）：
//
//   - 自动注入：用用户这条消息检索 top-k，拼成 <memory>…</memory> 注入 system prompt；
//   - 两个工具的业务逻辑：memory_search（主动翻找）/ memory_write（记下一条）。
//
// 设计意图：自动注入解决"顺带想起"，工具解决"主动翻找"，两者共存。
// 铁律（契约 §5）：**检索失败一律降级为「无记忆」，绝不阻断对话**。

// MemorySearcher 是记忆检索所需的最小接口，*Ingester 天然满足。
// 抽成接口不是为了抽象而抽象：它让"检索失败"这条分支在测试里可以确定性构造，
// 不需要真的去把数据库弄坏。
type MemorySearcher interface {
	Search(ctx context.Context, userID, query string, topK int) ([]Hit, error)
}

// ScopedMemorySearcher 是可选的扩展接口：检索时带一个 scope（当前会话的类型）。
// 用「可选接口」而不是改 MemorySearcher 的签名，是为了向后兼容：
// 旧的检索器（含测试里的假实现）不需要改，只要实现这个方法就能获得 scope 加权。
type ScopedMemorySearcher interface {
	SearchScoped(ctx context.Context, userID, query string, topK int, scope string) ([]Hit, error)
}

// MemoryQueryOptions 控制一次记忆检索/注入。
//   - TopK：注入条数上限（<=0 用 DefaultMemoryTopK，>MaxMemoryTopK 收敛）。
//   - Scope：当前会话的类型（"life"/"work"/""）；空串表示不加权。
//   - Budget：注入的 token 预算（<=0 时读 AGENT_MEMORY_TOKEN_BUDGET，仍非法则用默认值）。
type MemoryQueryOptions struct {
	TopK   int
	Scope  string
	Budget int
}

// searchWithScope 调用检索器：实现了 ScopedMemorySearcher 就用带 scope 的版本，
// 否则退回旧接口（scope 被忽略，等价于不加权）。
func searchWithScope(ctx context.Context, s MemorySearcher, userID, query string, topK int, scope string) ([]Hit, error) {
	if ss, ok := s.(ScopedMemorySearcher); ok {
		return ss.SearchScoped(ctx, userID, query, topK, scope)
	}
	return s.Search(ctx, userID, query, topK)
}

// MemoryWriter 是写入一条记忆所需的最小接口，*Ingester 天然满足。
type MemoryWriter interface {
	IngestText(ctx context.Context, userID, title, text string) (Result, error)
}

const (
	// DefaultMemoryTopK 是自动注入默认检索的记忆条数（契约 §5 的 top-k）。
	DefaultMemoryTopK = 5
	// MaxMemoryTopK 是上限：调用方传再大的 limit 也不会把 prompt 撑爆。
	MaxMemoryTopK = 20
	// DefaultMemoryTokenBudget 是自动注入 <memory> 块的默认 token 预算。
	// 旧实现是「取 topK 条全塞」，条数可控但长度不可控；改成按 token 预算贪心填充。
	DefaultMemoryTokenBudget = 1200
	// memoryTokenBudgetEnv 是覆盖默认预算的环境变量名。
	memoryTokenBudgetEnv = "AGENT_MEMORY_TOKEN_BUDGET"
	// memoryMaxRunes 是注入块正文的字符上限，超出的命中块整块丢弃。
	memoryMaxRunes = 4000
	// memoryTitleRunes 是记忆标题的字符上限：kb_document.title 只有 255，
	// 留出余量给 tag 前缀，也避免 MySQL 严格模式报 Data too long。
	memoryTitleRunes = 200

	memoryOpenTag  = "<memory>"
	memoryCloseTag = "</memory>"
)

// memoryHint 说明这段东西是什么、能不能忽略、想翻找时该调什么工具。
const memoryHint = "以下是从你的长期记忆（用户的知识库）中自动检索到的片段，按相关度排序。" +
	"它们可能过时或与当前问题无关：只在确实相关时参考，不要凭空引用，也不要把这段说明复述给用户。" +
	"需要更多记忆时，可以调用 memory_search 工具主动检索。"

// ClampMemoryLimit 把 limit 收敛到 [1, MaxMemoryTopK]，<=0 时用默认值。
func ClampMemoryLimit(n int) int {
	if n <= 0 {
		return DefaultMemoryTopK
	}
	if n > MaxMemoryTopK {
		return MaxMemoryTopK
	}
	return n
}

// FormatMemoryBlock 把命中的记忆拼成 <memory>…</memory>；没有命中返回空串（而不是一个空标签）。
//
// 这是兼容入口：等价于「全部命中都被选中、没有被预算省略」。带预算信息的版本见
// MemoryBlockWithOptions 内部的 formatMemoryBlock。
func FormatMemoryBlock(hits []Hit, maxRunes int) string {
	return formatMemoryBlock(hits, maxRunes, len(hits), 0)
}

// formatMemoryBlock 是拼块的实际实现：total 是本次检索共命中的条数，omitted 是因 token
// 预算被省略的条数。把这两个数写进注入文本，出问题时能直接看出「为什么少了几条」。
func formatMemoryBlock(hits []Hit, maxRunes, total, omitted int) string {
	if len(hits) == 0 {
		return ""
	}
	body := strings.TrimRight(BuildContext(hits, maxRunes), " \t\r\n")
	if body == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(body) + 320)
	b.WriteString(memoryOpenTag)
	b.WriteString("\n")
	b.WriteString(memoryHint)
	b.WriteString("\n")
	if total > 0 {
		fmt.Fprintf(&b, "本次共命中 %d 条，因 token 预算省略 %d 条。\n", total, omitted)
	}
	b.WriteString("\n")
	b.WriteString(body)
	b.WriteString("\n")
	b.WriteString(memoryCloseTag)
	return b.String()
}

// MemoryTokenBudget 读 AGENT_MEMORY_TOKEN_BUDGET；未设/非法/<=0 时用 DefaultMemoryTokenBudget。
func MemoryTokenBudget() int {
	raw := strings.TrimSpace(os.Getenv(memoryTokenBudgetEnv))
	if raw == "" {
		return DefaultMemoryTokenBudget
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultMemoryTokenBudget
	}
	return n
}

// memoryCandidateLimit 是注入前向检索器要的候选条数：多取一些（limit×4，最少 60），
// 让 token 预算贪心有足够的选择空间，而不是只拿到 topK 条就没得选。
func memoryCandidateLimit(topK int) int {
	n := ClampMemoryLimit(topK) * 4
	if n < 60 {
		n = 60
	}
	return n
}

// MemoryBlock 是严格版：检索失败原样返回错误。
// 给需要区分「检索失败」与「确实没有记忆」的调用方用（也是降级测试的对照物）。
//
// 这是兼容入口：topK 条、默认 token 预算、不加 scope。需要 scope/预算时用 MemoryBlockWithOptions。
func MemoryBlock(ctx context.Context, s MemorySearcher, userID, query string, topK int) (string, error) {
	return MemoryBlockWithOptions(ctx, s, userID, query, MemoryQueryOptions{TopK: topK})
}

// MemoryBlockWithOptions 是带 scope/token 预算的严格版。
//
// 流程：多取候选（limit×4，最少 60）→ 检索器已按 priority 降序 → 按 token 预算贪心选取
// → 拼成 <memory> 块（带「共命中 N 条、省略 M 条」说明）。
func MemoryBlockWithOptions(ctx context.Context, s MemorySearcher, userID, query string, opts MemoryQueryOptions) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		// 空消息没什么可翻的，这既不是失败也不产生块。
		return "", nil
	}
	if s == nil {
		return "", errors.New("rag: 记忆检索器为空")
	}
	hits, err := searchWithScope(ctx, s, userID, query, memoryCandidateLimit(opts.TopK), opts.Scope)
	if err != nil {
		return "", err
	}
	budget := opts.Budget
	if budget <= 0 {
		budget = MemoryTokenBudget()
	}
	picked, omitted := selectByTokenBudget(hits, budget)
	return formatMemoryBlock(picked, memoryMaxRunes, len(hits), omitted), nil
}

// MemoryPrompt 是契约 §5「自动注入」的实现：返回可直接拼进 system prompt 的 <memory> 块。
//
// **检索失败不得阻断对话**——这里把错误吞掉、降级为「无记忆」（空串），只往日志里留一行。
// 第二个返回值因此恒为 nil，保留它有两个原因：
//  1. 调用方（web 层）与将来可能的严格模式可以共用同一种写法；
//  2. 让「把降级去掉」这个反例有一个可断言的信号：见 TestMemoryPromptDegradesOnSearchError。
func MemoryPrompt(ctx context.Context, s MemorySearcher, userID, query string, topK int) (string, error) {
	return MemoryPromptWithOptions(ctx, s, userID, query, MemoryQueryOptions{TopK: topK})
}

// MemoryPromptWithOptions 是带 scope/token 预算的注入入口，降级语义与 MemoryPrompt 完全一致。
// 主 agent 要按当前会话类型加权时，把 opts.Scope 设成 "life"/"work" 即可；
// 不传（空串）与旧调用行为一致。
func MemoryPromptWithOptions(ctx context.Context, s MemorySearcher, userID, query string, opts MemoryQueryOptions) (string, error) {
	block, err := MemoryBlockWithOptions(ctx, s, userID, query, opts)
	if err != nil {
		log.Printf("[memory] 检索失败，本轮降级为无记忆：%v", err)
		return "", nil
	}
	return block, nil
}

// RunMemorySearch 是 memory_search 工具的业务逻辑：主动检索记忆。
// limit 省略（<=0）时用 DefaultMemoryTopK，超过 MaxMemoryTopK 会被收敛。
//
// 兼容入口：不加 scope。需要 scope 时用 RunMemorySearchWithOptions。
func RunMemorySearch(ctx context.Context, s MemorySearcher, userID, query string, limit int) ([]Hit, error) {
	return RunMemorySearchWithOptions(ctx, s, userID, query, MemoryQueryOptions{TopK: limit})
}

// RunMemorySearchWithOptions 是带 scope 的 memory_search：返回的条数仍按 TopK 收敛。
func RunMemorySearchWithOptions(ctx context.Context, s MemorySearcher, userID, query string, opts MemoryQueryOptions) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query 不能为空")
	}
	if s == nil {
		return nil, errors.New("rag: 记忆检索器为空")
	}
	return searchWithScope(ctx, s, userID, query, ClampMemoryLimit(opts.TopK), opts.Scope)
}

// RunMemoryWrite 是 memory_write 工具的业务逻辑：把一段文本写成一条可检索的记忆。
// 内容完全相同的记忆会被 sha256 去重（复用已有文档，不新建、不重复嵌入）。
func RunMemoryWrite(ctx context.Context, w MemoryWriter, userID, text string, tags []string) (Result, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, errors.New("text 不能为空")
	}
	if w == nil {
		return Result{}, errors.New("rag: 记忆写入器为空")
	}
	return w.IngestText(ctx, userID, MemoryTitle(text, tags), text)
}

// MemoryTitle 生成记忆的标题。
// IngestText 没有独立的 tags 字段，而标题是唯一「人看得见 + 参与检索」的位置，
// 所以 tags 以 #tag 前缀拼进标题。
func MemoryTitle(text string, tags []string) string {
	base := firstLine(text, 40)
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#"))
		if t == "" {
			continue
		}
		clean = append(clean, "#"+t)
	}
	if len(clean) == 0 {
		return clipRunes(base, memoryTitleRunes)
	}
	return clipRunes(strings.Join(clean, " ")+" "+base, memoryTitleRunes)
}

// clipRunes 按字符（不是字节）截断，避免把中文切成半个字。
func clipRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
