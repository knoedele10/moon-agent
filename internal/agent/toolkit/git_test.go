package toolkit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

func gitTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v (%s)", err, out)
		}
	}
	return root
}

func gitExecTool(t *testing.T, tool agent.Tool, root string, args map[string]any) (agent.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), raw, &agent.Exec{Root: root})
	return res, err
}

func TestGitStatusAndDiffAreReadOnly(t *testing.T) {
	root := gitTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := gitExecTool(t, GitStatusTool{}, root, map[string]any{})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(status.Content, "hello.txt") {
		t.Fatalf("status missing file: %q", status.Content)
	}
	// Untracked files are intentionally absent from git diff; stage once to
	// establish a baseline, then verify a later modification.
	if _, err := gitExecTool(t, GitAddTool{}, root, map[string]any{"paths": []string{"hello.txt"}}); err != nil { t.Fatal(err) }
	if _, err := gitExecTool(t, GitCommitTool{}, root, map[string]any{"message": "baseline"}); err != nil { t.Fatal(err) }
	diff, err := gitExecTool(t, GitDiffTool{}, root, map[string]any{"path": "hello.txt"})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(diff.Content, "没有检测到改动") {
		t.Fatalf("unexpected initial diff: %q", diff.Content)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err = gitExecTool(t, GitDiffTool{}, root, map[string]any{"path": "hello.txt"})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(diff.Content, "after") {
		t.Fatalf("diff missing changed content: %q", diff.Content)
	}
}

func TestGitAddAndCommitNeverAcceptOutsidePath(t *testing.T) {
	root := gitTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := gitExecTool(t, GitAddTool{}, root, map[string]any{"paths": []string{"hello.txt"}}); err != nil || res.IsError {
		t.Fatalf("git add failed: %+v", res)
	}
	if res, err := gitExecTool(t, GitCommitTool{}, root, map[string]any{"message": "initial"}); err != nil || res.IsError {
		t.Fatalf("git commit failed: %+v", res)
	}
	if res, err := gitExecTool(t, GitAddTool{}, root, map[string]any{"paths": []string{"..\\outside.txt"}}); err == nil && !res.IsError {
		t.Fatal("outside path should be rejected")
	}
	log, err := gitExecTool(t, GitLogTool{}, root, map[string]any{"limit": 1})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(log.Content, "initial") {
		t.Fatalf("log missing commit: %q", log.Content)
	}
}

func TestGitShowRejectsOptionInjection(t *testing.T) {
	root := gitTestRepo(t)
	res, err := gitExecTool(t, GitShowTool{}, root, map[string]any{"ref": "--format=%x"})
	if err == nil && !res.IsError {
		t.Fatal("option-like ref should be rejected")
	}
}
