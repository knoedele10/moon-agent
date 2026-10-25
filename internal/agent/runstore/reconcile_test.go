package runstore

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func ev(kind, tool, id, effect string, status Status) Event {
	payload, _ := json.Marshal(map[string]string{"id": id, "effect": effect})
	return Event{At: time.Now().UTC(), RunID: "run", Type: kind, Tool: tool, Status: status, Payload: payload}
}

func TestClassifyDoesNotReplayUnknownEffects(t *testing.T) {
	running := []Event{
		{Type: "run_started", Status: StatusRunning},
		ev("tool_started", "read", "1", "read_only", ""),
		ev("tool_finished", "read", "1", "read_only", ""),
	}
	if got := Classify(running); got != StatusRecovered {
		t.Fatalf("read-only run = %s", got)
	}
	unknown := append(running, ev("tool_started", "delete", "2", "non_idempotent_side_effect", ""))
	if got := Classify(unknown); got != StatusNeedsConfirm {
		t.Fatalf("unfinished delete = %s", got)
	}
	done := []Event{{Type: "run_finished", Status: StatusDone}}
	if got := Classify(done); got != StatusDone {
		t.Fatalf("finished run = %s", got)
	}
}

func TestReconcileMarksRecoveredWithoutCreatingFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Start("run-a", "s", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("run-a", "s", "tool_finished", StatusRunning, map[string]string{"id": "1", "effect": "read_only"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != StatusRecovered {
		t.Fatalf("reconcile = %+v", got)
	}
	events, err := store.Read("run-a")
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != "run_reconciled" || last.Status != StatusRecovered {
		t.Fatalf("last = %+v", last)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "*.jsonl")); err != nil {
		t.Fatal(err)
	}
}
