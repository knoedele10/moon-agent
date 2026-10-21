package web

import (
	"fmt"
	"net/http"
	"strings"

	"blog-my/internal/llm"
	"blog-my/internal/rag"

	"github.com/gin-gonic/gin"
)

// kbSystemPrompt 是 RAG 的系统提示。要求“只依据资料回答 + 标注出处”，
// 是为了让模型的回答可核查；资料不够时明确说没有，而不是编一个看起来很真的。
const kbSystemPrompt = "你是本地知识库的问答助手。下面会给出一组从知识库检索到的资料（编号 [1][2]…）。" +
	"请只依据这些资料回答用户的问题；如果资料不足以回答，就直接说“知识库中没有相关信息”，不要凭常识补充。" +
	"回答末尾用 [编号] 标出依据了哪几条资料。如果资料里包含图片描述或图片中的文字，也可以引用。"

// supportsVision 用模型名启发式判断能不能看图。
// 名字里带 vl / vision 的（qwen2.5vl 等）与 gemma3 系列确定支持；
// 其余按不支持处理 —— 宁可少传图，也不要把图片塞给文本模型把整个请求打报错。
func supportsVision(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "vl") || strings.Contains(m, "vision") || strings.Contains(m, "gemma3")
}

// kbAsk 检索 + 生成：先推一个 sources 事件，再流式推 delta —— 前端拿到出处后就能分两段渲染。
func kbAsk(c *gin.Context) {
	userID := currentUserID(c)

	var body struct {
		Query string `json:"query"`
		Model string `json:"model"`
		TopK  int    `json:"top_k"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query 不能为空"})
		return
	}

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	send := func(event string, payload any) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonMustMarshal(payload))
		flusher.Flush()
	}

	hits, err := kb().Search(c.Request.Context(), userID, body.Query, clampTopK(body.TopK))
	if err != nil {
		send("error", gin.H{"error": err.Error()})
		return
	}
	send("sources", gin.H{"hits": hits})

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: kbSystemPrompt},
		{Role: llm.RoleUser, Content: "资料：\n" + rag.BuildContext(hits, 6000) + "\n问题：" + body.Query},
	}

	// 命中图片且模型能看图时，把原图也带上：描述是给检索用的压缩信息，
	// 图片本身才是细节。最多带 3 张，否则上下文会灌爆而且显存也放不下。
	if supportsVision(body.Model) {
		var images []llm.Image
		for _, h := range hits {
			if len(images) >= 3 {
				break
			}
			if h.Kind != "image_caption" {
				continue
			}
			mime, data, ierr := kb().DocumentImage(c.Request.Context(), h.DocumentID, userID)
			if ierr != nil || len(data) == 0 {
				continue
			}
			images = append(images, llm.Image{MIME: mime, Data: data})
		}
		if len(images) > 0 {
			messages[len(messages)-1].Images = images
		}
	}

	ep, ok := registry.FindByModel(body.Model)
	if !ok {
		send("error", gin.H{"error": llm.ErrNoEndpoint.Error()})
		return
	}
	key := resolveKey(userID, ep)

	stream, err := registry.Stream(c.Request.Context(), body.Model, key, llm.ChatRequest{Messages: messages})
	if err != nil {
		send("error", gin.H{"error": err.Error()})
		return
	}

	for ev := range stream {
		switch ev.Type {
		case llm.EventDelta:
			send("delta", gin.H{"content": ev.Content})
		case llm.EventDone:
			send("done", gin.H{})
			return
		case llm.EventError:
			msg := "上游返回错误"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			send("error", gin.H{"error": msg})
			return
		}
	}
	send("error", gin.H{"error": "连接意外中断"})
}
