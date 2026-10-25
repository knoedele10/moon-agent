package fs

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// 检查点默认写用户缓存；测试必须隔离到临时目录，避免污染开发机。
func isolateCheckpoints(t *testing.T) {
	t.Helper()
	t.Setenv(checkpointEnvDir, t.TempDir())
}

func TestCheckpointRestoresEdit(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	mustWriteFile(t, path, "v1\n")
	ex := execFor(t, root)

	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "v1", "new_string": "v2"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "v2\n" {
		t.Fatalf("编辑未生效：%q", got)
	}

	res, err := call(RestoreTool{}, ex, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "已恢复") || !strings.Contains(res.Content, "v1") {
		t.Fatalf("恢复回执应包含检查点与内容预览：%q", res.Content)
	}
	if got := readFile(t, path); got != "v1\n" {
		t.Fatalf("恢复后内容应为 v1：%q", got)
	}
	if res.Display["checkpoint"] == "" {
		t.Fatalf("回执应带 checkpoint id：%+v", res.Display)
	}
}

func TestCheckpointRestoresDeletedFile(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	path := filepath.Join(root, "gone.txt")
	mustWriteFile(t, path, "keep me\n")
	ex := execFor(t, root)

	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "gone.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(DeleteTool{}, ex, map[string]any{"file_path": "gone.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := call(RestoreTool{}, ex, map[string]any{"file_path": "gone.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(readFile(t, path)) != "keep me" {
		t.Fatalf("删除后应能按原路径恢复：%q", res.Content)
	}
}

func TestCheckpointMissingIsRepairable(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "new.txt"), "x")
	ex := execFor(t, root)

	_, err := call(RestoreTool{}, ex, map[string]any{"file_path": "new.txt"})
	mustCode(t, err, CodeNotFound)
	if !strings.Contains(err.Error(), "checkpoint") || !strings.Contains(err.Error(), "write/edit/move/delete") {
		t.Fatalf("无检查点时应说明哪些操作会留检查点：%v", err)
	}
}

func TestCheckpointKeepsLatestTen(t *testing.T) {
	isolateCheckpoints(t)
	target := filepath.Join(t.TempDir(), "a.txt")
	for i := 0; i < 12; i++ {
		if err := checkpoints.save("session", "turn", target, "a.txt", []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	metas := checkpoints.list("session", target)
	if len(metas) != checkpointKeepPerPath {
		t.Fatalf("每个目标应保留最近 %d 份，实际 %d", checkpointKeepPerPath, len(metas))
	}
	_, data, ok := checkpoints.latest("session", target)
	if !ok || string(data) != "v11" {
		t.Fatalf("最新检查点应是 v11，实际 %q ok=%v", string(data), ok)
	}
}

func TestCheckpointSkipsOversizedFile(t *testing.T) {
	isolateCheckpoints(t)
	target := filepath.Join(t.TempDir(), "big.log")
	big := bytes.Repeat([]byte("x"), checkpointMaxBytes+1)
	if err := checkpoints.save("session", "turn", target, "big.log", big); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := checkpoints.latest("session", target); ok {
		t.Fatal("超过上限的文件不应留检查点")
	}
}

func TestRecoveryToolsShape(t *testing.T) {
	tools := RecoveryTools()
	if len(tools) != 2 || tools[0].Name() != "restore" || tools[1].Name() != "restore_turn" {
		t.Fatalf("恢复工具清单不对：%v", tools)
	}
	props, _ := tools[0].Parameters()["properties"].(map[string]any)
	if props["file_path"] == nil {
		t.Fatal("restore 缺少 file_path 参数")
	}
}

// 整轮回滚：这一轮改过的多个文件都恢复到改动前。
func TestRestoreTurnRollsBackAllFiles(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	mustWriteFile(t, a, "a1\n")
	mustWriteFile(t, b, "b1\n")
	ex := execFor(t, root)
	ex.TurnID = "turn-1"

	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := call(ReadTool{}, ex, map[string]any{"file_path": name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "a1", "new_string": "a2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "b.txt", "old_string": "b1", "new_string": "b2"}); err != nil {
		t.Fatal(err)
	}

	res, err := call(RestoreTurnTool{}, ex, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "2 个文件") {
		t.Fatalf("应报告回滚两个文件：%q", res.Content)
	}
	if got := readFile(t, a); got != "a1\n" {
		t.Fatalf("a.txt 未回滚：%q", got)
	}
	if got := readFile(t, b); got != "b1\n" {
		t.Fatalf("b.txt 未回滚：%q", got)
	}
}

// 同一文件一轮内改两次：整轮回滚要恢复本轮开始前的内容，而不是上一次编辑前的中间态。
func TestRestoreTurnUsesOldestCheckpointInTurn(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	mustWriteFile(t, path, "v0\n")
	ex := execFor(t, root)
	ex.TurnID = "turn-1"
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "v0", "new_string": "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "v1", "new_string": "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(RestoreTurnTool{}, ex, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "v0\n" {
		t.Fatalf("整轮回滚应回到 v0，实际 %q", got)
	}
}

// 只回滚最近一轮，不能把更早轮次的改动一起带走。
func TestRestoreTurnOnlyLatestTurn(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	mustWriteFile(t, a, "a0\n")
	mustWriteFile(t, b, "b0\n")
	ex := execFor(t, root)

	ex.TurnID = "turn-1"
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "a0", "new_string": "a1"}); err != nil {
		t.Fatal(err)
	}

	ex.TurnID = "turn-2"
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "b.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "b.txt", "old_string": "b0", "new_string": "b1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(RestoreTurnTool{}, ex, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, a); got != "a1\n" {
		t.Fatalf("更早轮次的 a.txt 不应被回滚：%q", got)
	}
	if got := readFile(t, b); got != "b0\n" {
		t.Fatalf("最近一轮的 b.txt 应被回滚：%q", got)
	}
}

// 只新建文件时没有「改动前」版本，不能整轮回滚，也不能谎称成功。
func TestRestoreTurnWithoutCheckpoints(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	ex := execFor(t, root)
	ex.TurnID = "turn-new"
	if _, err := call(WriteTool{}, ex, map[string]any{"file_path": "new.txt", "content": "hello"}); err != nil {
		t.Fatal(err)
	}
	res, err := call(RestoreTurnTool{}, ex, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "没有可回滚") {
		t.Fatalf("没有检查点时应明确失败：%+v", res)
	}
}

// 检查点目标在工作区外且未开启 AllowOutside 时，整轮回滚必须跳过它。
func TestRestoreTurnSkipsOutsideWorkspace(t *testing.T) {
	isolateCheckpoints(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWriteFile(t, outside, "secret\n")
	if err := checkpoints.save("s", "turn-out", outside, "secret.txt", []byte("old-secret\n")); err != nil {
		t.Fatal(err)
	}
	ex := &agent.Exec{SessionID: "s", TurnID: "turn-out", Root: root}
	res, err := call(RestoreTurnTool{}, ex, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "不在活动工作区内") {
		t.Fatalf("工作区外目标应被跳过并说明原因：%+v", res)
	}
	if got := readFile(t, outside); got != "secret\n" {
		t.Fatalf("工作区外文件不应被改动：%q", got)
	}
}
