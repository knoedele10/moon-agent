package rag

import (
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchHitsKeepOneCompleteCandidatePerChunk(t *testing.T) {
	now := time.Now()
	rows := []searchRow{
		{Id: 1, DocumentID: 10, Seq: 1, Kind: "text", Text: "部署端口 8300", Title: "项目",
			Embedding: encodeVec([]float32{1, 2}), Meta: `{"entities":["8300"]}`, CreatedAt: now,
			Scope: ScopeWork, Importance: .7, Activity: .8, Emotion: .3, ReinforcedAt: &now},
		{Id: 2, DocumentID: 10, Seq: 2, Kind: "text", Text: "部署端口 8300", Title: "项目",
			Embedding: encodeVec([]float32{1, 2}), Meta: `{"entities":["8300"]}`, CreatedAt: now,
			Scope: ScopeWork, Importance: .7, Activity: .8, Emotion: .3, ReinforcedAt: &now},
	}
	for _, semantic := range []bool{false, true} {
		var vec []float32
		if semantic {
			vec = []float32{1, 2}
		}
		hits := searchHits(rows, vec)
		if len(hits) != len(rows) {
			t.Fatalf("semantic=%v: got %d candidates for %d rows", semantic, len(hits), len(rows))
		}
		for i, hit := range hits {
			row := rows[i]
			if hit.ChunkID != row.Id || hit.DocumentID != row.DocumentID || hit.Seq != row.Seq ||
				hit.Kind != row.Kind || hit.Text != row.Text || hit.Title != row.Title ||
				!hit.CreatedAt.Equal(now) || hit.Scope != row.Scope || hit.Importance != row.Importance ||
				hit.Activity != row.Activity || hit.Emotion != row.Emotion || hit.ReinforcedAt != &now ||
				!reflect.DeepEqual(hit.Entities, []string{"8300"}) {
				t.Fatalf("lost candidate metadata: %+v", hit)
			}
		}
		var ranked []Hit
		if semantic {
			ranked = rankCandidates(hits, "部署端口 8300", 5, ScopeWork)
		} else {
			ranked = rankLexicalOnly(hits, "部署端口 8300", 5, ScopeWork)
		}
		if len(ranked) != 2 || ranked[0].ChunkID == ranked[1].ChunkID {
			t.Fatalf("semantic=%v: expected two distinct chunks: %+v", semantic, ranked)
		}
	}
	// 缓存只活在一次调用里，后续查询必须读到更新后的实体。
	rows[0].Meta = `{"entities":["新端口"]}`
	if got := searchHits(rows[:1], nil)[0].Entities; !reflect.DeepEqual(got, []string{"新端口"}) {
		t.Fatalf("request cache leaked: %v", got)
	}
}

func TestSearchHitsSkipsOnlyInvalidVectorRows(t *testing.T) {
	rows := []searchRow{
		{Id: 1, Embedding: encodeVec([]float32{1, 2})},
		{Id: 2, Embedding: encodeVec([]float32{1})},
		{Id: 3, Embedding: []byte{1, 2, 3, 4, 5}},
		{Id: 4},
	}
	if hits := searchHits(rows, []float32{1, 2}); len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("invalid vectors participated in semantic retrieval: %+v", hits)
	}
	if hits := searchHits(rows, nil); len(hits) != 4 {
		t.Fatalf("lexical retrieval must keep chunks without valid vectors: %+v", hits)
	}
}

// 保留优化前的解码和累加次序作为独立参考，覆盖随机数据和异常浮点值。
func referenceDecodedCosine(a []float32, encoded []byte) float64 {
	if len(encoded) == 0 || len(encoded)%4 != 0 || len(a) == 0 || len(encoded)/4 != len(a) {
		return 0
	}
	b := make([]float32, len(encoded)/4)
	for i := range b {
		b[i] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4:]))
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestCosineEncodedEquivalent(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	vectors := [][]float32{nil, {0}, {1}, {-1}, {1, 0}, {0, 0}, {float32(math.Inf(1))}, {float32(math.NaN())}}
	for _, dim := range []int{3, 16, 128, 1024} {
		for range 4 {
			vec := make([]float32, dim)
			for i := range vec {
				vec[i] = rng.Float32()*2 - 1
			}
			vectors = append(vectors, vec)
		}
	}
	for _, a := range vectors {
		for _, b := range vectors {
			encoded := encodeVec(b)
			want, got := referenceDecodedCosine(a, encoded), cosineEncoded(a, encoded)
			if !(math.IsNaN(want) && math.IsNaN(got)) && math.Float64bits(want) != math.Float64bits(got) {
				t.Fatalf("dim %d/%d: got %v want %v", len(a), len(b), got, want)
			}
		}
	}
	if got := cosineEncoded([]float32{1}, []byte{1, 2, 3, 4, 5}); got != 0 {
		t.Fatalf("malformed vector: %v", got)
	}
}

