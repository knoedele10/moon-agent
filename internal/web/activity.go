package web

import "time"

// emitToolActivityStart emits the richer activity event alongside the legacy
// tool_call frame. Keeping both frames lets older clients continue to render
// while newer clients can show a real lifecycle with timing and status.
func emitToolActivityStart(sendEvent func(string, any), id, name string, args any, started time.Time) {
	if sendEvent == nil {
		return
	}
	payload := map[string]any{
		"type":      "tool_call",
		"phase":     "tool_call",
		"status":    "running",
		"id":        id,
		"name":      name,
		"arguments": args,
	}
	if !started.IsZero() {
		payload["started_at"] = started.UTC().Format(time.RFC3339Nano)
	}
	sendEvent("activity", payload)
}

// emitToolActivityResult emits the terminal lifecycle event for a tool call.
// A waiting result is distinct from failure: ask_user has not failed, it is
// paused until the user supplies the missing input or confirmation.
func emitToolActivityResult(sendEvent func(string, any), id, name string, started, finished time.Time, ok, waiting bool, summary, detail string, display map[string]any) {
	if sendEvent == nil {
		return
	}
	status := "completed"
	if !ok {
		status = "failed"
	} else if waiting {
		status = "waiting"
	}
	payload := map[string]any{
		"type":    "tool_result",
		"phase":   "tool_result",
		"status":  status,
		"id":      id,
		"name":    name,
		"ok":      ok,
		"summary": summary,
		"detail":  detail,
	}
	if display != nil {
		payload["display"] = display
	}
	if !started.IsZero() {
		payload["started_at"] = started.UTC().Format(time.RFC3339Nano)
	}
	if !finished.IsZero() {
		payload["finished_at"] = finished.UTC().Format(time.RFC3339Nano)
	}
	if !started.IsZero() && !finished.IsZero() && finished.After(started) {
		payload["duration_ms"] = finished.Sub(started).Milliseconds()
	}
	sendEvent("activity", payload)
}

func emitActivityError(sendEvent func(string, any), err error, status string) {
	if sendEvent == nil || err == nil {
		return
	}
	if status == "" {
		status = "failed"
	}
	sendEvent("activity", map[string]any{
		"type":        "error",
		"phase":       "error",
		"status":      status,
		"ok":          false,
		"summary":     err.Error(),
		"detail":      err.Error(),
		"finished_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}
