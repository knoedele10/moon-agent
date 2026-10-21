package evolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 变更日志：追加、读回、坏行跳过。
func TestJournalAppendAndList(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir)
	if err := j.Append(JournalEntry{Batch: "b1", Kind: JournalArchive, Name: "alpha"}); err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	if err := j.Append(JournalEntry{Batch: "b1", Kind: JournalSkill, Name: "beta"}); err != nil {
		t.Fatalf("追加失败：%v", err)
	}
	// 手动塞一行坏数据：读的时候要跳过，而不是整份日志报废。
	f, err := os.OpenFile(filepath.Join(dir, journalFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{这不是 JSON\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	entries, err := j.List(10)
	if err != nil {
		t.Fatalf("List 失败：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("条数 = %d，期望 2（坏行跳过）：%+v", len(entries), entries)
	}
	if entries[0].Name != "alpha" || entries[1].Name != "beta" {
		t.Fatalf("顺序不对：%+v", entries)
	}
	if entries[0].At.IsZero() {
		t.Fatal("Append 应自动盖时间戳")
	}
}

// 归档回滚：把技能恢复成变更前的样子，且同一批不能回滚两次。
func TestRollbackRestoresArchivedSkill(t *testing.T) {
	st := newMemStore(duplicateSkills()...)
	dir := t.TempDir()
	j := NewJournal(dir)
	c := &Curator{Store: st, Recorder: NewRecorder(dir), Apply: true, Journal: j}
	rep, err := c.Run(t.Context(), "u1")
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if len(rep.Applied) != 1 {
		t.Fatalf("应先归档 1 条：%v", rep.Applied)
	}
	archived := strings.TrimPrefix(rep.Applied[0], "archive:")

	actions, err := RollbackLast(j, st, "u1")
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	if len(actions) != 1 || actions[0] != "restore:"+archived {
		t.Fatalf("回滚动作 = %v", actions)
	}
	if !st.enabled(archived) {
		t.Fatalf("%q 应恢复启用", archived)
	}
	if _, err := RollbackLast(j, st, "u1"); !errors.Is(err, ErrAlreadyRolledBack) {
		t.Fatalf("第二次回滚应返回 ErrAlreadyRolledBack，实际 %v", err)
	}
}

// 新建技能的回滚：不物理删除，改为归档并标来源 rolledback。
func TestRollbackDisablesCreatedSkill(t *testing.T) {
	st := newMemStore()
	dir := t.TempDir()
	j := NewJournal(dir)
	c := &Curator{Store: st, Apply: true, Journal: j}
	cand := Candidate{
		Kind:           "skill",
		Name:           "fresh",
		Description:    "一次成功流程沉淀出来的新技能，可复用",
		Body:           "先读配置，再改字段，最后跑测试。",
		AutoApplicable: true,
	}
	applied, err := c.apply("u1", nil, nil, []Candidate{cand}, "batch-1")
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if len(applied) != 1 || applied[0] != "skill:fresh" {
		t.Fatalf("应先落地新技能：%v", applied)
	}
	actions, err := RollbackLast(j, st, "u1")
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	if len(actions) != 1 || actions[0] != "disable:fresh" {
		t.Fatalf("回滚动作 = %v", actions)
	}
	got, ok, _ := st.Get("u1", "fresh")
	if !ok || got.Enabled {
		t.Fatalf("新建技能回滚后应被归档：%+v", got)
	}
	if got.Source != "evolve:rolledback" {
		t.Fatalf("来源应标记回滚：%q", got.Source)
	}
}

// 没有日志时回滚要说人话，而不是 panic 或静默成功。
func TestRollbackWithoutJournal(t *testing.T) {
	j := NewJournal(t.TempDir())
	if _, err := RollbackLast(j, newMemStore(), "u1"); !errors.Is(err, ErrNothingToRollback) {
		t.Fatalf("期望 ErrNothingToRollback，实际 %v", err)
	}
}

// 覆盖已有技能的回滚：写回变更前的正文与描述。
func TestRollbackRestoresOverwrittenSkill(t *testing.T) {
	old := SkillItem{Name: "cfg", Description: "旧的描述足够长可以过体检", Body: "旧正文", Enabled: true}
	st := newMemStore(old)
	dir := t.TempDir()
	j := NewJournal(dir)
	c := &Curator{Store: st, Apply: true, Journal: j}
	cand := Candidate{Kind: "skill", Name: "cfg", Description: "新的描述足够长可以过体检", Body: "新正文", AutoApplicable: true}
	if _, err := c.apply("u1", []SkillItem{old}, nil, []Candidate{cand}, "batch-2"); err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if got, _, _ := st.Get("u1", "cfg"); got.Body != "新正文" {
		t.Fatalf("应已覆盖：%+v", got)
	}
	if _, err := RollbackLast(j, st, "u1"); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	got, _, _ := st.Get("u1", "cfg")
	if got.Body != "旧正文" || got.Description != old.Description {
		t.Fatalf("应写回旧内容：%+v", got)
	}
}

// 批次隔离：只回滚最近一批，更早的改动不能被动。
func TestRollbackOnlyTouchesLatestBatch(t *testing.T) {
	st := newMemStore()
	j := NewJournal(t.TempDir())
	j.Now = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	c := &Curator{Store: st, Apply: true, Journal: j}
	first := Candidate{Kind: "skill", Name: "one", Description: "第一批新增的技能描述够长", Body: "正文一", AutoApplicable: true}
	second := Candidate{Kind: "skill", Name: "two", Description: "第二批新增的技能描述够长", Body: "正文二", AutoApplicable: true}
	if _, err := c.apply("u1", nil, nil, []Candidate{first}, "batch-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.apply("u1", nil, nil, []Candidate{second}, "batch-B"); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackLast(j, st, "u1"); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	if got, _, _ := st.Get("u1", "one"); !got.Enabled {
		t.Fatal("更早一批的技能不该被回滚")
	}
	if got, _, _ := st.Get("u1", "two"); got.Enabled {
		t.Fatal("最近一批应被回滚")
	}
}
