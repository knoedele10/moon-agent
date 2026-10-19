package rag

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
)

// 混合检索：语义 + 关键词 + 实体，融合方式参考 mem0 v3 的做法。
//
// 三条与 mem0 对齐的关键设计：
//  1. **语义分先过阈值再融合**——否则关键词/实体能把不相关的块拉进来；
//  2. combined = (semantic + keyword + entity) / maxPossible，maxPossible 随启用的信号数自适应
//     （只有语义 1.0 / 语义+实体 1.5 / 语义+关键词 2.0 / 三者 2.5）；
//  3. 语义先过采样出一个候选池，再在池内算关键词与实体，避免全量做昂贵计算。
const (
	// semanticThreshold 是进入融合前语义分的下限（mem0 默认 0.1）。
	semanticThreshold = 0.1
	// keywordWeight / entityWeight 是两个附加信号的满分权重。
	keywordWeight = 1.0
	entityWeight  = 0.5
)

// tokenize 把文本切成检索词。
//
// 中文按**字符二元组**切：中文没有空格，二元组是最省事又够用的近似（“发布口令” → 发布/布口/口令）。
// ASCII 与数字按连续串切，统一小写；单字符的英文词丢弃（噪声大）。
func tokenize(s string) []string {
	var out []string
	var ascii strings.Builder
	var han []rune

	flushASCII := func() {
		if ascii.Len() >= 2 {
			out = append(out, strings.ToLower(ascii.String()))
		}
		ascii.Reset()
	}
	flushHan := func() {
		if len(han) == 1 {
			out = append(out, string(han))
		} else {
			for i := 0; i+1 < len(han); i++ {
				out = append(out, string(han[i:i+2]))
			}
		}
		han = han[:0]
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushASCII()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			// 连字符/下划线算词内字符：Galaxy-42、bge-m3 这类标识符不该被切开。
			flushHan()
			ascii.WriteRune(r)
		default:
			flushASCII()
			flushHan()
		}
	}
	flushASCII()
	flushHan()
	return out
}

// lexicalScore 计算查询词在文本里的覆盖率，落在 [0,1]。
//
// 这是 BM25 的简化版：不做 IDF（个人库量级下 IDF 不稳定），
// 只看“查询里有几个词在文本中出现了”，再按文本长度轻微惩罚超长块。
func lexicalScore(query, text string) float64 {
	return lexicalScoreTokens(queryTokenSet(query), text)
}

func queryTokenSet(query string) map[string]struct{} {
	tokens := tokenize(query)
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		seen[token] = struct{}{}
	}
	return seen
}

func lexicalScoreTokens(seen map[string]struct{}, text string) float64 {
	if len(seen) == 0 {
		return 0
	}
	textTokens := tokenize(text)
	if len(textTokens) == 0 {
		return 0
	}
	have := make(map[string]struct{}, len(textTokens))
	for _, t := range textTokens {
		have[t] = struct{}{}
	}

	hit := 0
	for t := range seen {
		if _, ok := have[t]; ok {
			hit++
		}
	}
	score := float64(hit) / float64(len(seen))

	// 长度惩罚：同样命中，短块更可能是“精准命中”而不是顺带提到。
	if len(textTokens) > 120 {
		score *= 0.85
	}
	return score
}

// extractQueryEntities 从查询里挑出“像实体”的片段：
// 引号里的内容、含数字的串（银河-42、8300、v1.2）、长度 ≥3 的英文词、2~6 字的中文连续串。
func extractQueryEntities(q string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}

	// 引号内的内容优先当作实体（用户往往会强调专名）。
	for _, pair := range [][2]string{{"“", "”"}, {"「", "」"}, {`"`, `"`}, {"'", "'"}} {
		rest := q
		for {
			i := strings.Index(rest, pair[0])
			if i < 0 {
				break
			}
			j := strings.Index(rest[i+len(pair[0]):], pair[1])
			if j < 0 {
				break
			}
			add(rest[i+len(pair[0]) : i+len(pair[0])+j])
			rest = rest[i+len(pair[0])+j+len(pair[1]):]
		}
	}

	var ascii strings.Builder
	var han []rune
	flushASCII := func() {
		s := ascii.String()
		ascii.Reset()
		if len(s) < 2 {
			return
		}
		hasDigit := strings.ContainsAny(s, "0123456789")
		if hasDigit || len(s) >= 3 {
			add(s)
		}
	}
	flushHan := func() {
		if len(han) >= 2 && len(han) <= 6 {
			add(string(han))
		}
		han = han[:0]
	}
	for _, r := range q {
		switch {
		case unicode.Is(unicode.Han, r):
			flushASCII()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			flushHan()
			ascii.WriteRune(r)
		default:
			flushASCII()
			flushHan()
		}
	}
	flushASCII()
	flushHan()
	return out
}

// entityBoost 计算查询实体在文档实体里的命中比例，落在 [0,1]。
func entityBoost(queryEntities, docEntities []string) float64 {
	return entityBoostNormalized(normalizeEntities(queryEntities), normalizeEntities(docEntities))
}

func normalizeEntities(entities []string) []string {
	if len(entities) == 0 {
		return nil
	}
	normalized := make([]string, len(entities))
	for i, entity := range entities {
		normalized[i] = stripEntityPunct(entity)
	}
	return normalized
}

// 查询的分词、实体归一化和反查文本每次检索只准备一次。
type retrievalQuery struct {
	tokens     map[string]struct{}
	entities   []string
	normalized string
}

