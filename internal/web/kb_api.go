package web

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/model"
	"blog-my/internal/rag"

	"github.com/gin-gonic/gin"
)

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
