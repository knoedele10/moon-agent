package evolve

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCandidatesRequireActualRecovery(t *testing.T) {
	for _, run := range []RunRecord{
		{Outcome: "ok", ToolCalls: []ToolCallRecord{{Name: "read", OK: true}, {Name: "read", IsError: true}, {Name: "read", IsError: true}}},
		{Outcome: "error", ToolCalls: []ToolCallRecord{{Name: "read", IsError: true}, {Name: "read", IsError: true}, {Name: "read", OK: true}}},
	} {
		for _, cand := range Candidates(run) {
			if cand.Kind == "skill" {
				t.Fatalf("未恢复的任务不应产出技能：%+v", cand)
			}
		}
	}
}

func TestCrossRunLearningPreservesVerifiedArguments(t *testing.T) {
	calls := []ToolCallRecord{
		{Name: "read_file", Args: `{"path":"settings.json"}`, OK: true},
		{Name: "search_files", Args: `{"query":"enabled","path":"src"}`, OK: true},
	}
	runs := []RunRecord{{RunMeta: RunMeta{RunID: "first"}, Outcome: "ok", ToolCalls: calls}, {RunMeta: RunMeta{RunID: "second"}, Outcome: "ok", ToolCalls: calls}}
	candidates := CrossRunCandidates(runs)
	if len(candidates) != 1 || !candidates[0].AutoApplicable {
		t.Fatalf("应提炼一个有完整证据的候选：%+v", candidates)
	}
	if !strings.Contains(candidates[0].Body, "settings.json") || !strings.Contains(candidates[0].Body, "## 验证") {
		t.Fatalf("技能缺少成功参数或验证方法：%s", candidates[0].Body)
	}
	store := newFakeStore()
	cur := &Curator{Store: store, Apply: true, ManagedOnly: true}
	applied, err := cur.apply("u1", nil, nil, candidates)
	if err != nil || len(applied) != 1 {
		t.Fatalf("固化失败：%v %v", applied, err)
	}
	applied, err = cur.apply("u1", nil, nil, candidates)
	if err != nil || len(applied) != 0 {
		t.Fatalf("重复固化不幂等：%v %v", applied, err)
	}
}

func TestLegacyTrajectoryCannotAutoCreateSkeletonSkill(t *testing.T) {
	run := RunRecord{Outcome: "ok", ToolCalls: []ToolCallRecord{{Name: "read", OK: true}, {Name: "write", OK: true}, {Name: "read", OK: true}, {Name: "write", OK: true}}}
	candidates := Candidates(run)
	if len(candidates) == 0 {
		t.Fatal("旧轨迹仍应提供建议")
	}
	cur := &Curator{Store: newFakeStore(), Apply: true}
	applied, err := cur.apply("u1", nil, nil, candidates)
	if err != nil || len(applied) != 0 {
		t.Fatalf("无参数骨架被当作技能固化：%v %v", applied, err)
	}
}

func TestManagedCurationProtectsUserSkills(t *testing.T) {
	owned := validSkill("user-authored")
	owned.Source = "user"
	store := newFakeStore(owned)
	cur := &Curator{Store: store, Apply: true, ManagedOnly: true}
	cand := Candidate{Kind: "skill", Name: owned.Name, Description: owned.Description, Body: owned.Body + "\n新的内容", AutoApplicable: true}
	applied, err := cur.apply("u1", []SkillItem{owned}, nil, []Candidate{cand})
	if err != nil || len(applied) != 0 {
		t.Fatalf("用户技能被覆盖：%v %v", applied, err)
	}
	got, _, _ := store.Get("u1", owned.Name)
	if got.Body != owned.Body {
		t.Fatal("用户原文改变")
	}
}

func TestRecorderRedactsNestedCredentials(t *testing.T) {
	recorder := NewRecorder(t.TempDir())
	run := recorder.Start(RunMeta{RunID: "private"})
	run.Tool(ToolCallRecord{Name: "request", OK: true, Args: `{"nested":{"password":"short","token":"opaque"},"path":"ok"}`, Summary: "password=another-secret"})
	if err := run.Finish("ok", "api_key=some-private-value"); err != nil {
		t.Fatal(err)
	}
	rows, err := recorder.List(0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取失败：%v %+v", err, rows)
	}
	data, _ := json.Marshal(rows)
	for _, secret := range []string{"short", "opaque", "another-secret", "some-private-value"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("轨迹泄漏凭证：%q", secret)
		}
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(rows[0].ToolCalls[0].Args), &object); err != nil {
		t.Fatalf("脱敏破坏 JSON：%v", err)
	}
}
