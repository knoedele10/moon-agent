package fs

// 独立验收（task-43）：观察策略三态 / 版本守卫 / 原子写 / 并发 / 符号链接能力。
// 与实现方 fs_test.go 的用例刻意不同：这里测 mtime 单独变化、被拒读是否污染观察表、
// 失败写是否留 tmp、以及「观察后被外部抢先创建」的竞态。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func vListTmp(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.Contains(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestVerifySymlinkCapability 独立确认实现方那句「本机无 SeCreateSymbolicLinkPrivilege」。
func TestVerifySymlinkCapability(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.txt")
	vWrite(t, target, "OUTSIDE")
	link := filepath.Join(root, "link.txt")
	err := os.Symlink(target, link)
	if err != nil {
		t.Logf("符号链接**建不了**（确认实现方说法）：%v", err)
		return
	}
	t.Logf("符号链接**建得出来**（实现方说法不成立）：%s -> %s", link, target)
	ex := vExec(t, root)
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "link.txt"}); err == nil {
		t.Fatalf("符号链接逃逸成功：读到了根外内容")
	} else if got := CodeOf(err); got != CodeSandboxDenied {
		t.Fatalf("符号链接逃逸期望 %s，实际 %s（%v）", CodeSandboxDenied, got, err)
	}
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "link.txt", "content": "OVERWRITE"}); err == nil {
		t.Fatalf("符号链接写入没被拒")
	}
	if b, _ := os.ReadFile(target); string(b) != "OUTSIDE" {
		t.Fatalf("根外文件被改了：%q", string(b))
	}
}

// TestVerifyMtimeOnlyChangeIsStale：内容与大小都不变，只动 mtime —— 也必须 STALE。
// （只看 size 的版本令牌会漏掉这种情况。）
func TestVerifyMtimeOnlyChangeIsStale(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	vWrite(t, p, "hello world")
	ex := vExec(t, root)
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatalf("read: %v", err)
	}
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, err := vCall(t, "edit", ex, map[string]any{"file_path": "a.txt", "old_string": "world", "new_string": "WORLD"}); err == nil {
		t.Fatalf("mtime 变了但 edit 成功了（版本令牌只看 size？）")
	} else if got := CodeOf(err); got != CodeStaleVersion {
		t.Fatalf("期望 %s，实际 %s（%v）", CodeStaleVersion, got, err)
	}
	// 陈旧 + 字面量根本不存在：仍然必须是 STALE，绝不能是 NO_MATCH
	if _, err := vCall(t, "edit", ex, map[string]any{"file_path": "a.txt", "old_string": "NOT-THERE", "new_string": "x"}); err == nil {
		t.Fatalf("陈旧编辑居然成功")
	} else if got := CodeOf(err); got != CodeStaleVersion {
		t.Fatalf("版本守卫没有先于字面量匹配：%s（%v）", got, err)
	}
}

// TestVerifyDeniedReadDoesNotRecordAbsent：被**拒绝**的读（不是「不存在」）不得记成「确认不存在」。
// 契约 §4.2 说的是「read 失败（不存在）→ 记 absent」。
func TestVerifyDeniedReadDoesNotRecordAbsent(t *testing.T) {
	root := t.TempDir()
	vMkdir(t, filepath.Join(root, "adir"))
	ex := vExec(t, root)
	_, err := vCall(t, "read", ex, map[string]any{"file_path": "adir"})
	if err == nil {
		t.Fatalf("读目录居然成功")
	}
	// 再 edit 同一个目标：不允许出现「确认不存在」导致的 FS_NOT_FOUND
	_, err2 := vCall(t, "edit", ex, map[string]any{"file_path": "adir", "old_string": "a", "new_string": "b"})
	code := CodeOf(err2)
	t.Logf("read(目录)=%s ; edit(目录)=%s（%v）", CodeOf(err), code, err2)
	if code == CodeNotFound {
		t.Fatalf("被拒的读被记成了「确认不存在」：edit 报 %s", code)
	}
}

