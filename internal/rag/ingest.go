package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"blog-my/internal/model"
)

// Ingester 把一份文档（文本或图片）变成可检索的向量块，并提供查询入口。
type Ingester struct {
	store *Store
	emb   Embedder
	vis   Visioner
	chunk ChunkOptions
}

func NewIngester(store *Store, emb Embedder, vis Visioner) *Ingester {
	return &Ingester{store: store, emb: emb, vis: vis, chunk: DefaultChunkOptions()}
}

// Result 是一次入库的结果。
type Result struct {
	DocumentID uint   `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	ChunkCount int    `json:"chunk_count"`
	Duplicated bool   `json:"duplicated"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IngestText 把一个纯文本文档切块、嵌入、入库。
func (i *Ingester) IngestText(ctx context.Context, userID, title, text string) (Result, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, errors.New("文本内容为空")
	}
	if strings.TrimSpace(title) == "" {
		title = firstLine(text, 40)
	}

	sha := sha256Hex([]byte(text))
	if doc, err := i.store.FindDocumentBySHA(ctx, sha, userID); err != nil {
		return Result{}, err
	} else if doc != nil {
		return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount, Duplicated: true}, nil
	}

	chunks := ChunkText(text, i.chunk)
	if len(chunks) == 0 {
		return Result{}, errors.New("切块结果为空")
	}
	vecs, embErr := i.emb.Embed(ctx, chunks)
	if embErr != nil {
		// 嵌入服务不可用时**也要把内容存下来**（向量留空）。
		// 代价是它暂时只能被关键词检索命中；好处是“用户刚说过的事”不会凭空消失。
		vecs = nil
	}

	inputs := make([]ChunkInput, 0, len(chunks))
	for idx, c := range chunks {
		vec := []float32(nil)
		if idx < len(vecs) {
			vec = vecs[idx]
		}
		inputs = append(inputs, ChunkInput{Seq: idx, Kind: "text", Text: c, Vec: vec})
	}

	doc := &model.KBDocument{
		UserID:  userID,
		Kind:    "text",
		Title:   title,
		Mime:    "text/plain; charset=utf-8",
		Bytes:   int64(len(text)),
		Sha256:  sha,
		Content: []byte(text),
	}
	// 普通文档/整轮降级记录不是抽取出的原子事实：给中性默认值（重要性 0.5、活跃度 1、无情绪）。
	doc.Importance = 0.5
	doc.Activity = 1
	if err := i.store.CreateDocument(ctx, doc, inputs); err != nil {
		return Result{}, err
	}
	return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount}, nil
}

// Fact 是一条从对话里抽出来的原子记忆。
//
// 与“把整轮对话存成一条”相比，原子事实的检索精度高得多：
// 一条事实只讲一件事，向量不会被无关内容稀释，关键词命中也不会被长文本摊平。
type Fact struct {
	Text     string   `json:"text"`
	Entities []string `json:"entities,omitempty"`
	// Scope 把陪伴记忆（life）与工作记忆（work）分开；空串=未分类（不硬猜）。
	Scope string `json:"scope,omitempty"`
	// Importance 是事实重要性（0/0.5/1），越高活跃度衰减越慢；缺省 0.5。
	Importance float64 `json:"importance,omitempty"`
	// Emotion 是情绪强度 0~1，没有情绪信息时为 0。
	Emotion float64 `json:"emotion,omitempty"`
}

