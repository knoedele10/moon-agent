package trigger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotAndChanged(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Snapshot(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Path != "a.txt" {
		t.Fatalf("snapshot = %#v", first)
	}
	if Changed(nil, first) != nil {
		t.Fatal("没有基线时不应把全部文件当成变化")
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Snapshot(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	changed := Changed(first, second)
	if len(changed) != 1 || changed[0] != "b.txt" {
		t.Fatalf("changed = %#v", changed)
	}
}
