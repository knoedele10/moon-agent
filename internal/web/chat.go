package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

type conversationItem struct {
	ID        uint   `json:"id"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	UpdatedAt string `json:"updated_at"`
	// 子 agent 派出去的会话在侧栏里缩进挂在父会话下面，所以列表要带父信息。
	// 父关系存在 agent_subagent_run 里，没有加在 conversation 表上（那是全站共享表）。
	ParentID    uint   `json:"parent_id"`
	ParentTitle string `json:"parent_title"`
}

type messageItem struct {
	ID        uint   `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func titleFromContent(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) == 0 {
		return "新对话"
	}
	if len(r) > 30 {
		r = r[:30]
	}
	return string(r)
}

func listConversations(c *gin.Context) {
	userID := currentUserID(c)
	var rows []model.Conversation
	// 侧栏只用这四列。Summary 是长文本，随会话数增长会拖慢每次打开页面。
	model.DB.Select("id", "title", "model", "updated_at").Where("user_id = ?", userID).Order("updated_at desc").Find(&rows)
	backfillPlaceholderTitles(userID, rows)

	// 子会话的父关系一次查完、在内存里建索引：列表页每行查一次数据库是没必要的。
	parentOf := map[uint]uint{}
	titles := make(map[uint]string, len(rows))
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		titles[r.Id] = r.Title
		ids = append(ids, r.Id)
	}
	if len(ids) > 0 {
		var runs []model.AgentSubagentRun
		// 子任务 prompt / reply 也可能很长，列表只需要父子关系。
		model.DB.Select("child_conversation_id", "parent_conversation_id").Where("user_id = ? AND child_conversation_id IN ?", userID, ids).Find(&runs)
		for _, run := range runs {
			parentOf[run.ChildConversationID] = run.ParentConversationID
		}
	}

	items := make([]conversationItem, 0, len(rows))
	for _, r := range rows {
		item := conversationItem{
			ID:        r.Id,
			Title:     r.Title,
			Model:     r.Model,
			UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
		}
		if pid := parentOf[r.Id]; pid != 0 {
			item.ParentID = pid
			item.ParentTitle = titles[pid]
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, items)
}

// backfillPlaceholderTitles repairs conversations created by the browser's
// eager-create flow before automatic first-message titles were persisted.
// It only touches the default placeholder and leaves manually named chats as-is.
func backfillPlaceholderTitles(userID string, rows []model.Conversation) {
	ids := make([]uint, 0)
	for _, row := range rows {
		if strings.TrimSpace(row.Title) == "" || strings.TrimSpace(row.Title) == "新对话" {
			ids = append(ids, row.Id)
		}
	}
	if len(ids) == 0 {
		return
	}
	var messages []model.Message
	if err := model.DB.Select("id", "conversation_id", "content").
		Where("conversation_id IN ? AND role = ?", ids, "user").
		Order("id asc").Find(&messages).Error; err != nil {
		fmt.Printf("[chat] 查询默认标题会话的首条消息失败: %v\n", err)
		return
	}
	firstByConversation := make(map[uint]string, len(ids))
	for _, message := range messages {
		if _, exists := firstByConversation[message.ConversationID]; !exists {
			firstByConversation[message.ConversationID] = message.Content
		}
	}
	for i := range rows {
		if strings.TrimSpace(rows[i].Title) != "" && strings.TrimSpace(rows[i].Title) != "新对话" {
			continue
		}
		content, exists := firstByConversation[rows[i].Id]
		if !exists {
			continue
		}
		title := titleFromContent(content)
		if title == "新对话" {
			continue
		}
		updated := model.DB.Model(&model.Conversation{}).
			Where("id = ? AND user_id = ? AND (title = '' OR title = ?)", rows[i].Id, userID, "新对话").
			UpdateColumn("title", title)
		if updated.Error == nil && updated.RowsAffected > 0 {
			rows[i].Title = title
		}
	}
}

func createConversation(c *gin.Context) {
	userID := currentUserID(c)

	var body struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	conv := model.Conversation{UserID: userID, Title: "新对话", Model: body.Model}
	if err := model.DB.Create(&conv).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建会话失败"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": conv.Id, "title": conv.Title, "model": conv.Model})
}

