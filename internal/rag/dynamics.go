package rag

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// 本文件实现「记忆召回公式」（借鉴 AAAAGENT 的活跃度/重要性/情绪模型）：
//
//	活跃度半衰期 H = 30 × (1 + 2×重要性) 天     // 重要性越高衰减越慢
//	活动度 A(t)   = A₀ × 2^(-t/H)              // 稳定档案类恒为 1（A₀=1）
//	情绪强度 E(t) = E₀ × 2^(-t/7)
//	优先级 P      = 相关性 × (0.55 + 0.25A + 0.15I + 0.05E)
//	强化 A'       = A + 0.2 × (1 - A)
//
// 全部实现成**纯函数**，不碰数据库、不读时钟（时间由调用方传入），
// 这样公式本身可以离线单测，检索层只负责把库里的值喂进来。
//
// 与旧的「7 天内满分、之后最多衰减 15%」相比，这条公式多了两个维度：
//   - 重要性 I：重要的事衰减更慢，不会被新写入的琐事盖过；
//   - 情绪 E：情绪强度衰减快（半衰期 7 天），让「刚刚发生的强烈感受」优先浮现。
//
// 铁律：**自然淡化只影响排序，绝不删除行**。活跃度再低，行也照样留在库里。

// DynamicsParams 是召回公式的全部参数。默认值见 DefaultDynamicsParams。
//
// 四个权重（BaseWeight/ActivityWeight/ImportanceWeight/EmotionWeight）之和必须等于 1，
// 这样优先级才落在「相关性 × [0,1]」的量纲上，不会因为调参把相关度淹掉。
type DynamicsParams struct {
	// ActivityHalfLifeBaseDays 是重要度为 0 时的活跃度半衰期（天）。
	ActivityHalfLifeBaseDays float64
	// ImportanceHalfLifeGain 让半衰期随重要性线性拉长：H = base × (1 + gain×I)。
	ImportanceHalfLifeGain float64
	// EmotionHalfLifeDays 是情绪强度的半衰期（天），衰减比活跃度快。
	EmotionHalfLifeDays float64

	// BaseWeight / ActivityWeight / ImportanceWeight / EmotionWeight 是优先级公式的四项权重。
	BaseWeight       float64
	ActivityWeight   float64
	ImportanceWeight float64
	EmotionWeight    float64

	// ReinforceGain 是强化增益：被明确重提时 A ← A + gain×(1-A)。
	ReinforceGain float64
}

// DefaultDynamicsParams 返回公式的默认参数，并在构造时断言权重之和为 1。
func DefaultDynamicsParams() DynamicsParams {
	p := DynamicsParams{
		ActivityHalfLifeBaseDays: 30,
		ImportanceHalfLifeGain:   2,
		EmotionHalfLifeDays:      7,
		BaseWeight:               0.55,
		ActivityWeight:           0.25,
		ImportanceWeight:         0.15,
		EmotionWeight:            0.05,
		ReinforceGain:            0.2,
	}
	if err := p.validate(); err != nil {
		// 默认参数写错属于编程错误：宁可在启动/首次调用时炸掉，也不要静默用错公式。
		panic("rag: 默认记忆动力学参数非法：" + err.Error())
	}
	return p
}

// validate 校验参数自洽：权重之和为 1，其余参数为正。
func (p DynamicsParams) validate() error {
	sum := p.BaseWeight + p.ActivityWeight + p.ImportanceWeight + p.EmotionWeight
	if math.Abs(sum-1.0) > 1e-9 {
		return fmt.Errorf("四项权重之和必须为 1，实际 %v", sum)
	}
	if p.ActivityHalfLifeBaseDays <= 0 || p.ImportanceHalfLifeGain < 0 || p.EmotionHalfLifeDays <= 0 {
		return fmt.Errorf("半衰期参数必须为正：base=%v gain=%v emotion=%v",
			p.ActivityHalfLifeBaseDays, p.ImportanceHalfLifeGain, p.EmotionHalfLifeDays)
	}
	if p.ReinforceGain < 0 || p.ReinforceGain > 1 {
		return fmt.Errorf("强化增益应在 [0,1]，实际 %v", p.ReinforceGain)
	}
	return nil
}

// halfLife 返回给定重要性下的活跃度半衰期（天）：H = base × (1 + gain×I)。
func (p DynamicsParams) halfLife(importance float64) float64 {
	i := clamp01(importance)
	h := p.ActivityHalfLifeBaseDays * (1 + p.ImportanceHalfLifeGain*i)
	if h <= 0 {
		// 参数被外部改坏时的兜底：至少保证半衰期为正，避免除零。
		return 1
	}
	return h
}

// decay 按半衰期做指数衰减：a0 × 2^(-elapsed/halfLife)。
// elapsed<=0 时返回初始值（不放大）；halfLife<=0 视为参数错误，返回 0。
func (p DynamicsParams) decay(a0, halfLifeDays, elapsedDays float64) float64 {
	a0 = clamp01(a0)
	if elapsedDays <= 0 {
		return a0
	}
	if halfLifeDays <= 0 {
		return 0
	}
	return a0 * math.Pow(2, -elapsedDays/halfLifeDays)
}

