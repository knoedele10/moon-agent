package rag

import (
	"math"
	"strings"
	"testing"
)

func TestTokenizeChineseBigramsAndASCII(t *testing.T) {
	got := tokenize("发布口令是 Galaxy-42")
	joined := strings.Join(got, ",")
	for _, want := range []string{"发布", "布口", "口令", "galaxy-42"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应当切出 %q，实际 %v", want, got)
		}
	}
	if strings.Contains(joined, ",是,") {
		t.Fatalf("单个汉字不该单独成词（噪声太大）: %v", got)
	}
}

func TestLexicalScorePrefersExactMatch(t *testing.T) {
	q := "发布口令"
	exact := lexicalScore(q, "用户的发布口令是银河-42")
	unrelated := lexicalScore(q, "今天天气不错，适合出门散步")
	if exact <= unrelated {
		t.Fatalf("命中查询词的文本得分应当更高: exact=%v unrelated=%v", exact, unrelated)
	}
	if exact <= 0 {
		t.Fatalf("应当有非零得分: %v", exact)
	}
	if unrelated != 0 {
		t.Fatalf("完全无关的文本应当得 0，实际 %v", unrelated)
	}
}

func TestExtractQueryEntities(t *testing.T) {
	got := extractQueryEntities(`我的「银河-42」口令是什么？deploy port 是多少`)
	joined := strings.ToLower(strings.Join(got, ","))
	for _, want := range []string{"银河-42", "deploy", "port"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应当抽到实体 %q，实际 %v", want, got)
		}
	}
}

func TestEntityBoostMatchesSubstring(t *testing.T) {
	// 查询里的“银河-42”与库里存的“银河42”应当算命中（标点差异不该漏）。
	got := entityBoost([]string{"银河-42"}, []string{"银河42", "项目"})
	if got <= 0 {
		t.Fatalf("标点差异应当仍算命中，实际 %v", got)
	}
	if entityBoost([]string{"完全无关"}, []string{"银河42"}) != 0 {
		t.Fatal("无关实体不该有加成")
	}
}

// 融合公式：分母随启用的信号自适应（mem0 的做法）。
func TestFuseScoresAdaptiveDenominator(t *testing.T) {
	// 只有语义：除以 1.0
	if got := fuseScores(0.8, 0, 0, false, false); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("只有语义时应当等于语义分，实际 %v", got)
	}
	// 语义 + 关键词：除以 2.0
	if got := fuseScores(0.8, 0.6, 0, true, false); math.Abs(got-0.7) > 1e-9 {
		t.Fatalf("语义+关键词应当除以 2.0，实际 %v", got)
	}
	// 三者齐全：除以 2.5
	if got := fuseScores(0.8, 0.6, 0.5, true, true); math.Abs(got-0.76) > 1e-9 {
		t.Fatalf("三路信号应当除以 2.5，实际 %v", got)
	}
}

// 关键词命中但语义分低于阈值的块必须被排除：这正是 mem0「阈值先卡语义再融合」的用意，
// 否则一个只靠关键词命中的无关块会被抬上来。
func TestRankCandidatesGatesSemanticFirst(t *testing.T) {
	cands := []Hit{
		{ChunkID: 1, Text: "发布口令是银河-42", Semantic: 0.80},
		{ChunkID: 2, Text: "发布口令 发布口令 发布口令", Semantic: 0.02}, // 关键词满分但语义极低
	}
	got := rankCandidates(cands, "发布口令", 5, "")
	if len(got) != 1 || got[0].ChunkID != 1 {
		t.Fatalf("低于语义阈值的候选应当被排除，实际 %+v", got)
	}
}

// 关键词那一路要能改变排序：语义稍低但精确包含查询词的块应当反超。
func TestRankCandidatesKeywordCanReorder(t *testing.T) {
	cands := []Hit{
		{ChunkID: 1, Text: "这一段聊了很多事情，顺带提过一句口令相关的话题", Semantic: 0.62},
		{ChunkID: 2, Text: "发布口令是银河-42", Semantic: 0.60},
	}
	got := rankCandidates(cands, "发布口令", 5, "")
	if len(got) != 2 {
		t.Fatalf("应当返回两条，实际 %d", len(got))
	}
	if got[0].ChunkID != 2 {
		t.Fatalf("精确命中关键词的块应当排第一，实际顺序 %+v", got)
	}
	if got[0].Keyword <= 0 {
		t.Fatalf("应当记录关键词分项，实际 %v", got[0].Keyword)
	}
}

func TestRankCandidatesEntityBoost(t *testing.T) {
	cands := []Hit{
		{ChunkID: 1, Text: "无关内容", Semantic: 0.55},
		{ChunkID: 2, Text: "无关内容", Semantic: 0.55, Entities: []string{"银河-42"}},
	}
	got := rankCandidates(cands, "银河-42 是什么", 5, "")
	if len(got) != 2 {
		t.Fatalf("应当返回两条，实际 %d", len(got))
	}
	if got[0].ChunkID != 2 {
		t.Fatalf("带实体命中的块应当排第一，实际 %+v", got)
	}
}

func TestParseDocMetaTolerant(t *testing.T) {
	if got := parseDocMeta(""); len(got.Entities) != 0 {
		t.Fatalf("空 meta 应当返回零值，实际 %+v", got)
	}
	if got := parseDocMeta("not json"); len(got.Entities) != 0 {
		t.Fatalf("非法 meta 不该让检索失败，实际 %+v", got)
	}
	got := parseDocMeta(`{"type":"fact","entities":["A","B"]}`)
	if got.Type != "fact" || len(got.Entities) != 2 {
		t.Fatalf("正常 meta 应当解析出来，实际 %+v", got)
	}
}

// scope 加权：同 scope 加成，不同 scope / 未指定都不惩罚。
func TestRankCandidatesScopeBoost(t *testing.T) {
	makeCands := func() []Hit {
		return []Hit{
			{ChunkID: 1, Text: "用户喜欢喝咖啡", Semantic: 0.60, Scope: ScopeWork},
			{ChunkID: 2, Text: "用户喜欢喝咖啡", Semantic: 0.60, Scope: ScopeLife},
		}
	}

	// 未指定 scope：两条同分，稳定排序保持原顺序。
	plain := rankCandidates(makeCands(), "喜欢喝咖啡", 5, "")
	if len(plain) != 2 || plain[0].ChunkID != 1 {
		t.Fatalf("未指定 scope 时不应改变原有顺序，实际 %+v", plain)
	}

	// 陪伴会话：life 那条应反超到第一。
	life := rankCandidates(makeCands(), "喜欢喝咖啡", 5, ScopeLife)
	if len(life) != 2 || life[0].ChunkID != 2 {
		t.Fatalf("陪伴会话里 life 记忆应排第一，实际 %+v", life)
	}

	// 工作会话：work 那条应反超到第一。
	work := rankCandidates(makeCands(), "喜欢喝咖啡", 5, ScopeWork)
	if len(work) != 2 || work[0].ChunkID != 1 {
		t.Fatalf("工作会话里 work 记忆应排第一，实际 %+v", work)
	}
}
