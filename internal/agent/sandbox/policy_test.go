package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyDeniesShellAndOutsideWrites(t *testing.T) {
	root := t.TempDir()
	audit := filepath.Join(root, "audit.jsonl")
	p := Policy{WriteRoots: []string{root}, AllowExec: []string{"go", "npm"}, AuditPath: audit}
	if err := p.CheckExec("cmd.exe"); err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("cmd.exe should be denied, err=%v", err)
	}
	if err := p.CheckExec("powershell"); err == nil {
		t.Fatal("powershell should be denied")
	}
	if err := p.CheckExec("go"); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckExec("make"); err == nil {
		t.Fatal("make is outside the allow list")
	}
	inside := filepath.Join(root, "sub")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckWrite(inside); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckWrite(t.TempDir()); err == nil {
		t.Fatal("write outside the workspace should fail")
	}
	p.AllowDomains = []string{"example.com"}
	if err := p.CheckDomain("docs.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckDomain("evil.test"); err == nil {
		t.Fatal("evil.test should be denied")
	}
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "denied shell") {
		t.Fatalf("audit missing denial: %s", data)
	}
}