func listMessages(c *gin.Context) {
	userID := currentUserID(c)

	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}

	var rows []model.Message
	query := model.DB.WithContext(c.Request.Context()).Where("conversation_id = ?", conv.Id)
	// 保留旧数组契约；新客户端用游标只取最近一页，避免长对话全量传输。
	paged := c.Query("limit") != ""
	limit := 60
	if paged {
		if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 {
			limit = n
		}
		if limit > 200 {
			limit = 200
		}
		if before, err := strconv.ParseUint(c.Query("before"), 10, 64); err == nil && before > 0 {
			query = query.Where("id < ?", before)
		}
		query = query.Order("id desc").Limit(limit + 1)
	} else {
		query = query.Order("id asc")
	}
	if err := query.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取历史失败"})
		return
	}
	hasMore := paged && len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if paged {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}

	items := make([]messageItem, 0, len(rows))
	for _, m := range rows {
		items = append(items, messageItem{
			ID:        m.Id,
			Role:      m.Role,
			Content:   m.Content,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
		})
	}
	if paged {
		var oldest uint
		if len(items) > 0 {
			oldest = items[0].ID
		}
		c.JSON(http.StatusOK, gin.H{"messages": items, "has_more": hasMore, "before": oldest})
		return
	}
	c.JSON(http.StatusOK, items)
}

func deleteConversation(c *gin.Context) {
	userID := currentUserID(c)

	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}

	model.DB.Where("conversation_id = ?", conv.Id).Delete(&model.Message{})
	model.DB.Delete(&conv)
	// 计划只活在进程内：会话删了就顺手清掉，避免同名 id 复用时串到新会话。
	agent.ClearPlan(strconv.FormatUint(uint64(conv.Id), 10))
	c.Status(http.StatusNoContent)
}