func referenceLexicalScore(query, text string) float64 {
	seen, have := map[string]bool{}, map[string]bool{}
	for _, token := range tokenize(query) {
		seen[token] = true
	}
	textTokens := tokenize(text)
	for _, token := range textTokens {
		have[token] = true
	}
	if len(seen) == 0 || len(textTokens) == 0 {
		return 0
	}
	hit := 0
	for token := range seen {
		if have[token] {
			hit++
		}
	}
	score := float64(hit) / float64(len(seen))
	if len(textTokens) > 120 {
		score *= .85
	}
	return score
}

func referenceEntityBoost(query, docs []string) float64 {
	if len(query) == 0 || len(docs) == 0 {
		return 0
	}
	hits := 0
	for _, q := range query {
		nq := stripEntityPunct(q)
		if nq == "" {
			continue
		}
		for _, doc := range docs {
			nd := stripEntityPunct(doc)
			if nd != "" && (nd == nq || strings.Contains(nd, nq) || strings.Contains(nq, nd)) {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(query))
}

func TestPreparedQueryScoringEquivalent(t *testing.T) {
	queries := []string{"", "!!!", "发布口令 发布口令", "银河-42 是什么", "蓝鲸项目怎么样", " PORT 8300 ", `「银河-42」和"!!!"`, "a 中 Unicode-字符"}
	texts := []string{"", "用户的发布口令是银河-42", "部署端口为 PORT 8300", "用户喜欢喝咖啡", strings.Repeat("发布口令 ", 70)}
	docs := [][]string{nil, {""}, {"!!!", "银河42", "蓝鲸"}, {"PORT", "8300", "a"}, {"银河-42", "银河-42"}}
	for _, query := range queries {
		prepared := prepareRetrievalQuery(query)
		for _, text := range texts {
			if got, want := lexicalScoreTokens(prepared.tokens, text), referenceLexicalScore(query, text); got != want {
				t.Fatalf("keyword %q / %q: got %v want %v", query, text, got, want)
			}
		}
		for _, entities := range docs {
			normalized := normalizeEntities(entities)
			if got, want := entityBoostNormalized(prepared.entities, normalized), referenceEntityBoost(extractQueryEntities(query), entities); got != want {
				t.Fatalf("entity %q / %v: got %v want %v", query, entities, got, want)
			}
			want := 0.0
			for _, entity := range entities {
				n := stripEntityPunct(entity)
				if len([]rune(n)) >= 2 && strings.Contains(stripEntityPunct(query), n) {
					want = 1
				}
			}
			if got := docEntityHitInNormalizedQuery(prepared.normalized, normalized); got != want {
				t.Fatalf("reverse entity %q / %v: got %v want %v", query, entities, got, want)
			}
		}
	}
	// 归一化后为空的实体仍占原公式的分母，不能把 1/2 改成 1。
	if got := entityBoost([]string{"!!!", "银河-42"}, []string{"银河42", "!!!"}); got != .5 {
		t.Fatalf("empty normalized entity changed denominator: %v", got)
	}
}

var cosineBenchmarkResult float64

func BenchmarkCosineEncoded(b *testing.B) {
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i + 1)
	}
	encoded := encodeVec(vec)
	b.Run("decoded", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cosineBenchmarkResult = referenceDecodedCosine(vec, encoded)
		}
	})
	b.Run("encoded", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cosineBenchmarkResult = cosineEncoded(vec, encoded)
		}
	})
}
