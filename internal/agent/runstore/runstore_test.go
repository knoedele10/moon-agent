package runstore

import (
	"os"
	"testing"
)

func TestJournalSurvivesAndRecoversRunningRun(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("run/1", "session-1", map[string]any{"model": "mock"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("run/1", "session-1", "tool_call", "", map[string]any{"tool": "read"}); err != nil {
		t.Fatal(err)
	}
	recoverable, err := s.Recoverable()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].RunID != "run/1" || recoverable[0].Status != StatusRunning {
		t.Fatalf("unexpected recoverable runs: %+v", recoverable)
	}
	events, err := s.Read("run/1")
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	if err := s.Finish("run/1", "session-1", StatusDone, "ok"); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil || len(all) != 1 || all[0].Status != StatusDone {
		t.Fatalf("finished summary = %+v, err = %v", all, err)
	}
	if _, err := os.Stat(all[0].Path); err != nil {
		t.Fatal(err)
	}
}

func TestJournalRejectsEmptyRunID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("", "", "event", "", nil); err == nil {
		t.Fatal("empty run id should fail")
	}
}