func postChat(c *gin.Context) {
	userID := currentUserID(c)

	var body struct {
		ConversationID uint   `json:"conversation_id"`
		Model          string `json:"model"`
		Content        string `json:"content"`
		// Tools 开启 AI 自主工具调用（契约 §5）。**缺省 false**：
		// 不传时走下面那条老路径，事件序列与改造前一个字节都不差。
		Tools bool `json:"tools"`
		// AssistantID 是本轮选中的自定义助手（0 = 不使用，全部走用户全局设置）。
		// 助手只会覆盖它显式填了的字段，详见 web/assistant_resolve.go。
		AssistantID   uint   `json:"assistant_id"`
		ExecutionMode string `json:"execution_mode"`
		// 以下三个是「对话参数」（可选，契约见 web/chat_params.go）。
		// 全不传时归一化成零值，生成行为与改造前完全一致。
		SystemPrompt string  `json:"system_prompt"`
		Temperature  float64 `json:"temperature"`
		MaxTokens    int     `json:"max_tokens"`
		// Kind 是本轮的会话模式：chat / work / code / learn。空串 = work（不变旧行为）。
		// 它会被记到会话上，所以切走再回来行为一致。
		Kind string `json:"kind"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	// 把助手记进请求上下文：runAgentChat 会据此解析本轮生效的灵魂/档位/工作区。
	// 放在这里而不是传参，是为了不改 runAgentChat 的签名（它被多处调用）。
	setAssistantID(c, body.AssistantID)
	if !setExecutionMode(c, body.ExecutionMode) {
		return
	}
	// 同理，把对话参数归一化后记进上下文；streamReply / runAgentChat 会从那里取。
	setChatParams(c, normalizeChatParams(chatParams{
		SystemPrompt: body.SystemPrompt,
		Temperature:  body.Temperature,
		MaxTokens:    body.MaxTokens,
	}))
	if strings.TrimSpace(body.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "内容不能为空"})
		return
	}

	var conv model.Conversation
	if body.ConversationID == 0 {
		conv = model.Conversation{UserID: userID, Title: titleFromContent(body.Content), Model: body.Model, Kind: normalizeKind(body.Kind)}
		if err := model.DB.Create(&conv).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建会话失败"})
			return
		}
	} else {
		if err := model.DB.Where("id = ? AND user_id = ?", body.ConversationID, userID).First(&conv).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
			return
		}
		if body.Model != "" {
			conv.Model = body.Model
			model.DB.Save(&conv)
		}
		// 显式传了模式且与库里不同 → 更新（切换模式后本轮立即生效）。
		if body.Kind != "" {
			if k := normalizeKind(body.Kind); k != conv.Kind {
				conv.Kind = k
				model.DB.Save(&conv)
			}
		}
	}

	// 模式取自**会话本身**（不是请求），这样客户端不传时行为也不会漂移。
	setConversationKind(c, conv.Kind)
	if err := model.DB.Create(&model.Message{
		ConversationID: conv.Id,
		Role:           "user",
		Content:        body.Content,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存消息失败"})
		return
	}
	// The browser creates a conversation before opening the stream so it can
	// associate messages with an ID. In that common path, the conversation was
	// created with the placeholder title and the no-ID title branch is skipped.
	// Fill it from the first user message, including older placeholder chats.
	if strings.TrimSpace(conv.Title) == "" || strings.TrimSpace(conv.Title) == "新对话" {
		var firstUser model.Message
		if err := model.DB.WithContext(c.Request.Context()).
			Where("conversation_id = ? AND role = ?", conv.Id, "user").
			Order("id asc").First(&firstUser).Error; err == nil {
			if title := titleFromContent(firstUser.Content); title != "新对话" {
				updated := model.DB.WithContext(c.Request.Context()).Model(&model.Conversation{}).
					Where("id = ? AND user_id = ? AND (title = '' OR title = ?)", conv.Id, userID, "新对话").
					Update("title", title)
				if updated.Error != nil {
					fmt.Printf("[chat] 自动设置会话标题失败 conversation_id=%d: %v\n", conv.Id, updated.Error)
				} else if updated.RowsAffected > 0 {
					conv.Title = title
				}
			}
		}
	}

	var history []model.Message
	model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&history)

	// SSE 建流 + 跑生成抽成共用 helper：/api/chat 与 /api/conversations/:id/regenerate
	// 走同一段代码，事件帧格式与顺序天然一致。
	streamReply(c, &conv, userID, body.Model, body.Content, rebuildLLMHistory(history), body.Tools)
}

// rebuildLLMHistory 把库里的消息重建成喂给模型的对话历史。
//
// tool_trace 是工具调用轨迹，Detail 可能很长，整条喂回模型会把它上一次的工具输出
// 当成对话正文读进去。所以这里只注入一行摘要（丢弃 Detail、只留最近 N 轮），
// 让模型跨轮还能知道自己调过什么工具，又不至于把上下文吃掉。
// 抽成纯函数（不碰 DB）是为了让这段兼容性有单测钉住。
func rebuildLLMHistory(history []model.Message) []llm.Message {
	out := make([]llm.Message, 0, len(history))
	// 从后往前数出「最近 N 轮」的轨迹行；N<=0 时集合为空（一条都不注入）。
	keep := recentTraceIndexes(history, traceNoteTurns())
	for i, m := range history {
		if m.Role == roleToolTrace {
			// 整条丢掉会让模型跨轮失忆（看不到自己上一轮调过什么工具），
			// 所以压成一行摘要注入；但只注入最近 N 轮的轨迹（见 agent_history.go），
			// 否则每轮一条注记，长会话里注记本身就会把上下文吃掉。
			if keep[i] {
				if note := toolTraceNote(m.Content); note != "" {
					// 用 role=system 而不是 role=user：这是运行时给模型的旁注，不是用户说的话。
					// 冒充 user 会让模型以为用户提过这件事（Cyrene 文档专门警告过：
					// Runtime Feedback 不伪装成 role:user）。
					out = append(out, llm.Message{Role: llm.RoleSystem, Content: note})
				}
			}
			continue
		}
		role := llm.RoleUser
		if m.Role == roleSubagent {
			// 子 agent 的完成通知：它是运行时投回来的外部事件，既不是用户说的话，
			// 也不是助手自己的输出。参考实现（deepseek-harness）同样用 user 角色承载它
			// （subagent-settled notice 就是一条 user 消息），这里保持一致。
			out = append(out, llm.Message{Role: llm.RoleUser, Content: m.Content})
			continue
		}
		switch m.Role {
		case "assistant":
			role = llm.RoleAssistant
		case "system":
			role = llm.RoleSystem
		}
		out = append(out, llm.Message{Role: role, Content: m.Content})
	}
	return out
}

// directChatRequest 装配直连路径（非 agent）要发给模型的请求：
// 用户额外系统提示词插在历史最前面（用「前插」语义，不覆盖已有的 system），
// 温度 / 最大输出长度照抄请求级参数。参数全为零值时与改造前逐字节一致
// （返回原 history 切片，Temperature=0、MaxTokens=0 都是「不设置」）。
// 纯函数，不碰依赖，便于单测钉住这三条。
func directChatRequest(history []llm.Message, p chatParams) llm.ChatRequest {
	req := llm.ChatRequest{
		Messages:    history,
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
	}
	if strings.TrimSpace(p.SystemPrompt) != "" {
		// 前插一条独立 system 消息：已有的 system（若有）保持原样跟在后面。
		req.Messages = append([]llm.Message{{Role: llm.RoleSystem, Content: p.SystemPrompt}}, history...)
	}
	return req
}

// sseResponder 负责一次 SSE 响应的头部与事件写出。
// 用值类型而不是闭包：send 可以直接当 func(string, any) 传给 runAgentChat。
type sseResponder struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// send 写一帧 SSE 并立刻 flush。序列化失败在 gin.H 负载下不会发生；
// 真失败也无从补救，保持与改造前一致：忽略错误、继续写。
func (s *sseResponder) send(event string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.flusher.Flush()
}

// startSSE 设好 SSE 响应头并返回写出器；响应写入器不支持 Flush 时返回 nil。
func startSSE(c *gin.Context) *sseResponder {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &sseResponder{w: w, flusher: flusher}
}

// streamReply 是 /api/chat 与 /api/conversations/:id/regenerate 共用的生成路径：
// 建 SSE → 按 shouldUseAgent 分 agent 循环 / 直连流式 → assistant 消息落库并 touch 会话。
//
// modelID 同时用于端点查找与上游调用（caller 传什么就是什么，不在内部回落）；
// content 是触发记忆检索的当前用户文本（只有 agent 分支会用到）。
func streamReply(
	c *gin.Context,
	conv *model.Conversation,
	userID, modelID, content string,
	history []llm.Message,
	toolsRequested bool,
) {
	sse := startSSE(c)
	if sse == nil {
		return
	}

	ep, ok := registry.FindByModel(modelID)
	if !ok {
		sse.send("error", gin.H{"error": llm.ErrNoEndpoint.Error()})
		return
	}
	key := resolveKey(userID, ep)

	// AI 自主工具调用分支（契约 §5）。只有显式要工具 **且**服务端开了
	// AGENT_TOOLS 才走这里；否则走下面那条老路径，行为完全不变。
	if shouldUseAgent(toolsRequested) {
		// 请求级配置（助手 / 会话模式 / 对话参数）都在 gin.Context 里，一次折进 turnInput。
		in := turnInputFromGin(c)
		in.conv = conv
		in.userID = userID
		in.modelID = modelID
		in.key = key
		in.content = content
		in.history = history
		in.sendEvent = sse.send
		runAgentChat(in)
		return
	}

	// 传 c.Request.Context()：浏览器断开时上游请求随之取消。
	// 对话参数只作用于直连路径；agent 路径在 runAgentChat 内部从上下文取。
	stream, err := registry.Stream(c.Request.Context(), modelID, key, directChatRequest(history, chatParamsFromContext(c)))
	if err != nil {
		sse.send("error", gin.H{"error": err.Error()})
		return
	}

	var acc strings.Builder
	for ev := range stream {
		switch ev.Type {
		case llm.EventDelta:
			acc.WriteString(ev.Content)
			sse.send("delta", gin.H{"content": ev.Content})
		case llm.EventDone:
			if reply := acc.String(); reply != "" {
				saveChatMessage(conv.Id, "assistant", reply)
			}
			touchConversation(conv.Id)
			sse.send("done", gin.H{})
			return
		case llm.EventError:
			// 已累积的部分照样落库（方便排查），但不更新 updated_at/标题。
			if partial := acc.String(); partial != "" {
				saveChatMessage(conv.Id, "assistant", partial)
			}
			msg := "上游返回错误"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			sse.send("error", gin.H{"error": msg})
			return
		}
	}

	sse.send("error", gin.H{"error": "连接意外中断"})
}