// IngestFact 把一条原子事实入库，并把抽出的实体写进 meta 供检索加权。
func (i *Ingester) IngestFact(ctx context.Context, userID, title string, f Fact) (Result, error) {
	text := strings.TrimSpace(f.Text)
	if text == "" {
		return Result{}, errors.New("事实内容为空")
	}
	if strings.TrimSpace(title) == "" {
		title = firstLine(text, 40)
	}

	// 前缀把事实与同名普通文档区分开：同一段文字作为上传资料和作为记忆，不应该互相去重。
	sha := sha256Hex([]byte("fact:" + text))
	if doc, err := i.store.FindDocumentBySHA(ctx, sha, userID); err != nil {
		return Result{}, err
	} else if doc != nil {
		// 去重命中 = 用户又提了一遍同一件事（“明确重提”）：强化这条记忆。
		// 只在这里强化——浏览/检索命中绝不强化（见 Ingester.Search / Store.Search）。
		if rerr := i.store.ReinforceDocument(ctx, doc.Id); rerr != nil {
			log.Printf("[memory] 强化记忆失败（不影响本次写入）：%v", rerr)
		}
		return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount, Duplicated: true}, nil
	}

	chunks := ChunkText(text, i.chunk)
	if len(chunks) == 0 {
		return Result{}, errors.New("切块结果为空")
	}
	vecs, embErr := i.emb.Embed(ctx, chunks)
	if embErr != nil {
		// 同上：嵌入不可用也先存下内容，别让这一轮的记忆凭空消失。
		vecs = nil
	}
	inputs := make([]ChunkInput, 0, len(chunks))
	for idx, c := range chunks {
		var vec []float32
		if idx < len(vecs) {
			vec = vecs[idx]
		}
		inputs = append(inputs, ChunkInput{Seq: idx, Kind: "text", Text: c, Vec: vec})
	}

	meta, _ := json.Marshal(map[string]any{
		"type":     "fact",
		"entities": f.Entities,
	})
	doc := &model.KBDocument{
		UserID:  userID,
		Kind:    "text",
		Title:   title,
		Source:  "memory", // 标明这条来自自动记忆，便于与上传资料区分
		Mime:    "text/plain; charset=utf-8",
		Bytes:   int64(len(text)),
		Sha256:  sha,
		Content: []byte(text),
		Meta:    string(meta),
	}
	// 抽取时定下的 scope / importance / emotion 直接落库；活跃度从 1.0 起步。
	// 旧调用方若没填这些字段：scope=""、importance=0、emotion=0，均与兼容默认值一致
	// （parseFacts 已把缺省的 importance 补成 0.5）。
	doc.Scope = normalizeScope(f.Scope)
	doc.Importance = f.Importance
	doc.Activity = 1
	doc.Emotion = clamp01(f.Emotion)
	if err := i.store.CreateDocument(ctx, doc, inputs); err != nil {
		return Result{}, err
	}
	i.store.AppendMemoryEvent(ctx, doc.Id, userID, "ADD", text)
	// 同主题的旧记忆失效：让“改主意”是干净的，而不是新旧并存靠排序碰运气。
	// 失败不影响本次写入（新事实已经存下了）。
	if n, serr := i.supersedeSimilarFacts(ctx, userID, doc.Id, text); serr != nil {
		log.Printf("[memory] 标记旧记忆失效失败（不影响本次写入）：%v", serr)
	} else if n > 0 {
		// 新事实取代了旧的同主题记忆：这也是一次“明确重提”。新文档刚写入，
		// activity 本就是 1.0（强化后的上限），这里只需把强化时间戳明确记下来。
		if rerr := i.store.ReinforceDocument(ctx, doc.Id); rerr != nil {
			log.Printf("[memory] 更新强化时间失败（不影响本次写入）：%v", rerr)
		}
		log.Printf("[memory] 已让 %d 条旧记忆失效（被新事实取代）", n)
	}
	return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount}, nil
}

// IngestImage 把一个图片文档入库：先让视觉模型生成描述与 OCR 文字，
// 再把这段文字向量化 —— 于是图片和普通文本进了同一个检索空间。
func (i *Ingester) IngestImage(ctx context.Context, userID, title, mime, source string, data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, errors.New("图片内容为空")
	}
	if strings.TrimSpace(title) == "" {
		title = "未命名图片"
	}

	sha := sha256Hex(data)
	if doc, err := i.store.FindDocumentBySHA(ctx, sha, userID); err != nil {
		return Result{}, err
	} else if doc != nil {
		return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount, Duplicated: true}, nil
	}

	desc, err := i.vis.Describe(ctx, data)
	if err != nil {
		return Result{}, fmt.Errorf("图片理解失败（视觉模型不可用或超时）：%w", err)
	}

	// 把标题拼进检索文本：这样即使视觉模型没认出图里的东西，按文件名也能搜到。
	chunkText := fmt.Sprintf("【图片】%s\n%s", title, desc)
	vecs, err := i.emb.Embed(ctx, []string{chunkText})
	if err != nil {
		return Result{}, err
	}
	if len(vecs) == 0 {
		return Result{}, errors.New("图片描述嵌入为空")
	}

	meta, _ := json.Marshal(map[string]any{
		"indexed_by":    "vision",
		"desc_runes":    utf8.RuneCountInString(desc),
		"vision_prompt": "v1",
		"original_mime": mime,
	})

	doc := &model.KBDocument{
		UserID:  userID,
		Kind:    "image",
		Title:   title,
		Source:  source,
		Mime:    mime,
		Bytes:   int64(len(data)),
		Sha256:  sha,
		Content: data,
		Meta:    string(meta),
	}
	// 图片文档同样给中性默认值（重要性 0.5、活跃度 1、无情绪、无 scope）。
	doc.Importance = 0.5
	doc.Activity = 1
	inputs := []ChunkInput{{Seq: 0, Kind: "image_caption", Text: chunkText, ImagePath: source, Vec: vecs[0]}}
	if err := i.store.CreateDocument(ctx, doc, inputs); err != nil {
		return Result{}, err
	}
	return Result{DocumentID: doc.Id, Kind: doc.Kind, Title: doc.Title, ChunkCount: doc.ChunkCount}, nil
}

