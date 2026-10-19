package web

import (
	"strings"
	"testing"

	"blog-my/internal/model"
)

// 技能检索的排序逻辑必须是**纯函数**能测的：不起数据库、不起 Ollama、不调大模型。
// 这三件事任一不可用都要能降级，降级路径正是最容易被改坏的地方。
func TestFuseSkillScoresAdaptsDenominator(t *testing.T) {
	// 只有词面一路：分母 1.0，命中一半就是 0.5。
	if got := fuseSkillScores(0.5, 0, 0, false, false); got != 0.5 {
		t.Fatalf("只有词面时应当 0.5，got %v", got)
	}
	// 词面 + 实体：分母 1.5，(0.5+0+0.5)/1.5。
	if got := fuseSkillScores(0.5, 0, 0.5, false, true); got != 1.0/1.5 {
		t.Fatalf("词面+实体时应当 %v，got %v", 1.0/1.5, got)
	}
	// 三路齐全：分母 2.5。
	if got := fuseSkillScores(1, 1, 0.5, true, true); got != 1 {
		t.Fatalf("三路满分应当 1，got %v", got)
	}
	// 缺语义时分数不该被“少一路”压到 0：这正是 Ollama 没开的机器上的路径。
	if got := fuseSkillScores(1, 0, 0, false, false); got != 1 {
		t.Fatalf("只有词面且满分时应当 1，got %v", got)
	}
}

func TestSkillCosine(t *testing.T) {
	if got := skillCosine([]float32{1, 2, 3}, []float32{1, 2, 3}); got < 0.999 {
		t.Fatalf("同向量应当约等于 1，got %v", got)
	}
	if got := skillCosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("正交应当 0，got %v", got)
	}
	// 维度不一致（换过嵌入模型）必须返回 0，而不是算出一个假相似度。
	if got := skillCosine([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Fatalf("维度不一致应当 0，got %v", got)
	}
	if got := skillCosine([]float32{0, 0}, []float32{1, 2}); got != 0 {
		t.Fatalf("零向量应当 0，got %v", got)
	}
	if got := skillCosine(nil, []float32{1}); got != 0 {
		t.Fatalf("空向量应当 0，got %v", got)
	}
}

func TestSkillNameHit(t *testing.T) {
	if !skillNameHit("用 caveman 风格说一句", "caveman") {
		t.Fatal("用户直接点名时应当命中")
	}
	if skillNameHit("帮我把中文改自然", "caveman") {
		t.Fatal("没点名不该命中")
	}
	if skillNameHit("", "caveman") {
		t.Fatal("空查询不该命中")
	}
}

func TestRankSkillsPrefersExplicitlyNamedSkill(t *testing.T) {
	rows := []model.AgentSkill{
		{Id: 1, Name: "humanizer-zh", Description: "中文润色，让文字自然", Body: "把机械的中文改自然。"},
		{Id: 2, Name: "caveman", Description: "压缩表达，去废话", Body: "极简输出。"},
		{Id: 3, Name: "officecli-pptx", Description: "做 PPT", Body: "生成演示文稿。"},
	}
	hits := rankSkills(rows, "用 caveman 风格说一句：你好", nil, 20)
	if len(hits) != 3 {
		t.Fatalf("应当召回 3 条，got %d", len(hits))
	}
	if hits[0].Name != "caveman" {
		t.Fatalf("被点名的技能应当排第一，got %s", hits[0].Name)
	}
	if !hits[0].NameHit {
		t.Fatal("应当标记 NameHit，便于界面解释为什么它排第一")
	}
}

func TestRankSkillsOrdersByLexicalMatch(t *testing.T) {
	rows := []model.AgentSkill{
		{Id: 1, Name: "caveman", Description: "压缩表达", Body: "极简。"},
		{Id: 2, Name: "humanizer-zh", Description: "中文润色，让文字自然流畅有人情味", Body: "适用于润色 AI 生成内容。"},
		{Id: 3, Name: "remotion-dev", Description: "视频编程", Body: "用代码做视频。"},
	}
	hits := rankSkills(rows, "帮我把这段中文润色得自然一点", nil, 20)
	if hits[0].Name != "humanizer-zh" {
		t.Fatalf("词面命中的应当排第一，got %s", hits[0].Name)
	}
}

