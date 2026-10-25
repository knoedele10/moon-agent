package web

import (
	"testing"
	"time"
)

func TestToolActivityLifecyclePayload(t *testing.T) {
	var events []map[string]any
	send := func(name string, payload any) {
		if name != "activity" {
			t.Fatalf("activity helper emitted %q", name)
		}
		m, ok := payload.(map[string]any)
		if !ok {
			t.Fatalf("payload type=%T, want map[string]any", payload)
		}
		events = append(events, m)
	}
	started := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	finished := started.Add(1250 * time.Millisecond)
	emitToolActivityStart(send, "c1", "run_command", map[string]any{"command": "dir"}, started)
	emitToolActivityResult(send, "c1", "run_command", started, finished, false, false, "退出码 1", "找不到路径", nil)

	if len(events) != 2 {
		t.Fatalf("got %d activity events, want 2", len(events))
	}
	if got := events[0]["status"]; got != "running" {
		t.Fatalf("start status=%v", got)
	}
	if got := events[0]["phase"]; got != "tool_call" {
		t.Fatalf("start phase=%v", got)
	}
	if got := events[1]["status"]; got != "failed" {
		t.Fatalf("result status=%v", got)
	}
	if got := events[1]["duration_ms"]; got != int64(1250) {
		t.Fatalf("duration_ms=%v, want 1250", got)
	}
}

func TestToolActivityWaitingPayload(t *testing.T) {
	var got map[string]any
	send := func(name string, payload any) {
		if name != "activity" {
			t.Fatalf("activity helper emitted %q", name)
		}
		got = payload.(map[string]any)
	}
	emitToolActivityResult(send, "ask-1", "ask_user", time.Time{}, time.Time{}, true, true, "等待确认", "", nil)
	if got["status"] != "waiting" {
		t.Fatalf("status=%v, want waiting", got["status"])
	}
}