// TestVerifyThreeStateFlow：read 缺失→absent；write 创建；外部删除后 read 再→absent；edit→FS_NOT_FOUND。
func TestVerifyThreeStateFlow(t *testing.T) {
	root := t.TempDir()
	ex := vExec(t, root)
	if _, err := vCall(t, "edit", ex, map[string]any{"file_path": "n.txt", "old_string": "a", "new_string": "b"}); CodeOf(err) != CodeNotObserved {
		t.Fatalf("没见过就 edit 应报 %s，实际 %s", CodeNotObserved, CodeOf(err))
	}
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "n.txt"}); CodeOf(err) != CodeNotFound {
		t.Fatalf("读缺失文件应报 %s，实际 %s", CodeNotFound, CodeOf(err))
	}
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "n.txt", "content": "made"}); err != nil {
		t.Fatalf("读缺失后 write 应该能创建：%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "n.txt")); string(b) != "made" {
		t.Fatalf("内容不对：%q", string(b))
	}
	if err := os.Remove(filepath.Join(root, "n.txt")); err != nil {
		t.Fatalf("删: %v", err)
	}
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "n.txt"}); CodeOf(err) != CodeNotFound {
		t.Fatalf("删掉后 read 应报 %s，实际 %s", CodeNotFound, CodeOf(err))
	}
	if _, err := vCall(t, "edit", ex, map[string]any{"file_path": "n.txt", "old_string": "a", "new_string": "b"}); CodeOf(err) != CodeNotFound {
		t.Fatalf("确认不存在后 edit 应报 %s，实际 %s", CodeNotFound, CodeOf(err))
	}
}

// TestVerifyCreateIfAbsentLosesRaceToExternalCreator：观察时不存在，写之前被外部创建 → 必须 NOT_OBSERVED，
// 且**外部内容不许被覆盖**。
func TestVerifyCreateIfAbsentLosesRaceToExternalCreator(t *testing.T) {
	root := t.TempDir()
	ex := vExec(t, root)
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "race.txt"}); CodeOf(err) != CodeNotFound {
		t.Fatalf("应确认不存在")
	}
	vWrite(t, filepath.Join(root, "race.txt"), "EXTERNAL")
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "race.txt", "content": "MINE"}); CodeOf(err) != CodeNotObserved {
		t.Fatalf("外部抢先创建应报 %s，实际 %s（%v）", CodeNotObserved, CodeOf(err), err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "race.txt")); string(b) != "EXTERNAL" {
		t.Fatalf("外部文件被覆盖了：%q", string(b))
	}
}

// TestVerifyNoTmpLeftovers：成功写与失败写之后都不许留 .tmp。
func TestVerifyNoTmpLeftovers(t *testing.T) {
	root := t.TempDir()
	ex := vExec(t, root)
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "ok.txt", "content": "v1"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if left := vListTmp(t, root); len(left) != 0 {
		t.Fatalf("成功写之后残留 tmp：%v", left)
	}
	// 失败写：目标是个已存在的目录 → 穿不过去，必须失败且不留 tmp
	vMkdir(t, filepath.Join(root, "adir"))
	_, _ = vCall(t, "write", ex, map[string]any{"file_path": "adir", "content": "x"})
	if left := vListTmp(t, root); len(left) != 0 {
		t.Fatalf("失败写之后残留 tmp：%v", left)
	}
}

// TestVerifyConcurrentEditsExactlyOneWins：我自己的并发用例（2 个 goroutine，反复 5 轮）。
func TestVerifyConcurrentEditsExactlyOneWins(t *testing.T) {
	for round := 0; round < 5; round++ {
		root := t.TempDir()
		p := filepath.Join(root, "c.txt")
		vWrite(t, p, "X")
		ex := vExec(t, root)
		if _, err := vCall(t, "read", ex, map[string]any{"file_path": "c.txt"}); err != nil {
			t.Fatalf("read: %v", err)
		}
		codes := make([]string, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := vCall(t, "edit", ex, map[string]any{"file_path": "c.txt", "old_string": "X", "new_string": "Y"})
				if err != nil {
					codes[i] = CodeOf(err)
				} else {
					codes[i] = "OK"
				}
			}(i)
		}
		close(start)
		wg.Wait()
		ok, stale, other := 0, 0, 0
		for _, c := range codes {
			switch c {
			case "OK":
				ok++
			case CodeStaleVersion:
				stale++
			default:
				other++
				t.Logf("第 %d 轮出现非预期码：%s", round, c)
			}
		}
		if ok != 1 || stale != 1 || other != 0 {
			t.Fatalf("第 %d 轮：期望 1 成功 + 1 STALE，实际 codes=%v", round, codes)
		}
		if b, _ := os.ReadFile(p); string(b) != "Y" {
			t.Fatalf("第 %d 轮最终内容应为 Y，实际 %q", round, string(b))
		}
	}
}
