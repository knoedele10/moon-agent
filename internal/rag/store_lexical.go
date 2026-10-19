package rag

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// 纯关键词检索：**不依赖任何嵌入模型**。
//
// 为什么需要它：记忆的读写原本都硬依赖本地 Ollama 的嵌入模型。Ollama 一停，
// 检索就直接降级成“本轮无记忆”——用户看到的是助手突然失忆，而且毫无提示。
// 但我们的混合检索里本来就有词面（中文二元组）与实体两路，这两路**纯本地计算**，
// 不需要任何模型。所以嵌入不可用时改走这里，已经存好的记忆依旧能被搜到
// （尤其是“蓝鲸”“8300”这类专名——词面命中恰恰是它最擅长的）。
//
// 与 rankCandidates 的关键差别：**不做语义阈值门控**（这里根本没有语义分），
// 改看“词面或实体是否真的命中”。
// scope 是当前会话的类型（"life"/"work"/""）；空串表示不加权（未指定）。
func rankLexicalOnly(cands []Hit, query string, topK int, scope string) []Hit {
	if len(cands) == 0 {
		return nil
	}
	prepared := prepareRetrievalQuery(query)

	params := DefaultDynamicsParams()

	pool := make([]Hit, 0, len(cands))
	for _, h := range cands {
		h.Keyword = lexicalScoreTokens(prepared.tokens, h.Text)
		docEntities := normalizeEntities(h.Entities)
		h.Entity = entityBoostNormalized(prepared.entities, docEntities)
		// 反向匹配：把**库里的实体**当关键词，看它是否出现在查询里。
		// 为什么需要：查询里的专名可能因为长度/切分规则没被抽出来
		// （例如「蓝鲸项目怎么样」是 7 个字，抽不出「蓝鲸」），
		// 但库里存着实体「蓝鲸」，直接反查就能命中。
		if e := docEntityHitInNormalizedQuery(prepared.normalized, docEntities); e > h.Entity {
			h.Entity = e
		}
		if h.Keyword <= 0 && h.Entity <= 0 {
			continue // 两边都没命中：与查询无关，直接丢掉
		}
		// 词面为主、实体为辅；量纲不同，按各自满分归一后相加，再套召回公式（见 dynamics.go）。
		relevance := (h.Keyword*keywordWeight + h.Entity*entityWeight) / (keywordWeight + entityWeight)
		h.Score = priorityForHit(h, relevance, scope, params)
		pool = append(pool, h)
	}

	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Score == pool[j].Score {
			return pool[i].Keyword > pool[j].Keyword
		}
		return pool[i].Score > pool[j].Score
	})
	if topK > 0 && len(pool) > topK {
		pool = pool[:topK]
	}
	return pool
}

// docEntityHitInNormalizedQuery 在已归一化的查询中反查实体；命中返回 1，否则 0。
func docEntityHitInNormalizedQuery(q string, docEntities []string) float64 {
	if q == "" || len(docEntities) == 0 {
		return 0
	}
	for _, n := range docEntities {
		if len([]rune(n)) >= 2 && strings.Contains(q, n) {
			return 1
		}
	}
	return 0
}

// stripEntityPunct 去掉标点与空白，只留字母/数字/汉字，并把英文转小写。
// 与 entityBoost 的口径一致（“银河-42”与“银河42”算同一个实体）。
func stripEntityPunct(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SearchLexical 是 Store 的关键词检索：只读文本与元信息，**不读 embedding 列**
// （省一次大字段扫描，也让没有向量的记忆一样能被搜到）。
func (s *Store) SearchLexical(ctx context.Context, query string, topK int, userID string) ([]Hit, error) {
	return s.SearchLexicalScoped(ctx, query, topK, userID, "")
}

// SearchLexicalScoped 与 SearchLexical 相同，额外接受一个 scope（空串=不加权）。
func (s *Store) SearchLexicalScoped(ctx context.Context, query string, topK int, userID, scope string) ([]Hit, error) {
	if topK <= 0 {
		topK = 5
	}
	q := "SELECT c.id AS id, c.document_id AS document_id, c.seq AS seq, c.kind AS kind, " +
		"c.`text` AS `text`, d.title AS title, d.meta AS meta, d.created_at AS created_at, " +
		"COALESCE(d.scope, '') AS scope, d.importance AS importance, d.activity AS activity, d.emotion AS emotion, d.reinforced_at AS reinforced_at " +
		"FROM kb_chunk c JOIN kb_document d ON d.id = c.document_id"
	// 条件用 AND 拼起来：不能直接再写一个 WHERE（那会生成两段 WHERE，SQL 直接报错）。
	// 其中 stale = 0 是排除“已被更新的同主题记忆”：旧事实留在库里做审计，但不再参与检索。
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

	// 词面打分只需把文本读出来：个人知识库量级不大，直接全量算，简单且不会漏。
	hits := searchHits(rows, nil)
	return rankLexicalOnly(hits, query, topK, scope), nil
}
