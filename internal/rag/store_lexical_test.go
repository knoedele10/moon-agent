package rag

import "testing"

// 嵌入不可用时的兜底：只看词面与实体，**不做语义阈值门控**。
// 目标是“已经存好的记忆依旧能被搜到”，而不是“完美排序”。
func TestRankLexicalOnly(t *testing.T) {
	cands := []Hit{
		{ChunkID: 1, Text: "用户的部署端口是 8300"},
		{ChunkID: 2, Text: "今天天气不错，适合出门散步"},
		{ChunkID: 3, Text: "无关内容", Entities: []string{"蓝鲸"}},
	}

	// 词面命中：只有第一条与查询有关。
	got := rankLexicalOnly(cands, "部署端口是多少", 5, "")
	if len(got) != 1 || got[0].ChunkID != 1 {
		t.Fatalf("词面命中应当只留下第 1 条，实际 %+v", got)
	}
	if got[0].Score <= 0 {
		t.Fatalf("命中项应当有正分: %+v", got[0])
	}

	// 实体命中同样能捞出来（“蓝鲸”这种专名，语义分不可用时只能靠实体）。
	got = rankLexicalOnly(cands, "蓝鲸项目怎么样", 5, "")
	if len(got) == 0 {
		t.Fatal("实体命中应当至少留一条")
	}
	found := false
	for _, h := range got {
		if h.ChunkID == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("实体命中的第 3 条应当被留下: %+v", got)
	}

	// 完全无关的查询：不该返回任何东西（宁可空，也别塞无关记忆）。
	if got := rankLexicalOnly(cands, "完全不相关的另一件事", 5, ""); len(got) != 0 {
		t.Fatalf("无关查询不该有命中，实际 %+v", got)
	}

	if got := rankLexicalOnly(nil, "任意", 5, ""); got != nil {
		t.Fatalf("空候选应当返回 nil，实际 %+v", got)
	}
}
