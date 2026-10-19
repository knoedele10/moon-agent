package rag

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"blog-my/internal/model"

	"gorm.io/gorm"
)

// Store 负责知识库的持久化与向量检索。
type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// ChunkInput 是待落库的一个块。
type ChunkInput struct {
	Seq       int
	Kind      string
	Text      string
	ImagePath string
	Vec       []float32
}

// Hit 是一次检索命中的块。
type Hit struct {
	ChunkID    uint    `json:"chunk_id"`
	DocumentID uint    `json:"document_id"`
	Seq        int     `json:"seq"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	// 下面三个是融合前的分项得分，便于排查“为什么这条排前面”。
	Semantic float64 `json:"semantic"`
	Keyword  float64 `json:"keyword"`
	Entity   float64 `json:"entity"`
	// Entities 是这条记忆入库时抽出的实体（仅记忆类文档有）。
	Entities []string `json:"entities,omitempty"`
	// CreatedAt 是这条记忆/文档的入库时间：注入时标出来，让模型能判断新旧。
	CreatedAt time.Time `json:"created_at"`
	// Scope 是这条记忆的分类（life/work/未分类），用于检索时按会话类型加权。
	Scope string `json:"scope,omitempty"`
	// 下面四个是召回公式（见 dynamics.go）的输入，原样从 kb_document 读出。
	Importance   float64    `json:"importance,omitempty"`
	Activity     float64    `json:"activity,omitempty"`
	Emotion      float64    `json:"emotion,omitempty"`
	ReinforcedAt *time.Time `json:"reinforced_at,omitempty"`
}

// Stats 是知识库的总量概览。
type Stats struct {
	Documents int64 `json:"documents"`
	Chunks    int64 `json:"chunks"`
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// cosineEncoded 按 float64 累加余弦相似度，直接读取落库字节，避免为每个候选分配向量切片。
func cosineEncoded(a []float32, encoded []byte) float64 {
	if len(a) == 0 || len(encoded)%4 != 0 || len(encoded)/4 != len(a) {
		return 0
	}
	var dot, na, nb float64
	for i, av := range a {
		bv := math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4:]))
		dot += float64(av) * float64(bv)
		na += float64(av) * float64(av)
		nb += float64(bv) * float64(bv)
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// FindDocumentBySHA 按内容指纹找已有文档；找不到返回 (nil, nil)。
// 用于上传去重：同一份文件传两次不应该变成两条记录。
func (s *Store) FindDocumentBySHA(ctx context.Context, sha, userID string) (*model.KBDocument, error) {
	if sha == "" {
		return nil, nil
	}
	var doc model.KBDocument
	q := s.db.WithContext(ctx).
		Select("id, user_id, kind, title, mime, sha256, chunk_count, created_at").
		Where("sha256 = ?", sha)
	// 去重必须限定在同一个用户内：否则 B 用户上传一份与 A 相同的内容时会命中 A 的文档，
	// 直接把 B 的那份丢掉（内容根本没入库，用户却收到“已存在”）。
	if strings.TrimSpace(userID) != "" {
		q = q.Where("user_id = ?", userID)
	}
	err := q.First(&doc).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// ReinforceDocument 强化一条记忆：活跃度按 A' = A + gain×(1-A) 提升，并刷新强化时间。
//
// 只有「用户明确重提同一件事」（sha256 去重命中）才调用；浏览/检索**绝不**调用，
// 否则随便搜一下就会把旧记忆越搜越“活跃”。
//
// 用一条 UPDATE 在数据库侧完成读改写，避免并发下丢更新；LEAST(1, …) 兜住浮点误差，
// 保证 activity 始终 <= 1。
func (s *Store) ReinforceDocument(ctx context.Context, docID uint) error {
	if docID == 0 {
		return nil
	}
	gain := DefaultDynamicsParams().ReinforceGain
	return s.db.WithContext(ctx).Model(&model.KBDocument{}).
		Where("id = ?", docID).
		Updates(map[string]any{
			"activity":      gorm.Expr("LEAST(1, activity + ? * (1 - activity))", gain),
			"reinforced_at": time.Now(),
		}).Error
}

// CreateDocument 在一个事务里写入文档与其全部块，避免出现“文档在、块不全”的中间态。
func (s *Store) CreateDocument(ctx context.Context, doc *model.KBDocument, chunks []ChunkInput) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		doc.ChunkCount = len(chunks)
		if err := tx.Create(doc).Error; err != nil {
			return err
		}
		// GORM 对带 default 标签的字段会跳过零值：importance=0 会被写成列默认值 0.5。
		// 但重要性 0 是合法档位（低价值细节），所以这里显式补一条 UPDATE 把它落成 0。
		// 其余调用方（IngestText/IngestImage）都显式写了 0.5，不会走到这里。
		if doc.Importance == 0 {
			if err := tx.Model(&model.KBDocument{}).Where("id = ?", doc.Id).Update("importance", 0).Error; err != nil {
				return err
			}
		}
		if len(chunks) == 0 {
			return nil
		}
		rows := make([]model.KBChunk, 0, len(chunks))
		for _, c := range chunks {
			rows = append(rows, model.KBChunk{
				DocumentID: doc.Id,
				Seq:        c.Seq,
				Kind:       c.Kind,
				Text:       c.Text,
				ImagePath:  c.ImagePath,
				Embedding:  encodeVec(c.Vec),
				Dim:        len(c.Vec),
			})
		}
		return tx.Create(&rows).Error
	})
}

// ListDocuments 只取元信息，**不**select content —— content 是 LONGBLOB，
// 列表页把每张图的字节都拉一遍就白白浪费带宽和内存。
func (s *Store) ListDocuments(ctx context.Context, userID string, limit int) ([]model.KBDocument, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := s.db.WithContext(ctx).
		Select("id, user_id, kind, title, source, mime, bytes, sha256, chunk_count, created_at").
		Order("id desc").Limit(limit)
	if userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	var rows []model.KBDocument
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetDocument 返回文档元信息与它的全部块（按序号）。
// 图片文档额外把原始字节读出来，供前端直接展示。
func (s *Store) GetDocument(ctx context.Context, id uint, userID string) (*model.KBDocument, []model.KBChunk, error) {
	var doc model.KBDocument
	q := s.db.WithContext(ctx).Where("id = ?", id)
	if userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	if err := q.First(&doc).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	var chunks []model.KBChunk
	if err := s.db.WithContext(ctx).
		Select("id, document_id, seq, kind, text, image_path, dim, created_at").
		Where("document_id = ?", doc.Id).Order("seq asc").Find(&chunks).Error; err != nil {
		return nil, nil, err
	}
	return &doc, chunks, nil
}

// DocumentImage 取出一个图片文档的 MIME 与原始字节，供多模态回答时随问题一起发给模型。
// 返回 ("", nil, nil) 表示不存在或不是图片。这里单独开一个方法而不是复用 GetDocument，
// 是因为只有回答命中图片时才需要读 LONGBLOB，日常列表不应该拉它。
func (s *Store) DocumentImage(ctx context.Context, id uint, userID string) (string, []byte, error) {
	var doc model.KBDocument
	q := s.db.WithContext(ctx).Select("id, kind, mime, content").Where("id = ?", id)
	if userID != "" {
		q = q.Where("user_id = ? OR user_id = ''", userID)
	}
	if err := q.First(&doc).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", nil, nil
		}
		return "", nil, err
	}
	if doc.Kind != "image" || len(doc.Content) == 0 {
		return "", nil, nil
	}
	return doc.Mime, doc.Content, nil
}

// DeleteDocument 删除文档及其全部块；返回是否真的删到了东西，便于上层区分 404。
func (s *Store) DeleteDocument(ctx context.Context, id uint, userID string) (bool, error) {
	deleted := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("id = ?", id)
		if userID != "" {
			q = q.Where("user_id = ? OR user_id = ''", userID)
		}
		var doc model.KBDocument
		if err := q.First(&doc).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil
			}
			return err
		}
		if err := tx.Where("document_id = ?", doc.Id).Delete(&model.KBChunk{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&model.KBDocument{}, doc.Id).Error; err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

// Stats 统计文档与块的数量。
// AppendMemoryEvent 写一条记忆变更流水。失败只返回错误，调用方可以忽略——
// 流水是审计用，不该因为它写失败而让一次正常的记忆写入变成失败。
func (s *Store) AppendMemoryEvent(ctx context.Context, docID uint, userID, event, detail string) error {
	if docID == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Create(&model.KBMemoryEvent{
		DocumentID: docID,
		UserID:     userID,
		Event:      event,
		Detail:     detail,
	}).Error
}

func (s *Store) Stats(ctx context.Context, userID string) (Stats, error) {
	var st Stats
	dq := s.db.WithContext(ctx).Model(&model.KBDocument{})
	cq := s.db.WithContext(ctx).Model(&model.KBChunk{})
	if userID != "" {
		dq = dq.Where("user_id = ? OR user_id = ''", userID)
		cq = cq.Where("document_id IN (SELECT id FROM kb_document WHERE user_id = ? OR user_id = '')", userID)
	}
	if err := dq.Count(&st.Documents).Error; err != nil {
		return st, err
	}
	if err := cq.Count(&st.Chunks).Error; err != nil {
		return st, err
	}
	return st, nil
}

// searchRow 是检索时从库里拉出的一行。
type searchRow struct {
	Id           uint
	DocumentID   uint
	Seq          int
	Kind         string
	Text         string
	Embedding    []byte
	Title        string
	CreatedAt    time.Time
	Meta         string
	Scope        string
	Importance   float64
	Activity     float64
	Emotion      float64
	ReinforcedAt *time.Time
}

// searchHits 供向量和词面检索共用：每个块只生成一条完整的候选。
// 元信息缓存仅在本次查询内复用，不会掩盖后续请求中的画像/记忆更新。
func searchHits(rows []searchRow, vec []float32) []Hit {
	hits := make([]Hit, 0, len(rows))
	metadata := make(map[string]docMeta)
	for _, r := range rows {
		if vec != nil && (len(r.Embedding)%4 != 0 || len(r.Embedding)/4 != len(vec)) {
			continue // 其他嵌入模型或损坏的向量不能参与语义检索。
		}
		meta, ok := metadata[r.Meta]
		if !ok {
			meta = parseDocMeta(r.Meta)
			metadata[r.Meta] = meta
		}
		h := Hit{
			ChunkID: r.Id, DocumentID: r.DocumentID, Seq: r.Seq,
			Kind: r.Kind, Title: r.Title, Text: r.Text,
			Entities: meta.Entities, CreatedAt: r.CreatedAt,
			Scope: normalizeScope(r.Scope), Importance: r.Importance,
			Activity: r.Activity, Emotion: r.Emotion, ReinforcedAt: r.ReinforcedAt,
		}
		if vec != nil {
			h.Semantic = cosineEncoded(vec, r.Embedding)
		}
		hits = append(hits, h)
	}
	return hits
}

// Search 做暴力余弦检索：把候选块全部读出来算相似度后取 topK。
//
// 为什么敢暴力算：个人知识库量级（几千到几万块）下，一次全表扫描 + 余弦
// 只需几十毫秒；换成向量数据库要多一个服务、多一份数据、多一类故障，
// 对这个量级不划算。真正的上限见 docs/rag-kb.md。
func (s *Store) Search(ctx context.Context, vec []float32, query string, topK int, userID string) ([]Hit, error) {
	return s.SearchScoped(ctx, vec, query, topK, userID, "")
}

// SearchScoped 与 Search 相同，额外接受一个 scope："life"/"work" 时对同 scope 的记忆加成，
// 空串表示不加权（旧调用方的行为）。
func (s *Store) SearchScoped(ctx context.Context, vec []float32, query string, topK int, userID, scope string) ([]Hit, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("查询向量为空")
	}
	if topK <= 0 {
		topK = 5
	}

	q := "SELECT c.id AS id, c.document_id AS document_id, c.seq AS seq, c.kind AS kind, " +
		"c.`text` AS `text`, c.embedding AS embedding, d.title AS title, d.meta AS meta, d.created_at AS created_at, " +
		"COALESCE(d.scope, '') AS scope, d.importance AS importance, d.activity AS activity, d.emotion AS emotion, d.reinforced_at AS reinforced_at " +
		"FROM kb_chunk c JOIN kb_document d ON d.id = c.document_id"
	// stale = 0：排除已被更新的同主题记忆（旧事实留库做审计，不参与检索）。
	var args []any
	conds := []string{"d.stale = 0"}
	if userID != "" {
		conds = append(conds, "(d.user_id = ? OR d.user_id = '')")
		args = append(args, userID)
	}
	q += " WHERE " + strings.Join(conds, " AND ")

	var rows []searchRow
	if err := s.db.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	hits := searchHits(rows, vec)

	// 先按语义分排序，取一个**过采样**的候选池，再在池内做关键词/实体融合。
	// 过采样是必要的：关键词命中但语义分不高的块（专有名词、编号）本来排在后面，
	// 不过采样就永远进不了融合环节。
	sort.Slice(hits, func(i, j int) bool { return hits[i].Semantic > hits[j].Semantic })
	poolSize := topK * 4
	if poolSize < 60 {
		poolSize = 60
	}
	if len(hits) > poolSize {
		hits = hits[:poolSize]
	}
	return rankCandidates(hits, query, topK, scope), nil
}
