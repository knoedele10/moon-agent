package evolve

import (
	"strings"
	"testing"
	"time"
)

// TestCandidatesRules 覆盖契约 §4 的三条提炼规则（确定性）。
func TestCandidatesRules(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)

	t.Run("重复成功序列产出技能候选", func(t *testing.T) {
		run := RunRecord{
			RunMeta: RunMeta{RunID: "run-repeat", StartedAt: now},
			ToolCalls: []ToolCallRecord{
				{Name: "fs_read", OK: true}, {Name: "fs_write", OK: true},
				{Name: "fs_read", OK: true}, {Name: "fs_write", OK: true},
			},
			Outcome: "ok",
		}
		cands := Candidates(run)
		if len(cands) != 1 {
			t.Fatalf("候选数 = %d，期望 1（轮转序列合并成一条）: %+v", len(cands), cands)
		}
		c := cands[0]
		if c.Kind != "skill" || !strings.HasPrefix(c.Name, "repeat-") {
			t.Fatalf("候选 = %+v，期望 skill/repeat- 前缀", c)
		}
		if c.Confidence < 0.5 || c.Confidence > 0.9 {
			t.Fatalf("置信度 = %v，期望 [0.5,0.9]", c.Confidence)
		}
		if len(c.Evidence) == 0 {
			t.Fatal("候选必须带证据")
		}
		// 同一轨迹重复调用结果必须一致。
		if second := Candidates(run); len(second) != 1 || second[0].Name != c.Name {
			t.Fatalf("Candidates 不稳定：%v vs %v", cands, second)
		}
	})

	t.Run("失败后成功产出教训候选", func(t *testing.T) {
		run := RunRecord{
			RunMeta: RunMeta{RunID: "run-lesson", StartedAt: now},
			ToolCalls: []ToolCallRecord{
				{Name: "fs_edit", OK: false, IsError: true},
				{Name: "fs_edit", OK: false, IsError: true},
				{Name: "fs_edit", OK: true},
			},
			Outcome: "ok",
		}
		cands := Candidates(run)
		found := false
		for _, c := range cands {
			if c.Name == "lesson-fs-edit" && c.Kind == "skill" {
				found = true
			}
		}
		if !found {
			t.Fatalf("期望 lesson-fs-edit 候选，实际: %+v", cands)
		}
	})

	t.Run("预算触顶产出记忆候选", func(t *testing.T) {
		run := RunRecord{
			RunMeta:   RunMeta{RunID: "run-limit", StartedAt: now},
			ToolCalls: []ToolCallRecord{{Name: "fs_read", OK: true}},
			Outcome:   "limit",
			Note:      "轮数用完",
		}
		cands := Candidates(run)
		if len(cands) != 1 || cands[0].Kind != "memory" {
			t.Fatalf("期望一条 memory 候选，实际: %+v", cands)
		}
	})

	t.Run("用户纠正产出记忆候选", func(t *testing.T) {
		run := RunRecord{
			RunMeta:   RunMeta{RunID: "run-correction", StartedAt: now},
			ToolCalls: []ToolCallRecord{{Name: "fs_read", OK: true}},
			Outcome:   "ok",
			Note:      "用户纠正：不要再用英文回答",
		}
		cands := Candidates(run)
		if len(cands) != 1 || cands[0].Kind != "memory" {
			t.Fatalf("期望一条 memory 候选，实际: %+v", cands)
		}
	})

	t.Run("普通成功运行不产出候选", func(t *testing.T) {
		run := RunRecord{
			RunMeta:   RunMeta{RunID: "run-plain", StartedAt: now},
			ToolCalls: []ToolCallRecord{{Name: "fs_read", OK: true}},
			Outcome:   "ok",
		}
		if cands := Candidates(run); len(cands) != 0 {
			t.Fatalf("不应产出候选，实际: %+v", cands)
		}
	})
}

// TestLedgerStatsAndAtRisk 验证账本聚合与风险名单。
func TestLedgerStatsAndAtRisk(t *testing.T) {
	dir := t.TempDir()
	l := &Ledger{Path: dir + "/ledger.jsonl"}
	now := time.Now()
	for i := 0; i < 4; i++ {
		if err := l.Record(SkillUse{Name: "flaky", OK: i%2 == 0, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Record(SkillUse{Name: "steady", OK: true, At: now}); err != nil {
		t.Fatal(err)
	}
	stats, err := l.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("统计条数 = %d，期望 2", len(stats))
	}
	var flaky SkillStat
	for _, st := range stats {
		if st.Name == "flaky" {
			flaky = st
		}
	}
	if flaky.Uses != 4 || flaky.OK != 2 || flaky.Fail != 2 {
		t.Fatalf("flaky 统计 = %+v，期望 uses=4 ok=2 fail=2", flaky)
	}
	if flaky.SuccessRate != 0.5 {
		t.Fatalf("成功率 = %v，期望 0.5", flaky.SuccessRate)
	}
	one, ok, err := l.Stat("steady")
	if err != nil || !ok || one.Uses != 1 {
		t.Fatalf("Stat 单条失败: %+v ok=%v err=%v", one, ok, err)
	}
	if _, ok, _ := l.Stat("missing"); ok {
		t.Fatal("不存在的技能不应返回 ok=true")
	}
	risky, err := l.AtRisk(3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(risky) != 1 || risky[0].Name != "flaky" {
		t.Fatalf("风险名单 = %+v，期望只有 flaky", risky)
	}
	if err := l.Record(SkillUse{Name: ""}); err == nil {
		t.Fatal("空技能名应报错")
	}
}