// Search 把查询串向量化后检索最相似的若干块。
func (i *Ingester) Search(ctx context.Context, userID, query string, topK int) ([]Hit, error) {
	return i.SearchScoped(ctx, userID, query, topK, "")
}

// SearchScoped 与 Search 相同，额外接受一个 scope（当前会话的类型：life/work/空）。
// 空串表示不加权，行为与旧 Search 完全一致。它让 *Ingester 同时满足 MemorySearcher 与
// ScopedMemorySearcher（见 memory.go），而不必改动 MemorySearcher 的旧签名。
func (i *Ingester) SearchScoped(ctx context.Context, userID, query string, topK int, scope string) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("查询内容为空")
	}
	vecs, err := i.emb.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		// 嵌入不可用（Ollama 没启动、模型被删了……）——**不放弃检索**，
		// 改走纯关键词路径：词面与实体两路都是本地计算，不依赖任何模型。
		// 这让“助手突然失忆”不再发生：已经存好的记忆依旧能被搜到。
		return i.store.SearchLexicalScoped(ctx, query, topK, userID, scope)
	}
	// 把原始 query 一并传下去：融合排序要用它算关键词与实体。
	// 能嵌入查询 → 说明嵌入服务活着：顺手补算那批“写入时缺向量”的记忆（后台跑，不阻塞本轮）。
	i.maybeBackfill()
	return i.store.SearchScoped(ctx, vecs[0], query, topK, userID, scope)
}

// DocumentImage 转发给 Store：回答命中图片时取原图。
func (i *Ingester) DocumentImage(ctx context.Context, id uint, userID string) (string, []byte, error) {
	return i.store.DocumentImage(ctx, id, userID)
}

// ListDocuments / GetDocument / DeleteDocument / Stats 直接转发给 Store，
// 让上层只需要依赖 Ingester 一个东西。
func (i *Ingester) ListDocuments(ctx context.Context, userID string, limit int) ([]model.KBDocument, error) {
	return i.store.ListDocuments(ctx, userID, limit)
}

func (i *Ingester) GetDocument(ctx context.Context, id uint, userID string) (*model.KBDocument, []model.KBChunk, error) {
	return i.store.GetDocument(ctx, id, userID)
}

func (i *Ingester) DeleteDocument(ctx context.Context, id uint, userID string) (bool, error) {
	return i.store.DeleteDocument(ctx, id, userID)
}

func (i *Ingester) Stats(ctx context.Context, userID string) (Stats, error) {
	return i.store.Stats(ctx, userID)
}

// BuildContext 把命中块拼成给模型看的上下文，带编号以便回答时标注出处。
func BuildContext(hits []Hit, maxRunes int) string {
	var b strings.Builder
	used := 0
	for idx, h := range hits {
		label := h.Kind
		if label == "image_caption" {
			label = "图片"
		} else {
			label = "文本"
		}
		// 标出入库日期：让模型能自己判断“这条是不是过时了”（记忆是只增不改的）。
		when := ""
		if !h.CreatedAt.IsZero() {
			when = " · " + h.CreatedAt.Format("2006-01-02") + " 记录"
		}
		block := fmt.Sprintf("[%d]（%s · 《%s》%s · 相似度 %.3f）\n%s\n\n", idx+1, label, h.Title, when, h.Score, strings.TrimSpace(h.Text))
		n := utf8.RuneCountInString(block)
		if maxRunes > 0 && used+n > maxRunes && idx > 0 {
			break
		}
		b.WriteString(block)
		used += n
	}
	return b.String()
}

// firstLine 取第一行非空内容作为标题，并限制长度。
func firstLine(s string, maxRunes int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > maxRunes {
			r = r[:maxRunes]
		}
		return string(r)
	}
	return "未命名文档"
}