func prepareRetrievalQuery(query string) retrievalQuery {
	return retrievalQuery{
		tokens: queryTokenSet(query), entities: normalizeEntities(extractQueryEntities(query)),
		normalized: stripEntityPunct(query),
	}
}

func entityBoostNormalized(queryEntities, docEntities []string) float64 {
	if len(queryEntities) == 0 || len(docEntities) == 0 {
		return 0
	}

	hit := 0
	for _, qNorm := range queryEntities {
		if qNorm == "" {
			continue
		}
		for _, d := range docEntities {
			if d != "" && (d == qNorm || strings.Contains(d, qNorm) || strings.Contains(qNorm, d)) {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(queryEntities))
}

// fuseScores 按自适应分母融合三路得分。
func fuseScores(semantic, keyword, entity float64, hasKeyword, hasEntity bool) float64 {
	maxPossible := 1.0
	if hasKeyword {
		maxPossible += keywordWeight
	}
	if hasEntity {
		maxPossible += entityWeight
	}
	return (semantic + keyword + entity) / maxPossible
}

// docMeta 是 kb_document.meta 里我们会用到的部分。
// 用 json.RawMessage 宽松解析：老数据没有 meta，或 meta 是别的结构，都不能让检索失败。
type docMeta struct {
	Type     string   `json:"type"`
	Entities []string `json:"entities"`
}

func parseDocMeta(raw string) docMeta {
	var m docMeta
	if strings.TrimSpace(raw) == "" {
		return m
	}
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

// rankCandidates 在候选池内做融合排序，返回按融合分降序的前 topK 个。
// 抽成独立函数是为了能直接对“融合公式”写单测，不必起数据库。
// scope 是当前会话的类型（"life"/"work"/""）；空串表示不加权（未指定）。
func rankCandidates(cands []Hit, query string, topK int, scope string) []Hit {
	if len(cands) == 0 {
		return nil
	}
	prepared := prepareRetrievalQuery(query)

	params := DefaultDynamicsParams()

	// 先按语义分过阈值，并算出候选池里是否真的用得上关键词/实体两路。
	pool := make([]Hit, 0, len(cands))
	hasKeyword := false
	hasEntity := false
	for _, h := range cands {
		if h.Semantic < semanticThreshold {
			continue
		}
		h.Keyword = lexicalScoreTokens(prepared.tokens, h.Text)
		if h.Keyword > 0 {
			hasKeyword = true
		}
		h.Entity = entityBoostNormalized(prepared.entities, normalizeEntities(h.Entities))
		if h.Entity > 0 {
			hasEntity = true
		}
		pool = append(pool, h)
	}

	for i := range pool {
		kw, ent := 0.0, 0.0
		if hasKeyword {
			kw = pool[i].Keyword * keywordWeight
		}
		if hasEntity {
			ent = pool[i].Entity * entityWeight
		}
		// 召回公式（见 dynamics.go）：优先级 = 相关性 × (0.55 + 0.25A + 0.15I + 0.05E)。
		// 旧 recencyFactor（7 天内满分、之后最多扣 15%）已被这套多维公式取代。
		relevance := fuseScores(pool[i].Semantic, kw, ent, hasKeyword, hasEntity)
		pool[i].Score = priorityForHit(pool[i], relevance, scope, params)
	}

	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Score == pool[j].Score {
			return pool[i].Semantic > pool[j].Semantic
		}
		return pool[i].Score > pool[j].Score
	})
	if topK > 0 && len(pool) > topK {
		pool = pool[:topK]
	}
	return pool
}

// priorityForHit 把一条候选的“相关性 + 动力学 + scope”折算成最终排序分。
//
// 时间基准：有强化时间用强化时间，否则用写入时间（见 DynamicsParams.evolve）。
// scope 加权是**乘在最后**的：同 scope 加成，未分类/不同 scope 不惩罚。
func priorityForHit(h Hit, relevance float64, scope string, params DynamicsParams) float64 {
	t0 := h.CreatedAt
	if h.ReinforcedAt != nil && !h.ReinforcedAt.IsZero() {
		t0 = *h.ReinforcedAt
	}
	a, e := params.evolve(h.Activity, h.Emotion, h.Importance, t0, time.Now())
	return params.priority(relevance, a, h.Importance, e) * scopeWeight(h.Scope, scope)
}

// ---- 给技能检索复用的导出包装 ----
//
// 技能检索（web/skill_retrieve.go）要与知识库用**同一套**分词、词面覆盖与实体匹配，
// 否则同一个查询在“记忆”和“技能”两边会得出不一致的相关度。
// 这些函数本身与 KB 无关，导出即可，不需要把技能塞进这个包（那会把 rag 变成大杂烩）。

// Tokenize 暴露中文二元组 / ASCII 串分词。
func Tokenize(s string) []string { return tokenize(s) }

// LexicalScore 暴露词面覆盖率打分（简化 BM25）。
func LexicalScore(query, text string) float64 { return lexicalScore(query, text) }

// QueryEntities 暴露查询实体抽取。
func QueryEntities(q string) []string { return extractQueryEntities(q) }

// EntityBoost 暴露实体命中比例。
func EntityBoost(queryEntities, docEntities []string) float64 {
	return entityBoost(queryEntities, docEntities)
}

// FuseScores 暴露自适应分母的三路融合。
func FuseScores(semantic, keyword, entity float64, hasKeyword, hasEntity bool) float64 {
	return fuseScores(semantic, keyword, entity, hasKeyword, hasEntity)
}
