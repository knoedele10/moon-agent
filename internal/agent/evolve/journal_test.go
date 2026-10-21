package evolve

import (
	"errors"
	"testing"
)

func TestRollbackRestoresBatchAndDoesNotRepeat(t *testing.T) {
	before := validSkill("existing")
	after := before
	after.Source = "evolve"
	after.Body += "\n自动补充的步骤"
	created := validSkill("created")
	created.Source = "evolve"
	store := newFakeStore(after, created)
	journal := NewJournal(t.TempDir())
	for _, entry := range []JournalEntry{
		{Batch: "batch", Kind: JournalSkill, Name: before.Name, Before: &before, After: after},
		{Batch: "batch", Kind: JournalSkill, Name: created.Name, After: created},
	} {
		if err := journal.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := RollbackLast(journal, store, "u1")
	if err != nil || len(actions) != 2 {
		t.Fatalf("回滚失败：%v %v", actions, err)
	}
	got, _, _ := store.Get("u1", before.Name)
	if got.Body != before.Body {
		t.Fatal("没有恢复原正文")
	}
	got, _, _ = store.Get("u1", created.Name)
	if got.Enabled || got.Source != "evolve:rolledback" {
		t.Fatalf("新建技能应保留归档证据：%+v", got)
	}
	if _, err := RollbackLast(journal, store, "u1"); !errors.Is(err, ErrAlreadyRolledBack) {
		t.Fatalf("重复回滚没有被识别：%v", err)
	}
}

func TestRollbackDoesNotOverwriteLaterUserEdits(t *testing.T) {
	after := validSkill("later-edited")
	after.Source = "evolve"
	current := after
	current.Body += "\n用户后来增加的重要内容"
	store := newFakeStore(current)
	journal := NewJournal(t.TempDir())
	if err := journal.Append(JournalEntry{Batch: "b", Kind: JournalSkill, Name: after.Name, After: after}); err != nil {
		t.Fatal(err)
	}
	if actions, err := RollbackLast(journal, store, "u1"); err == nil || len(actions) != 0 {
		t.Fatalf("回滚覆盖用户后改内容：%v %v", actions, err)
	}
	got, _, _ := store.Get("u1", current.Name)
	if got.Body != current.Body {
		t.Fatal("用户后改内容丢失")
	}
}

func TestRollbackLegacyJournalOnlyTouchesLastEntry(t *testing.T) {
	a, b := validSkill("first"), validSkill("last")
	store := newFakeStore(a, b)
	journal := NewJournal(t.TempDir())
	for _, item := range []SkillItem{a, b} {
		if err := journal.Append(JournalEntry{Kind: JournalSkill, Name: item.Name, After: item}); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := RollbackLast(journal, store, "u1")
	if err != nil || len(actions) != 1 {
		t.Fatalf("无批次日志影响了旧条目：%v %v", actions, err)
	}
	got, _, _ := store.Get("u1", a.Name)
	if !got.Enabled {
		t.Fatal("第一条旧技能不应被回滚")
	}
}
