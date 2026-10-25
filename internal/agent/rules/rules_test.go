package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromOrdersGlobalToNearest(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(root, "global.md")
	if err := os.WriteFile(global, []byte("global rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fileName), []byte("root rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", fileName), []byte("src rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(child, global, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(got.Documents))
	}
	if !strings.Contains(got.Content, "global rule") || !strings.Contains(got.Content, "src rule") {
		t.Fatalf("prompt missing rules: %q", got.Content)
	}
	if strings.Index(got.Content, "root rule") > strings.Index(got.Content, "src rule") {
		t.Fatalf("nearer rule should come later: %q", got.Content)
	}
}

func TestLoadFromCapsInvalidUTF8WithoutBreakingUTF8(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, fileName)
	data := append([]byte("规则\n"), make([]byte, 20)...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(root, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Documents) != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if !strings.Contains(got.Content, "已截断") {
		t.Fatalf("missing truncation marker: %q", got.Content)
	}
}

func TestLoadFromEmptyWorkspaceIsNoop(t *testing.T) {
	got, err := LoadFrom("", "", 1)
	if err != nil || got.Content != "" || len(got.Documents) != 0 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}