// 回归：Ollama 没开时语义分表为空，排序必须照常工作（不能返回空、不能 panic）。
func TestRankSkillsWorksWithoutSemantic(t *testing.T) {
	rows := []model.AgentSkill{
		{Id: 1, Name: "a", Description: "无关", Body: "无关。"},
		{Id: 2, Name: "b", Description: "做表格", Body: "Excel 报表。"},
	}
	hits := rankSkills(rows, "帮我做个表格", nil, 20)
	if len(hits) != 2 {
		t.Fatalf("没有语义分时仍应返回全部候选，got %d", len(hits))
	}
	if hits[0].Name != "b" {
		t.Fatalf("词面命中的应当靠前，got %s", hits[0].Name)
	}
	for _, h := range hits {
		if h.Semantic != 0 {
			t.Fatalf("没有向量时语义分应当为 0，got %v", h.Semantic)
		}
	}
}

func TestRankSkillsUsesSemanticWhenAvailable(t *testing.T) {
	rows := []model.AgentSkill{
		{Id: 1, Name: "alpha", Description: "描述一", Body: "正文一。"},
		{Id: 2, Name: "beta", Description: "描述二", Body: "正文二。"},
	}
	// 两条词面分一样（都不命中），语义把 beta 顶上去。
	hits := rankSkills(rows, "完全不相关的查询词", map[uint]float64{2: 0.9}, 20)
	if hits[0].Name != "beta" {
		t.Fatalf("语义分高的应当排第一，got %s", hits[0].Name)
	}
	if hits[0].Semantic <= 0 {
		t.Fatalf("应当带上语义分，got %v", hits[0].Semantic)
	}
}

func TestRankSkillsTruncatesAndHandlesEmpty(t *testing.T) {
	var rows []model.AgentSkill
	for i := 0; i < 30; i++ {
		rows = append(rows, model.AgentSkill{Id: uint(i + 1), Name: string(rune('a' + i%26)), Body: "x"})
	}
	if hits := rankSkills(rows, "x", nil, 20); len(hits) != 20 {
		t.Fatalf("应当截断到 20 条，got %d", len(hits))
	}
	if hits := rankSkills(nil, "x", nil, 20); hits != nil {
		t.Fatalf("没有技能时应当返回 nil，got %v", hits)
	}
}

func TestParsePickedNames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"标准对象", `{"picked":["caveman","humanizer-zh"]}`, []string{"caveman", "humanizer-zh"}},
		{"skills 字段", `{"skills":["a"]}`, []string{"a"}},
		{"裸数组", `["x","y"]`, []string{"x", "y"}},
		{"markdown 围栏", "```json\n{\"picked\":[\"z\"]}\n```", []string{"z"}},
		{"前后有解释", `我觉得最相关的是 {"picked":["q"]}，其余不选。`, []string{"q"}},
		{"空数组", `{"picked":[]}`, nil},
		{"空串", "", nil},
		{"纯文字", "没有相关技能", nil},
		{"坏 JSON", `{"picked":[`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePickedNames(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
			}
		})
	}
}

func TestBuildSkillBlock(t *testing.T) {
	if got := buildSkillBlock(nil); got != "" {
		t.Fatalf("没有选中技能时应当返回空串，got %q", got)
	}

	long := strings.Repeat("很长的正文。", skillInjectRunes)
	block := buildSkillBlock([]skillHit{{Name: "big", Description: "简介", Body: long}})
	if !strings.Contains(block, "技能 1：big") {
		t.Fatalf("应当带上技能名，got %q", clipRunes(block, 80))
	}
	if !strings.Contains(block, "正文已截断") {
		t.Fatal("超长正文必须截断并明确告知模型，否则一次注入就能把上下文吃光")
	}
	if n := len([]rune(block)); n > skillInjectRunes+400 {
		t.Fatalf("截断后仍然过长：%d 字", n)
	}

	nobody := buildSkillBlock([]skillHit{{Name: "thin", Description: "只有简介"}})
	if !strings.Contains(nobody, "没有正文") {
		t.Fatalf("没有正文时应当说明，got %q", nobody)
	}
}

func TestJoinReason(t *testing.T) {
	if got := joinReason("", "b"); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := joinReason("a", "b"); got != "a；b" {
		t.Fatalf("got %q", got)
	}
}
