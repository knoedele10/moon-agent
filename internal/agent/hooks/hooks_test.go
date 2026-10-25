package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeHooks(t *testing.T, root string, file File) {
	t.Helper()
	dir := filepath.Join(root, ".moon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingHooksAllowEverything(t *testing.T) {
	d := PreToolUse(context.Background(), t.TempDir(), "write", "{}")
	if !d.Allow {
		t.Fatal(d.Reason)
	}
}

func TestPreToolUseCanDeny(t *testing.T) {
	root := t.TempDir()
	writeHooks(t, root, File{Hooks: []Hook{{
		Phase: "PreToolUse", Command: []string{"/bin/false"}, Tools: []string{"write"},
	}}})
	if d := PreToolUse(context.Background(), root, "write", "{}"); d.Allow {
		t.Fatal("false hook should deny write")
	}
	if d := PreToolUse(context.Background(), root, "read", "{}"); !d.Allow {
		t.Fatalf("read is not in the tool filter: %s", d.Reason)
	}
	writeHooks(t, root, File{Hooks: []Hook{{
		Phase: "PreToolUse", Command: []string{"/bin/true"},
	}}})
	if d := PreToolUse(context.Background(), root, "write", "{}"); !d.Allow {
		t.Fatal(d.Reason)
	}
}
