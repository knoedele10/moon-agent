package rag

import (
	"math"
	"testing"
	"time"
)

// 默认参数必须自洽：四项权重之和为 1，否则优先级会被整体缩放。
func TestDefaultDynamicsParamsWeightsSumToOne(t *testing.T) {
	p := DefaultDynamicsParams()
	sum := p.BaseWeight + p.ActivityWeight + p.ImportanceWeight + p.EmotionWeight
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("四项权重之和应为 1，实际 %v", sum)
	}
	if err := p.validate(); err != nil {
		t.Fatalf("默认参数应当通过校验：%v", err)
	}

	// 权重和不为 1 时必须被 validate 拒绝（这就是「加断言」的可测信号）。
	bad := p
	bad.EmotionWeight = 0.5
	if err := bad.validate(); err == nil {
		t.Fatal("权重和不为 1 时 validate 必须报错")
	}
}

// 活跃度半衰期随重要性拉长：H = 30 × (1 + 2×I)。
func TestHalfLifeGrowsWithImportance(t *testing.T) {
	p := DefaultDynamicsParams()
	if got := p.halfLife(0); math.Abs(got-30) > 1e-9 {
		t.Fatalf("I=0 时半衰期应为 30 天，实际 %v", got)
	}
	if got := p.halfLife(0.5); math.Abs(got-60) > 1e-9 {
		t.Fatalf("I=0.5 时半衰期应为 60 天，实际 %v", got)
	}
	if got := p.halfLife(1); math.Abs(got-90) > 1e-9 {
		t.Fatalf("I=1 时半衰期应为 90 天，实际 %v", got)
	}
	// 单调性：越重要衰减越慢。
	if !(p.halfLife(1) > p.halfLife(0.5) && p.halfLife(0.5) > p.halfLife(0)) {
		t.Fatal("半衰期应随重要性单调递增")
	}
}

// 指数衰减：经过一个半衰期正好减半；经过两个半衰期降到四分之一。
func TestDecayHalvesEachHalfLife(t *testing.T) {
	p := DefaultDynamicsParams()
	if got := p.decay(1.0, 30, 0); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("elapsed=0 时不应衰减，实际 %v", got)
	}
	if got := p.decay(1.0, 30, 30); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("一个半衰期应减半，实际 %v", got)
	}
	if got := p.decay(1.0, 30, 60); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("两个半衰期应降到 1/4，实际 %v", got)
	}
	// 时间倒流（elapsed<0）不放大：保持初始值。
	if got := p.decay(0.8, 30, -5); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("elapsed<0 时应返回初始值，实际 %v", got)
	}
}

// 重要性越高，同样的时间后活跃度保留得越多。
func TestDecaySlowerForMoreImportant(t *testing.T) {
	p := DefaultDynamicsParams()
	t0 := time.Now().AddDate(0, 0, -60) // 60 天前
	now := time.Now()

	lowA, _ := p.evolve(1.0, 0, 0.0, t0, now)
	highA, _ := p.evolve(1.0, 0, 1.0, t0, now)
	if !(highA > lowA) {
		t.Fatalf("同样 60 天后，高重要性应保留更多活跃度：I=1 -> %v, I=0 -> %v", highA, lowA)
	}
	// 情绪半衰期固定 7 天：60 天后应衰减到极小。
	_, e := p.evolve(1.0, 1.0, 0.5, t0, now)
	if e > 0.01 {
		t.Fatalf("情绪 60 天后应几乎归零，实际 %v", e)
	}
}

// 时间未知（零值）时既不衰减也不放大。
func TestEvolveUnknownTimeKeepsBaseline(t *testing.T) {
	p := DefaultDynamicsParams()
	a, e := p.evolve(0.7, 0.3, 0.5, time.Time{}, time.Now())
	if math.Abs(a-0.7) > 1e-9 || math.Abs(e-0.3) > 1e-9 {
		t.Fatalf("时间未知应保持基准值，实际 A=%v E=%v", a, e)
	}
}

// 强化公式：A' = A + 0.2×(1-A)，且永远不超过 1。
func TestReinforceFormula(t *testing.T) {
	p := DefaultDynamicsParams()
	if got := p.reinforce(0); math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("A=0 强化后应为 0.2，实际 %v", got)
	}
	if got := p.reinforce(0.5); math.Abs(got-0.6) > 1e-9 {
		t.Fatalf("A=0.5 强化后应为 0.6，实际 %v", got)
	}
	if got := p.reinforce(1); math.Abs(got-1) > 1e-9 {
		t.Fatalf("A=1 强化后仍应为 1，实际 %v", got)
	}
	// 连续强化单调收敛到 1，不会越界。
	a := 0.0
	for i := 0; i < 50; i++ {
		a = p.reinforce(a)
	}
	if a <= 0.99 || a > 1.0 {
		t.Fatalf("多次强化应收敛到接近 1，实际 %v", a)
	}
}