// reinforce 计算一次「明确重提」后的活跃度：A + gain×(1-A)，并夹在 [0,1]。
func (p DynamicsParams) reinforce(activity float64) float64 {
	a := clamp01(activity)
	return clamp01(a + p.ReinforceGain*(1-a))
}

// priority 计算最终优先级：相关性 × (Base + wA×A + wI×I + wE×E)。
func (p DynamicsParams) priority(relevance, activity, importance, emotion float64) float64 {
	w := p.BaseWeight +
		p.ActivityWeight*clamp01(activity) +
		p.ImportanceWeight*clamp01(importance) +
		p.EmotionWeight*clamp01(emotion)
	return relevance * w
}

// evolve 把「写入时的基准值」推进到 now：返回当前的活跃度 A(t) 与情绪强度 E(t)。
//
// t0 是衰减起点：有强化时间用强化时间，否则用写入时间；零值表示时间未知，
// 此时不衰减（保持基准值），与旧 recencyFactor「时间未知不加不减」的口径一致。
func (p DynamicsParams) evolve(activity, emotion, importance float64, t0, now time.Time) (a, e float64) {
	if t0.IsZero() {
		return clamp01(activity), clamp01(emotion)
	}
	elapsed := now.Sub(t0).Hours() / 24
	if elapsed < 0 {
		elapsed = 0
	}
	a = p.decay(activity, p.halfLife(importance), elapsed)
	e = p.decay(emotion, p.EmotionHalfLifeDays, elapsed)
	return a, e
}

// clamp01 把数值夹到 [0,1]。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// snapImportance 把模型给的任意重要性归一到 0 / 0.5 / 1 三档（就近取整），
// 避免模型返回 0.7 这类噪声值把权重调乱。范围外的值先夹到 [0,1]。
func snapImportance(v float64) float64 {
	v = clamp01(v)
	switch {
	case v < 0.25:
		return 0
	case v < 0.75:
		return 0.5
	default:
		return 1
	}
}

// ---- scope（陪伴记忆 / 工作记忆） ----

// ScopeLife / ScopeWork 是 scope 的两个合法取值；空串表示「未分类」。
const (
	ScopeLife = "life"
	ScopeWork = "work"
)

// scopeBoost 是同 scope 命中的加成系数。取 1.15 是刻意的小幅加成：
// 陪伴时优先生活记忆，但不会把「确实相关的工作事实」完全挡在外面。
const scopeBoost = 1.15

// normalizeScope 归一化 scope：只认 life / work，其余（含空串、旧数据）一律归为未分类 ""。
func normalizeScope(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ScopeLife:
		return ScopeLife
	case ScopeWork:
		return ScopeWork
	default:
		return ""
	}
}

// scopeWeight 返回 scope 加权系数：
//   - 查询未指定 scope（空串）→ 1.0（不加权）；
//   - 文档未分类（空串）→ 1.0（旧数据不受惩罚）；
//   - 文档与查询同 scope → scopeBoost；
//   - 不同 scope → 1.0（不惩罚，只是没有加成）。
func scopeWeight(docScope, queryScope string) float64 {
	d, q := normalizeScope(docScope), normalizeScope(queryScope)
	if d == "" || q == "" {
		return 1.0
	}
	if d == q {
		return scopeBoost
	}
	return 1.0
}

// ---- token 预算与贪心选取 ----

// estimateTokens 是注入文本的 token 近似估算，**不引入 tokenizer 依赖**：
//   - 中文（汉字）按 1 字 ≈ 1 token；
//   - 其余字符（英文/数字/标点/空白）按 4 字符 ≈ 1 token，向上取整。
//
// 只需要相对量级正确即可：预算是软上限，估算误差不会破坏正确性。
func estimateTokens(s string) int {
	han, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			han++
		} else {
			other++
		}
	}
	return han + (other+3)/4
}

// hitTokenCost 估算一条命中注入后的 token 开销（标题 + 正文）。
func hitTokenCost(h Hit) int {
	return estimateTokens(h.Title) + estimateTokens(strings.TrimSpace(h.Text))
}

// selectByTokenBudget 按给定顺序（调用方已按 priority 降序排好）贪心选取命中，
// 逐条累加估算 token，超出预算就停。返回选中的命中与**因预算省略的条数**。
//
// 边界行为：
//   - 空候选 → (nil, 0)；
//   - budget<=0 → 一条都不选，全部记为省略（调用方应先用默认值兜底）；
//   - 第一条就超预算 → 仍然带上第一条（否则注入块会凭空消失），其余全部省略；
//   - 后面的条目超预算 → 从该条起全部省略。
func selectByTokenBudget(cands []Hit, budget int) ([]Hit, int) {
	if len(cands) == 0 {
		return nil, 0
	}
	if budget <= 0 {
		return nil, len(cands)
	}

	picked := make([]Hit, 0, len(cands))
	used := 0
	for i, h := range cands {
		cost := hitTokenCost(h)
		if used+cost > budget {
			if len(picked) == 0 {
				// 预算连最高优先级的一条都装不下：至少保留它，避免「明明有记忆却注入空块」。
				return []Hit{h}, len(cands) - 1
			}
			return picked, len(cands) - i
		}
		picked = append(picked, h)
		used += cost
	}
	return picked, 0
}
