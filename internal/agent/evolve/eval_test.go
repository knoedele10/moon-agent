package evolve

import (
	"strings"
	"testing"
)

// TestScoreDeterministic 覆盖契约 §7.9：给定轨迹与用例，Score 输出确定值（表驱动）。
func TestScoreDeterministic(t *testing.T) {
	okRun := RunRecord{
		RunMeta: RunMeta{RunID: "r1"},
		ToolCalls: []ToolCallRecord{
			{Name: "fs_read", OK: true},
			{Name: "fs_write", OK: true},
		},
		Outcome: "ok",
	}
	halfFail := RunRecord{
		RunMeta: RunMeta{RunID: "r2"},
		ToolCalls: []ToolCallRecord{
			{Name: "fs_read", OK: true},
			{Name: "fs_write", OK: false, IsError: true},
		},
		Outcome: "ok",
	}
	limitRun := RunRecord{
		RunMeta:   RunMeta{RunID: "r3"},
		ToolCalls: halfFail.ToolCalls,
		Outcome:   "limit",
	}
	emptyOK := RunRecord{RunMeta: RunMeta{RunID: "r4"}, Outcome: "ok"}
	emptyErr := RunRecord{RunMeta: RunMeta{RunID: "r5"}, Outcome: "error"}

	cases := []struct {
		name string
		run  RunRecord
		c    EvalCase
		want float64
	}{
		{"全部命中且无失败", okRun, EvalCase{ID: "c1", WantTools: []string{"fs_read", "fs_write"}}, 1},
		{"命中一半", okRun, EvalCase{ID: "c2", WantTools: []string{"fs_read", "missing"}}, 0.7},
		{"无期望工具按满分算命中", okRun, EvalCase{ID: "c3"}, 1},
		{"一半调用失败", halfFail, EvalCase{ID: "c4", WantTools: []string{"fs_read", "fs_write"}}, 0.8},
		{"失败率超上限再减半", halfFail, EvalCase{ID: "c5", WantTools: []string{"fs_read", "fs_write"}, MaxFailRate: 0.3}, 0.4},
		{"预算触顶减半", limitRun, EvalCase{ID: "c6", WantTools: []string{"fs_read", "fs_write"}}, 0.4},
		{"触顶且失败率超限", limitRun, EvalCase{ID: "c7", WantTools: []string{"fs_read", "fs_write"}, MaxFailRate: 0.3}, 0.2},
		{"空运行无期望", emptyOK, EvalCase{ID: "c8"}, 1},
		{"空运行且出错减半", emptyErr, EvalCase{ID: "c9"}, 0.5},
		{"期望工具去重后按唯一项算", okRun, EvalCase{ID: "c10", WantTools: []string{"fs_read", "fs_read", "fs_write"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.run, tc.c)
			if got != tc.want {
				t.Fatalf("Score = %v，期望 %v", got, tc.want)
			}
			// 同一输入必须每次得到同一结果（确定性）。
			if second := Score(tc.run, tc.c); second != got {
				t.Fatalf("Score 不稳定：%v vs %v", got, second)
			}
		})
	}
}

// TestScoreNotes 验证扣分原因可解释。
func TestScoreNotes(t *testing.T) {
	run := RunRecord{
		RunMeta: RunMeta{RunID: "r"},
		ToolCalls: []ToolCallRecord{
			{Name: "fs_read", OK: false, IsError: true},
			{Name: "fs_write", OK: false, IsError: true},
		},
		Outcome: "interrupted",
	}
	score, notes := ScoreWithNotes(run, EvalCase{ID: "c", WantTools: []string{"fs_read"}})
	if score != 0.3 { // 0.6*1 + 0.4*0 = 0.6；中断减半 = 0.3；未设 MaxFailRate 不再减半
		t.Fatalf("Score = %v，期望 0.3", score)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"失败调用", "outcome=interrupted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Notes 缺少 %q: %v", want, notes)
		}
	}
}

// TestAggregate 验证平均分与样本数。
func TestAggregate(t *testing.T) {
	if mean, n := Aggregate(nil); mean != 0 || n != 0 {
		t.Fatalf("空输入 = (%v,%d)，期望 (0,0)", mean, n)
	}
	results := []EvalResult{
		{Score: 1},
		{Score: 0.7},
		{Score: 0.4},
	}
	mean, n := Aggregate(results)
	if n != 3 {
		t.Fatalf("样本数 = %d，期望 3", n)
	}
	if mean != 0.7 {
		t.Fatalf("平均分 = %v，期望 0.7", mean)
	}
}