// 优先级公式：相关性 × 权重和；权重和为 1 时，满分状态恰好等于相关性。
func TestPriorityFormula(t *testing.T) {
	p := DefaultDynamicsParams()
	full := p.priority(0.8, 1, 1, 1)
	if math.Abs(full-0.8) > 1e-9 {
		t.Fatalf("A=I=E=1 时优先级应等于相关性，实际 %v", full)
	}
	// 零维度只剩基础权重 0.55。
	base := p.priority(1.0, 0, 0, 0)
	if math.Abs(base-0.55) > 1e-9 {
		t.Fatalf("A=I=E=0 时优先级应为 0.55×相关性，实际 %v", base)
	}
	// 单调性：其余不变时，任一维度变大都不该降低优先级。
	if !(p.priority(1, 1, 0.5, 0) > p.priority(1, 0.5, 0.5, 0)) {
		t.Fatal("活跃度更高时优先级应更高")
	}
	if !(p.priority(1, 1, 1, 0) > p.priority(1, 1, 0.5, 0)) {
		t.Fatal("重要性更高时优先级应更高")
	}
	if !(p.priority(1, 1, 0.5, 1) > p.priority(1, 1, 0.5, 0)) {
		t.Fatal("情绪更高时优先级应更高")
	}
}

// scope 加权：同 scope 有加成，空串不加权、不惩罚。
func TestScopeWeight(t *testing.T) {
	if got := scopeWeight("life", "life"); math.Abs(got-scopeBoost) > 1e-9 {
		t.Fatalf("同 scope 应加成 %v，实际 %v", scopeBoost, got)
	}
	if got := scopeWeight("work", "work"); math.Abs(got-scopeBoost) > 1e-9 {
		t.Fatalf("work 同 scope 也应加成，实际 %v", got)
	}
	if got := scopeWeight("work", "life"); got != 1.0 {
		t.Fatalf("不同 scope 不该惩罚，实际 %v", got)
	}
	if got := scopeWeight("", "life"); got != 1.0 {
		t.Fatalf("未分类的旧数据不该被惩罚，实际 %v", got)
	}
	if got := scopeWeight("life", ""); got != 1.0 {
		t.Fatalf("调用方未指定 scope 时不加权，实际 %v", got)
	}
	if got := scopeWeight("  LIFE  ", "life"); math.Abs(got-scopeBoost) > 1e-9 {
		t.Fatalf("scope 应大小写/空白无关，实际 %v", got)
	}
}

// token 估算：中文按字、英文按 4 字符；空串为 0。
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"你好世界", 4},     // 4 个汉字 = 4
		{"abcd", 1},     // 4 字符 = 1
		{"abcde", 2},    // 5 字符向上取整 = 2
		{"中文abc", 3},    // 2 汉字 + ceil(3/4)=1 = 3
		{"12345678", 2}, // 8 字符 = 2
	}
	for _, c := range cases {
		got := estimateTokens(c.in)
		if got != c.want {
			t.Fatalf("estimateTokens(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
	// 单调性：更长（内容更多）不应更少。
	if !(estimateTokens("这是一段更长的中文记忆") > estimateTokens("短")) {
		t.Fatal("更长的文本 token 估算应更大")
	}
}

// 贪心选取：预算刚好够 / 不够 / 空候选。
func TestSelectByTokenBudget(t *testing.T) {
	cands := []Hit{
		{ChunkID: 1, Title: "A", Text: "aaaa"}, // 1 + 1 = 2
		{ChunkID: 2, Title: "B", Text: "bbbb"}, // 2
		{ChunkID: 3, Title: "C", Text: "cccc"}, // 2
	}

	// 空候选：不 panic，返回 (nil, 0)。
	if got, omitted := selectByTokenBudget(nil, 100); got != nil || omitted != 0 {
		t.Fatalf("空候选应返回 (nil,0)，实际 (%v,%d)", got, omitted)
	}

	// 预算刚好够前两条（2+2=4）：第三条因预算省略。
	got, omitted := selectByTokenBudget(cands, 4)
	if len(got) != 2 || got[0].ChunkID != 1 || got[1].ChunkID != 2 {
		t.Fatalf("预算 4 应恰好选前两条，实际 %+v", got)
	}
	if omitted != 1 {
		t.Fatalf("应省略 1 条，实际 %d", omitted)
	}

	// 预算宽裕：全部选上，省略 0。
	got, omitted = selectByTokenBudget(cands, 100)
	if len(got) != 3 || omitted != 0 {
		t.Fatalf("预算宽裕应全选，实际 len=%d omitted=%d", len(got), omitted)
	}

	// 预算连第一条都装不下：仍保留第一条，其余省略（避免注入空块）。
	got, omitted = selectByTokenBudget(cands, 1)
	if len(got) != 1 || got[0].ChunkID != 1 || omitted != 2 {
		t.Fatalf("预算不足应保留第一条并省略其余，实际 %+v omitted=%d", got, omitted)
	}

	// 非正预算：一条都不选，全部省略。
	got, omitted = selectByTokenBudget(cands, 0)
	if len(got) != 0 || omitted != 3 {
		t.Fatalf("预算<=0 应全省略，实际 len=%d omitted=%d", len(got), omitted)
	}
}
