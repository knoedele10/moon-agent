package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
	"blog-my/internal/rag"

	"github.com/gin-gonic/gin"
)

// kbMaxUploadBytes 限制单个上传文件大小。
// 20MB 对长文与截图都够用，又能避免一个巨大请求体把内存吃光。
const kbMaxUploadBytes = 20 << 20

var (
	kbOnce sync.Once
	kbIng  *rag.Ingester
)

// kb 懒加载知识库实例：真正用到时才连本地模型，
// 这样没装 / 没启 Ollama 的机器上服务依旧能正常启动与登录。
func kb() *rag.Ingester {
	kbOnce.Do(func() {
		cfg := rag.OllamaConfig{
			BaseURL:     os.Getenv("OLLAMA_BASE_URL"),
			EmbedModel:  os.Getenv("KB_EMBED_MODEL"),
			VisionModel: os.Getenv("KB_VISION_MODEL"),
		}
		kbIng = rag.NewIngester(rag.NewStore(model.DB), rag.NewOllamaEmbedder(cfg), rag.NewOllamaVision(cfg))
	})
	return kbIng
}

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

// kbListDocuments 列出知识库文档（只取元信息，不读 content/向量），支持按来源过滤。
//
// 这里直接查 model.DB 而不复用 rag.Store.ListDocuments：后者没有来源过滤参数，
// 而 rag/ 目录不属于本任务的改动范围，把过滤逻辑留在本文件里。
// 排序、上限与公共文档（user_id 为空串）的语义都与 ListDocuments 保持一致。
func kbListDocuments(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}

	source := kbSourceFilter(c.Query("source"))
	scope := kbScopeFilter(c.Query("scope"))

	q := model.DB.WithContext(c.Request.Context()).
		Model(&model.KBDocument{}).
		Select("id, user_id, kind, title, source, COALESCE(scope, '') AS scope, mime, bytes, sha256, chunk_count, created_at").
		Order("id desc").Limit(200)
	// 与 rag.Store.ListDocuments 一致：除了自己的文档，还能看到 user_id='' 的公共文档。
	if userID := currentUserID(c); userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	switch source {
	case "memory":
		q = q.Where("source = ?", "memory")
	case "document":
		// 含用户上传/粘贴的资料与系统生成的「用户画像」（source='profile'）。
		q = q.Where("source <> ?", "memory")
	}
	// scope 过滤：旧数据可能是 NULL 或空串，都归为 unclassified（未分类）。
	switch scope {
	case "life":
		q = q.Where("COALESCE(scope, '') = ?", "life")
	case "work":
		q = q.Where("COALESCE(scope, '') = ?", "work")
	case "unclassified":
		q = q.Where("COALESCE(scope, '') = ''")
	}

	var docs []model.KBDocument
	if err := q.Find(&docs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取知识库失败：" + err.Error()})
		return
	}

	items := make([]gin.H, 0, len(docs))
	for _, d := range docs {
		items = append(items, gin.H{
			"id":    d.Id,
			"kind":  d.Kind,
			"title": d.Title,
			// source 原样回传：'' = 普通资料，'memory' = AI 记忆，'profile' = 用户画像。
			// 前端据此显示徽标、决定删除文案。
			"source": d.Source,
			// scope 与 source 并列回传：'' = 未分类（含旧数据），'life' = 生活/陪伴，'work' = 项目/工作。
			"scope":       d.Scope,
			"mime":        d.Mime,
			"bytes":       d.Bytes,
			"chunk_count": d.ChunkCount,
			"created_at":  d.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true,
		// total 是本次过滤后返回的条数（= len(data)），供前端显示计数。
		"total":  len(items),
		"source": source,
		"scope":  scope,
		"data":   items,
	})
}

func kbCreateText(c *gin.Context) {
	var body struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	res, err := kb().IngestText(c.Request.Context(), currentUserID(c), body.Title, body.Text)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 内容重复时返回 200 并带上已有文档 id，让调用方能直接沿用而不是又存一份。
	status := http.StatusCreated
	if res.Duplicated {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{
		"ok": true, "id": res.DocumentID, "kind": res.Kind,
		"title": res.Title, "chunk_count": res.ChunkCount, "duplicated": res.Duplicated,
	})
}

// 按扩展名判定文档类型。不用 MIME 头是因为浏览器上传时给的 type 经常是
// application/octet-stream，而扩展名是用户真实看到的东西。
var kbImageExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif",
}

var kbTextExt = map[string]string{
	".txt": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8", ".log": "text/plain; charset=utf-8",
}

func kbUploadDocument(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, kbMaxUploadBytes)
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件字段 file（multipart/form-data）"})
		return
	}

	name := filepath.Base(fh.Filename)
	ext := strings.ToLower(filepath.Ext(name))

	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败：" + err.Error()})
		return
	}

	ctx := c.Request.Context()
	userID := currentUserID(c)

	var res rag.Result
	switch {
	case kbImageExt[ext] != "":
		res, err = kb().IngestImage(ctx, userID, name, kbImageExt[ext], name, data)
	case kbTextExt[ext] != "":
		res, err = kb().IngestText(ctx, userID, name, string(data))
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持的文件类型 %q（先支持 .txt/.md 与 .png/.jpg/.webp/.gif）", ext)})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	status := http.StatusCreated
	if res.Duplicated {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{
		"ok": true, "id": res.DocumentID, "kind": res.Kind,
		"title": res.Title, "chunk_count": res.ChunkCount, "duplicated": res.Duplicated,
	})
}

