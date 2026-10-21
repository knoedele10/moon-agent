package evolve

import (
	"fmt"
	"math"
	"strings"
)

// EvalCase 是一个回放评测用例：期望用到的工具 + 可容忍的失败率。
type EvalCase struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt,omitempty"`
	WantTools   []string `json:"want_tools,omitempty"`
	MaxFailRate float64  `json:"max_fail_rate,omitempty"`
}

// EvalResult 是一条轨迹在一个用例上的评测结果。
type EvalResult struct {
	Case  EvalCase  `json:"case"`
	Run   RunRecord `json:"run"`
	Score float64   `json:"score"`
	Notes []string  `json:"notes,omitempty"`
}

const (
	// evalHitWeight / evalSuccessWeight 是「工具命中」与「调用不失败」在基础分里的权重。
	evalHitWeight     = 0.6
	evalSuccessWeight = 0.4
	// evalAbortPenalty / evalFailRatePenalty 是两类扣分的乘数（各扣一半）。
	evalAbortPenalty    = 0.5
	evalFailRatePenalty = 0.5
)

// Score 给一次运行按用例打分，返回 [0,1] 的确定值（无随机、无时间依赖）。
//
// 公式（固定，便于回归对比）：
//
//	命中率 = 期望工具中实际出现过的比例（没有期望工具时记 1）
//	失败率 = 失败调用 / 全部调用（没有调用时记 0）
//	基础分 = 0.6*命中率 + 0.4*(1-失败率)
//	运行被中断/错误/触顶 → 基础分 ×0.5
//	失败率超过用例 MaxFailRate → 再 ×0.5
//
// 结果四舍五入到 4 位小数，避免浮点尾差让快照测试抖动。
func Score(run RunRecord, c EvalCase) float64 {
	score, _ := ScoreWithNotes(run, c)
	return score
}

// ScoreWithNotes 与 Score 同分，但额外返回扣分原因（给报告/界面展示用）。
func ScoreWithNotes(run RunRecord, c EvalCase) (float64, []string) {
	notes := make([]string, 0, 4)
	calls := run.ToolCalls
	fails := 0
	used := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		used[call.Name] = struct{}{}
		if call.IsError || !call.OK {
			fails++
		}
	}
	failRate := 0.0
	if len(calls) > 0 {
		failRate = float64(fails) / float64(len(calls))
	}
	wanted := make([]string, 0, len(c.WantTools))
	seen := map[string]struct{}{}
	for _, name := range c.WantTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		wanted = append(wanted, name)
	}
	hitRate := 1.0
	if len(wanted) > 0 {
		hit := 0
		for _, name := range wanted {
			if _, ok := used[name]; ok {
				hit++
				continue
			}
			notes = append(notes, fmt.Sprintf("缺少期望工具 %q", name))
		}
		hitRate = float64(hit) / float64(len(wanted))
	}
	score := evalHitWeight*hitRate + evalSuccessWeight*(1-failRate)
	if fails > 0 {
		notes = append(notes, fmt.Sprintf("失败调用 %d/%d（失败率 %.2f）", fails, len(calls), failRate))
	}
	if isAbortedOutcome(run.Outcome) {
		score *= evalAbortPenalty
		notes = append(notes, fmt.Sprintf("运行未正常收尾（outcome=%s），分数减半", run.Outcome))
	}
	if c.MaxFailRate > 0 && failRate > c.MaxFailRate {
		score *= evalFailRatePenalty
		notes = append(notes, fmt.Sprintf("失败率 %.2f 超过用例上限 %.2f，再减半", failRate, c.MaxFailRate))
	}
	return round4(clamp01(score)), notes
}

// isAbortedOutcome 判断运行是否属于「没正常跑完」。
func isAbortedOutcome(outcome string) bool {
	switch outcome {
	case "error", "limit", "interrupted":
		return true
	default:
		return false
	}
}

// round4 四舍五入到 4 位小数。
func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// Aggregate 计算一组评测结果的平均分与样本数；空输入返回 (0, 0)。
func Aggregate(results []EvalResult) (mean float64, n int) {
	if len(results) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, r := range results {
		sum += r.Score
	}
	return round4(sum / float64(len(results))), len(results)
}
