package web

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 本文件是「会话 / 消息编辑」类接口：重命名会话、删消息、编辑消息（截断后续）、
// 重新生成最后一条回复、导出 Markdown。只提供 handler，路由由主 agent 在
// web.go 里统一接线：
//
//	PATCH  /api/conversations/:id            -> patchConversation
//	DELETE /api/messages/:id                 -> deleteMessage
//	PATCH  /api/messages/:id                 -> patchMessage
//	POST   /api/conversations/:id/regenerate -> regenerate
//	GET    /api/conversations/:id/export     -> exportConversation

// conversationTitleMaxRunes 是会话标题的长度上限（按 rune 计，中文不被截半个字）。
const conversationTitleMaxRunes = 60

// normalizeConversationTitle 去空格并按 rune 截到 60。空标题返回 ok=false，
// 调用方据此报 400，而不是把一个空标题写进库。
func normalizeConversationTitle(raw string) (string, bool) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", false
	}
	return truncateRunes(title, conversationTitleMaxRunes), true
}

// findOwnedConversation 查一条属于当前用户的会话；查不到时已经写好 404，返回 nil。
// 调用前必须先判 model.DB != nil。
func findOwnedConversation(c *gin.Context, userID string) *model.Conversation {
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return nil
	}
	return &conv
}

// findOwnedMessage 查一条属于当前用户的消息。
//
// 归属判断必须走「消息所在会话的 user_id」，不能只看 message.id：否则拿到别人的
// 消息 id 就能删/改。别人的消息一律按 404 处理，不泄露「这个 id 存在」。
// 调用前必须先判 model.DB != nil。
func findOwnedMessage(c *gin.Context, userID string) *model.Message {
	var msg model.Message
	if err := model.DB.Where("id = ?", c.Param("id")).First(&msg).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "消息不存在"})
		return nil
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", msg.ConversationID, userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "消息不存在"})
		return nil
	}
	return &msg
}