func kbGetDocument(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 非法"})
		return
	}

	doc, chunks, err := kb().GetDocument(c.Request.Context(), uint(id), currentUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取文档失败：" + err.Error()})
		return
	}
	if doc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文档不存在"})
		return
	}

	items := make([]gin.H, 0, len(chunks))
	for _, ch := range chunks {
		items = append(items, gin.H{
			"seq": ch.Seq, "kind": ch.Kind, "text": ch.Text, "dim": ch.Dim,
		})
	}
	data := gin.H{
		"id": doc.Id, "kind": doc.Kind, "title": doc.Title, "mime": doc.Mime,
		"bytes": doc.Bytes, "chunk_count": doc.ChunkCount,
		"created_at": doc.CreatedAt.Format(time.RFC3339),
		"chunks":     items,
	}
	// 没有切块的纯文本文档（例如自动生成的「用户画像」）正文只存在 content 列里，
	// 不回传的话，前端点开这篇文档就是一片空白。
	if len(chunks) == 0 && strings.HasPrefix(doc.Mime, "text/") {
		data["content"] = string(doc.Content)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "data": data})
}

func kbDeleteDocument(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 非法"})
		return
	}

	ok, err := kb().DeleteDocument(c.Request.Context(), uint(id), currentUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败：" + err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "文档不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": id, "deleted": true})
}

func kbStats(c *gin.Context) {
	st, err := kb().Stats(c.Request.Context(), currentUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "统计失败：" + err.Error()})
		return
	}
	doc, mem, prof := kbCountBySource(c)
	life, work, unclassified := kbCountByScope(c)
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "documents": st.Documents, "chunks": st.Chunks,
		// by_source 的口径见 kbCountBySource；三个数之和恒等于 documents。
		"by_source": gin.H{"document": doc, "memory": mem, "profile": prof},
		// by_scope 的口径见 kbCountByScope；life/work/unclassified 三者之和恒等于 documents。
		"by_scope": gin.H{"life": life, "work": work, "unclassified": unclassified},
	})
}

// kbCountBySource 按来源统计当前用户的文档条数，口径与前端分段控件一一对应：
//
//	document = source <> 'memory' 且 source <> 'profile'（普通资料，含 source=''）
//	memory   = source = 'memory'（对话里自动抽取的记忆）
//	profile  = source = 'profile'（系统定期汇总的「用户画像」，每个用户只有一条）
//
// 用一条聚合 SQL 算完三个数，避免三条 Count 之间出现不一致；user_id 过滤与 kbListDocuments 一致。
// 出错（例如 DB 未初始化）时降级为 0，不影响 documents/chunks 本身。
func kbCountBySource(c *gin.Context) (int64, int64, int64) {
	if model.DB == nil {
		return 0, 0, 0
	}
	var row struct {
		Document int64 `gorm:"column:n_document"`
		Memory   int64 `gorm:"column:n_memory"`
		Profile  int64 `gorm:"column:n_profile"`
	}
	q := model.DB.WithContext(c.Request.Context()).Model(&model.KBDocument{})
	if userID := currentUserID(c); userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	err := q.Select(
		"COALESCE(SUM(CASE WHEN source = 'memory' THEN 1 ELSE 0 END), 0) AS n_memory, " +
			"COALESCE(SUM(CASE WHEN source = 'profile' THEN 1 ELSE 0 END), 0) AS n_profile, " +
			"COALESCE(SUM(CASE WHEN source <> 'memory' AND source <> 'profile' THEN 1 ELSE 0 END), 0) AS n_document").
		Scan(&row).Error
	if err != nil {
		return 0, 0, 0
	}
	return row.Document, row.Memory, row.Profile
}

// kbCountByScope 按 scope 统计当前用户的文档条数（陪伴/工作记忆分开）：
//
// life         = scope = 'life'（生活/陪伴）
// work         = scope = 'work'（项目/技术）
// unclassified = scope 为空或 NULL（旧数据/未分类）
//
// 三个数之和恒等于 documents（每条文档只属于一类）。口径与 kbScopeFilter / kbListDocuments 一致。
func kbCountByScope(c *gin.Context) (int64, int64, int64) {
	if model.DB == nil {
		return 0, 0, 0
	}
	var row struct {
		Life         int64 `gorm:"column:n_life"`
		Work         int64 `gorm:"column:n_work"`
		Unclassified int64 `gorm:"column:n_unclassified"`
	}
	q := model.DB.WithContext(c.Request.Context()).Model(&model.KBDocument{})
	if userID := currentUserID(c); userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	err := q.Select(
		"COALESCE(SUM(CASE WHEN scope = 'life' THEN 1 ELSE 0 END), 0) AS n_life, " +
			"COALESCE(SUM(CASE WHEN scope = 'work' THEN 1 ELSE 0 END), 0) AS n_work, " +
			"COALESCE(SUM(CASE WHEN COALESCE(scope, '') = '' THEN 1 ELSE 0 END), 0) AS n_unclassified").
		Scan(&row).Error
	if err != nil {
		return 0, 0, 0
	}
	return row.Life, row.Work, row.Unclassified
}

func kbSearch(c *gin.Context) {
	var body struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	hits, err := kb().Search(c.Request.Context(), currentUserID(c), body.Query, clampTopK(body.TopK))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "hits": hits})
}

func clampTopK(n int) int {
	if n <= 0 {
		return 5
	}
	if n > 20 {
		return 20
	}
	return n
}

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
