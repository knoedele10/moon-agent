package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldDetectsGoAndDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, created, err := Scaffold(dir, false)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "go test ./...") || !strings.Contains(string(data), "moon:verify") {
		t.Fatalf("template missing verify: %s", data)
	}
	if _, created, err := Scaffold(dir, false); err != nil || created {
		t.Fatalf("second call created=%v err=%v", created, err)
	}
}

func TestVerifyCommandsAndOncePerTurn(t *testing.T) {
	ResetVerifyState()
	dir := t.TempDir()
	body := "<!-- moon:verify\n/bin/echo ok\ngo version\n-->\n"
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, err := VerifyCommands(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 || cmds[1][0] != "go" {
		t.Fatalf("cmds = %#v", cmds)
	}
	first := MaybeVerify(t.Context(), dir, "turn-1", "edit", false)
	if !strings.Contains(first, "跳过 /bin/echo") || !strings.Contains(first, "通过 go version") {
		t.Fatalf("first verify:\n%s", first)
	}
	if second := MaybeVerify(t.Context(), dir, "turn-1", "write", false); second != "" {
		t.Fatalf("same turn should not verify again: %s", second)
	}
	if third := MaybeVerify(t.Context(), dir, "turn-2", "edit", false); !strings.Contains(third, "通过") {
		t.Fatalf("new turn should verify: %s", third)
	}
}

func TestTotalRulesBudget(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "pkg")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fileName), []byte(strings.Repeat("a", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, fileName), []byte(strings.Repeat("b", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(child, "", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Documents) != 2 {
		t.Fatalf("budget result: %+v", got)
	}
	if len(got.Documents[1].Content) > 2 {
		t.Fatalf("near file should take the remaining budget, got %q", got.Documents[1].Content)
	}
}