// patchConversation 重命名会话：PATCH /api/conversations/:id，body {"title":"..."}。
func patchConversation(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)

	var body struct {
		Title string `json:"title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	title, ok := normalizeConversationTitle(body.Title)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "标题不能为空"})
		return
	}

	conv := findOwnedConversation(c, userID)
	if conv == nil {
		return
	}
	if err := model.DB.Model(&model.Conversation{}).Where("id = ?", conv.Id).Update("title", title).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存会话失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "title": title})
}

// deleteMessage 删除单条消息：DELETE /api/messages/:id。成功返回 204。
func deleteMessage(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	msg := findOwnedMessage(c, currentUserID(c))
	if msg == nil {
		return
	}
	if err := model.DB.Where("id = ?", msg.Id).Delete(&model.Message{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除消息失败"})
		return
	}
	c.Status(http.StatusNoContent)
}

// patchMessage 编辑一条 user 消息并截断它之后的所有消息：
// PATCH /api/messages/:id，body {"content":"..."}。
//
// 只允许改 role="user"：改 assistant 会让喂回模型的历史自相矛盾。
func patchMessage(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "内容不能为空"})
		return
	}

	msg := findOwnedMessage(c, currentUserID(c))
	if msg == nil {
		return
	}
	if msg.Role != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能编辑用户消息"})
		return
	}

	if err := model.DB.Model(&model.Message{}).Where("id = ?", msg.Id).Update("content", body.Content).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存消息失败"})
		return
	}
	res := model.DB.Where("conversation_id = ? AND id > ?", msg.ConversationID, msg.Id).Delete(&model.Message{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除后续消息失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": msg.Id, "removed": res.RowsAffected})
}

// regenerateHistory 计算「重新生成」要喂给模型的历史：
// 保留到最后一条 user 消息为止，丢弃它之后的所有消息（assistant / tool_trace）。
//
// 没有 user 消息时返回空的 kept——调用方据此报错，而不是拿空历史去跑。
// 纯函数，不碰 DB，便于单测。
func regenerateHistory(msgs []model.Message) (kept, dropped []model.Message) {
	cut := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			cut = i
			break
		}
	}
	if cut < 0 {
		return nil, msgs
	}
	if cut+1 >= len(msgs) {
		return msgs, nil
	}
	return msgs[:cut+1], msgs[cut+1:]
}

// regenerate 重新生成最后一条回复：POST /api/conversations/:id/regenerate。
//
// 不插入新的 user 消息；把旧回复（最后一条 user 消息之后的 assistant / tool_trace）
// 从库里删掉后再跑，避免刷新页面后同时看到新旧两条回复。events 与 /api/chat 一致。
func regenerate(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)

	var body struct {
		Model string `json:"model"`
		// Tools 可选：不传时默认按“要工具”处理（与前端 /api/chat 一直带 tools=true 一致），
		// 服务端没开 AGENT_TOOLS 时 shouldUseAgent 自然回落到直连路径。
		Tools *bool `json:"tools"`
		// AssistantID 是本轮选中的自定义助手（0 = 不使用）。重新生成也走同一套解析，
		// 否则用助手聊到一半点“重新生成”会突然变回全局人格。
		AssistantID   uint   `json:"assistant_id"`
		ExecutionMode string `json:"execution_mode"`
		// 以下三个是「对话参数」（可选，契约见 web/chat_params.go）。重新生成也带上，
		// 否则「重来一次」会丢掉用户刚设的额外提示词与采样参数。
		SystemPrompt string  `json:"system_prompt"`
		Temperature  float64 `json:"temperature"`
		MaxTokens    int     `json:"max_tokens"`
	}
	// body 可以为空（只用会话里存的 model），所以 EOF 不算格式错误。
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	setAssistantID(c, body.AssistantID)
	if !setExecutionMode(c, body.ExecutionMode) {
		return
	}
	setChatParams(c, normalizeChatParams(chatParams{
		SystemPrompt: body.SystemPrompt,
		Temperature:  body.Temperature,
		MaxTokens:    body.MaxTokens,
	}))

	conv := findOwnedConversation(c, userID)
	if conv == nil {
		return
	}

	modelID := strings.TrimSpace(body.Model)
	if modelID == "" {
		modelID = conv.Model
	} else if modelID != conv.Model {
		conv.Model = modelID
		if err := model.DB.Save(conv).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存会话失败"})
			return
		}
	}

	var history []model.Message
	if err := model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&history).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取消息失败"})
		return
	}
	kept, dropped := regenerateHistory(history)
	if len(kept) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可重新生成的消息"})
		return
	}
	if len(dropped) > 0 {
		lastKeptID := kept[len(kept)-1].Id
		if err := model.DB.Where("conversation_id = ? AND id > ?", conv.Id, lastKeptID).
			Delete(&model.Message{}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除旧回复失败"})
			return
		}
	}

	// kept 的最后一条一定是 user 消息（regenerateHistory 的定义），用它做记忆检索的触发文本。
	content := kept[len(kept)-1].Content
	toolsRequested := true
	if body.Tools != nil {
		toolsRequested = *body.Tools
	}
	streamReply(c, conv, userID, modelID, content, rebuildLLMHistory(kept), toolsRequested)
}

// exportConversation 导出会话为 Markdown：GET /api/conversations/:id/export。
func exportConversation(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	conv := findOwnedConversation(c, currentUserID(c))
	if conv == nil {
		return
	}

	var msgs []model.Message
	if err := model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&msgs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取消息失败"})
		return
	}

	markdown := conversationExportMarkdown(*conv, msgs, time.Now())
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"chat-%d.md\"", conv.Id))
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", []byte(markdown))
}

// conversationExportMarkdown 把会话与消息拼成 Markdown。
//
// role=tool_trace 是内部工具轨迹，绝不导出；其余消息按角色分节，
// 内容原样输出（不转义、不加工）。纯函数，便于单测。
func conversationExportMarkdown(conv model.Conversation, msgs []model.Message, now time.Time) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(conv.Title)
	b.WriteString("\n\n> 模型：")
	b.WriteString(conv.Model)
	b.WriteString(" · 导出时间：")
	b.WriteString(now.Format(time.RFC3339))
	b.WriteString("\n")
	for _, m := range msgs {
		if m.Role == roleToolTrace {
			continue
		}
		b.WriteString("\n## ")
		b.WriteString(exportRoleHeading(m.Role))
		b.WriteString("\n\n")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// exportRoleHeading 把消息角色映射成导出正文里的分节标题。
func exportRoleHeading(role string) string {
	switch role {
	case "user":
		return "用户"
	case "assistant":
		return "助手"
	case "system":
		return "系统"
	default:
		return role
	}
}
