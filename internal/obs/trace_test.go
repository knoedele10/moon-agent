package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSpanWrittenWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_TRACE_DIR", dir)
	end := Start("chat", map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.request.model":  "mock",
	})
	end(false)
	data, err := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sp Span
	if err := json.Unmarshal(data[:len(data)-1], &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Name != "chat" || sp.Status != "ok" || sp.Attrs["gen_ai.request.model"] != "mock" {
		t.Fatalf("span = %+v", sp)
	}
	t.Setenv("AGENT_TRACE_DIR", "")
	Start("noop", nil)(true)
}
