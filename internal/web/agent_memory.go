package web

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"blog-my/internal/llm"
	"blog-my/internal/rag"
)

// 自动记忆：每轮对话结束后，把这一轮里值得长期记住的信息写进记忆库，
// 之后靠既有的记忆自动注入被检索回来。用户不必手动说“记住这个”。
//
// 升级后的流程（对齐 mem0 v3 的 ADD-only 抽取）：
//  1. **首选**：用一次模型调用把这一轮抽成若干条自包含的原子事实，逐条入库（带实体）；
//  2. **降级**：抽取失败或抽不出东西时，退回“整轮拼接”的确定性记录。
//     降级路径保证「记忆不丢」——宁可存得糙，也不能因为模型抽不出来就什么都没留下。
//
// 三条不变量：
//   - 失败只记日志，绝不打断对话；
//   - 内容完全相同的一条不会重复入库（sha256 唯一索引）；
//   - 水位（last_memory_at）在写成功后前移，关闭补齐时不会重写。

// autoMemoryEnabled 读 AGENT_AUTO_MEMORY，默认开启；写 0/false/off/no 可关掉。
func autoMemoryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_AUTO_MEMORY"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// clipRunes 按字符（不是字节）截断，避免把中文切出乱码。
func clipRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// traceSummary 把工具轨迹压成一行行文本，交给抽取器当作“AI 实际做了什么”。
func traceSummary(trace []agentTraceEntry) string {
	if len(trace) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range trace {
		state := "失败"
		if t.OK {
			state = "成功"
		}
		b.WriteString("- ")
		b.WriteString(t.Name)
		b.WriteString("（")
		b.WriteString(state)
		b.WriteString("）：")
		b.WriteString(clipRunes(t.Summary, 200))
		b.WriteString("\n")
	}
	return b.String()
}

// autoMemoryText 是降级路径用的：把一轮对话压成一段可检索文本。
func autoMemoryText(userMsg string, trace []agentTraceEntry, reply string) string {
	var b strings.Builder
	b.WriteString("用户说：")
	b.WriteString(clipRunes(userMsg, 400))

	if len(trace) > 0 {
		b.WriteString("\nAI 做的事：\n")
		b.WriteString(traceSummary(trace))
	}

	if r := strings.TrimSpace(reply); r != "" {
		b.WriteString("AI 的回复：")
		b.WriteString(clipRunes(r, 400))
	}
	return b.String()
}

// memoryLLMCaller 把模型注册表包成 rag 需要的“问一次模型”函数。
func memoryLLMCaller(modelID, key string) rag.LLMCaller {
	return func(ctx context.Context, system, user string) (string, error) {
		if registry == nil {
			return "", fmt.Errorf("模型注册表未初始化")
		}
		if strings.TrimSpace(modelID) == "" {
			return "", fmt.Errorf("本轮没有可用模型")
		}
		return registry.Generate(ctx, modelID, key, llm.ChatRequest{Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		}})
	}
}

// saveAutoMemory 落这一轮的记忆。
//
// 触发条件（刻意保守，避免把每句寒暄都变成记忆）：
//   - 这一轮**调用过工具**（做过事，值得记），或
//   - 用户这句话本身够长（≥15 字，算一段有信息量的陈述）。
//
// 返回非空字符串表示**没存上**（原因），由调用方在对话里如实告知用户。
// 为什么要返回而不是只写日志：写入失败时用户完全无感，下一轮问起才会发现“助手忘了”，
// 而那时已经完全不知道出了什么事。
func saveAutoMemory(userID, modelID, key, userMsg string, trace []agentTraceEntry, reply string) (warn string) {
	if !autoMemoryEnabled() || strings.TrimSpace(userID) == "" {
		return "" // 开关关着/没用户：本轮本来就不打算记，不算失败
	}
	substantive := len([]rune(strings.TrimSpace(userMsg))) >= 15
	if len(trace) == 0 && !substantive {
		return "" // 寒暄类不记，属预期行为
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[memory] 自动记录发生意外（不影响对话）：%v", r)
		}
	}()

	ctx := context.Background()

	// 首选：抽成原子事实。
	facts, ferr := rag.ExtractFacts(ctx, memoryLLMCaller(modelID, key), userMsg, reply, traceSummary(trace))
	if ferr != nil {
		log.Printf("[memory] 事实抽取失败，降级为整轮记录：%v", ferr)
	} else if len(facts) > 0 {
		saved := 0
		for _, f := range facts {
			if _, err := kb().IngestFact(ctx, userID, "记忆："+clipRunes(f.Text, 40), f); err != nil {
				log.Printf("[memory] 写入事实失败（跳过该条）：%v", err)
				continue
			}
			saved++
		}
		if saved > 0 {
			log.Printf("[memory] 已抽取并记录 %d 条原子事实", saved)
			touchMemoryWatermark(userID)
			return ""
		}
		log.Printf("[memory] 事实全部写入失败，降级为整轮记录")
		warn = "抽取出的记忆一条都没存上"
	}

	// 降级：整轮拼接，保证“记忆不丢”。
	title := "对话记录：" + clipRunes(userMsg, 30)
	text := autoMemoryText(userMsg, trace, reply)
	res, err := kb().IngestText(ctx, userID, title, text)
	if err != nil {
		log.Printf("[memory] 自动记录失败（不影响对话）：%v", err)
		return "记忆未保存：" + err.Error()
	}
	log.Printf("[memory] 已自动记录本轮（整轮降级路径）：文档 id=%d，切块 %d 个（去重命中=%v）", res.DocumentID, res.ChunkCount, res.Duplicated)
	touchMemoryWatermark(userID)
	return warn
}

// 编译期确认 *rag.Ingester 满足 MemoryWriter（接口漂移时这里先报错）。
// memoryQuery 把最近几轮用户消息拼进检索查询，解决“那个 / 它”这类指代。
// 只取用户消息、每条限长：把整段历史塞进嵌入请求既慢，也会把当前这句的语义稀释掉。
func memoryQuery(content string, history []llm.Message) string {
	var recent []string
	for i := len(history) - 1; i >= 0 && len(recent) < 2; i-- {
		if history[i].Role != llm.RoleUser {
			continue
		}
		txt := strings.TrimSpace(history[i].Content)
		if txt == "" || txt == strings.TrimSpace(content) {
			continue // 当前这句由调用方传入，别重复
		}
		recent = append([]string{clipRunes(txt, 80)}, recent...)
	}
	if len(recent) == 0 {
		return content
	}
	return strings.Join(recent, " ") + " " + content
}

var _ rag.MemoryWriter = (*rag.Ingester)(nil)
