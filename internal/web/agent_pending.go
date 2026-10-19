package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 待确认工具调用的存取与「确认后直接执行」。
//
// 背景：confirm 档把风险工具包了一层壳，第一次调用只会停下来问用户。
// 用户点「确认」之后，**不能**指望模型自己把同一个调用重发一遍——实测
// qwen2.5:7b 收到「确认」后只会回一句「请提供具体函数和参数」，工具根本没跑，
// 用户点了确认却什么都没发生。所以这里把被拦下的那次调用原样存下来，
// 确认时直接执行它，结果再回灌给模型继续。

// roleToolMessage 是回灌给模型的工具结果消息角色（与 agent 包内的取值一致）。
const roleToolMessage = "tool"

// savePendingTool 记下一条待确认的风险调用（同一会话只保留最新一条）。
func savePendingTool(convID uint, userID string, call *llm.ToolCall) {
	if model.DB == nil || convID == 0 || call == nil {
		return
	}
	row := model.AgentPendingTool{
		ConversationID: convID,
		UserID:         userID,
		ToolID:         call.ID,
		ToolName:       call.Name,
		Arguments:      call.Arguments,
		CreatedAt:      time.Now(),
	}
	// 先删后建：同一会话只会有一条，避免主键冲突，也避免旧参数残留。
	if err := model.DB.Where("conversation_id = ?", convID).Delete(&model.AgentPendingTool{}).Error; err != nil {
		log.Printf("[confirm] 清理旧的待确认调用失败（继续写新的）：%v", err)
	}
	if err := model.DB.Create(&row).Error; err != nil {
		log.Printf("[confirm] 保存待确认调用失败（不影响对话）：%v", err)
	}
}

// takePendingTool 取出并删除某会话的待确认调用；没有就返回 nil。
// 「取出即删除」保证一次确认只执行一次——重复点确认不会把删除/覆盖跑两遍。
func takePendingTool(convID uint) *model.AgentPendingTool {
	if model.DB == nil || convID == 0 {
		return nil
	}
	var rows []model.AgentPendingTool
	if err := model.DB.Where("conversation_id = ?", convID).Limit(1).Find(&rows).Error; err != nil || len(rows) == 0 {
		return nil
	}
	if err := model.DB.Where("conversation_id = ?", convID).Delete(&model.AgentPendingTool{}).Error; err != nil {
		log.Printf("[confirm] 删除待确认调用失败（仍继续执行，避免卡死）：%v", err)
	}
	return &rows[0]
}

// clearPendingTool 丢掉某会话的待确认调用（用户拒绝了这次操作时用）。
func clearPendingTool(convID uint) {
	if model.DB == nil || convID == 0 {
		return
	}
	model.DB.Where("conversation_id = ?", convID).Delete(&model.AgentPendingTool{})
}

// isRejection 判断用户是不是明确拒绝了刚才那次操作。
// 只在「上一条 assistant 是确认请求」的前提下才看这个判断，避免把日常对话里的
// 「不要」误当成拒绝。
func isRejection(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" || len([]rune(m)) > 12 {
		return false
	}
	for _, w := range []string{"取消", "不要", "不用", "算了", "停止", "别", "否", "no", "cancel", "abort", "stop"} {
		if m == w || strings.HasPrefix(m, w) {
			return true
		}
	}
	return false
}

// runPendingTool 执行一条已经获得用户确认的风险工具调用。
//
// 返回两条要追加进历史的消息 [assistant(tool_calls), tool(result)]，
// 以及一条写进轨迹的记录（轨迹会喂给记忆抽取器，让它知道「这轮真做了什么」）。
// 工具不存在（例如工作区被改、技能被删）时返回 nil——调用方据此保持原样继续。
func runPendingTool(
	ctx context.Context,
	userID, root, mode string,
	p *model.AgentPendingTool,
	sendEvent func(string, any),
	roots ...string,
) ([]llm.Message, *agentTraceEntry) {
	if p == nil {
		return nil, nil
	}
	// confirmed=true 拿到的是**没有确认壳**的真工具。
	// 深度 0：待确认调用的重放也是用户会话里的根轮次（子 agent 不走这条路）。
	reg := agent.NewRegistry(agentToolsFor(userID, root, mode, true, 0, roots...)...)
	tool, ok := reg.Lookup(p.ToolName)
	if !ok {
		sendEvent("tool_result", gin.H{
			"id": p.ToolID, "name": p.ToolName, "ok": false,
			"summary": "工具已不可用（工作区或技能可能已变）",
		})
		return nil, nil
	}

	call := llm.ToolCall{ID: p.ToolID, Name: p.ToolName, Arguments: p.Arguments}
	// 有些后端（实测 Ollama）不给 tool_call 发 id。空 id 在 OpenAI 系后端上会让
	// 「assistant.tool_calls[i].id ↔ tool.tool_call_id」对不上而被整条请求拒掉，
	// 所以缺了就补一个确定性的。两边用的是同一个值，不会错配。
	if strings.TrimSpace(call.ID) == "" {
		call.ID = fmt.Sprintf("call_%d_%d", p.ConversationID, time.Now().UnixNano())
	}
	sendEvent("tool_call", gin.H{
		"id": call.ID, "name": call.Name, "arguments": toolCallArguments(call.Arguments),
	})

	ex := &agent.Exec{
		ReadRoot:  agentSkillsDir(),
		SessionID: fmt.Sprintf("pending-%d", p.ConversationID),
		Root:      root,
		Roots:     append([]string(nil), roots...),
		// 重放的是**用户已经确认过**的调用，可能落在工作区外：
		// fs 层要放行外部绝对路径，否则用户点了确认也会被沙箱拒掉。
		AllowOutside: true,
	}
	res, err := tool.Execute(ctx, json.RawMessage(p.Arguments), ex)
	if err != nil {
		// 工具报错不回滚：折成 is_error 结果回灌，让模型自己决定重试还是换做法。
		res = agent.Result{Content: "工具执行失败：" + err.Error(), IsError: true}
	}

	okRun := !res.IsError
	summary := summarizeToolResult(call.Name, res)
	sendEvent("tool_result", gin.H{
		"id": call.ID, "name": call.Name, "ok": okRun,
		"summary": summary, "detail": res.Content, "display": res.Display,
	})

	// 给注入的这条 assistant 消息配一句说明：模型看到的是一个「它自己没发过的」工具调用，
	// 不加说明时小模型容易一头雾水（实测 qwen2.5:7b 会反问“到底哪个函数被调用了”）。
	msgs := []llm.Message{
		{
			Role:      llm.RoleAssistant,
			Content:   fmt.Sprintf("（用户已确认，正在执行 %s）", call.Name),
			ToolCalls: []llm.ToolCall{call},
		},
		{Role: roleToolMessage, Content: res.Content, ToolCallID: call.ID, ToolName: call.Name},
	}
	entry := &agentTraceEntry{Name: call.Name, OK: okRun, Summary: summary, Detail: res.Content}
	return msgs, entry
}

// summarizeToolResult 给工具结果生成一行摘要（前端卡片与轨迹都用它）。
// 取正文第一行、截断到一行能放下的长度；空结果给个明确说法而不是留白。
func summarizeToolResult(name string, res agent.Result) string {
	line := strings.TrimSpace(res.Content)
	if line == "" {
		if res.IsError {
			return name + "：失败（无输出）"
		}
		return name + "：完成"
	}
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return clipRunes(line, 160)
}
