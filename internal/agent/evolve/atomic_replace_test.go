package evolve

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestAtomicReplaceRetriesWithoutRemovingOriginal(t *testing.T) {
	oldContent, newContent := "original-data", "replacement-data"
	attempts := 0
	var waits []time.Duration
	rename := func(source, target string) error {
		attempts++
		if source != "temporary" || target != "memory.json" {
			t.Fatal("重试改变了源或目标")
		}
		if oldContent != "original-data" {
			t.Fatal("替换成功前原目标数据已改变")
		}
		if attempts <= 2 {
			return fs.ErrPermission
		}
		oldContent, newContent = newContent, ""
		return nil
	}
	err := replaceFileWithRetry("temporary", "memory.json", rename, func(d time.Duration) { waits = append(waits, d) }, func(err error) bool { return errors.Is(err, fs.ErrPermission) })
	if err != nil || attempts != 3 || len(waits) != 2 || oldContent != "replacement-data" || newContent != "" {
		t.Fatalf("临时占用恢复后替换失败：attempts=%d waits=%v err=%v", attempts, waits, err)
	}
}

func TestAtomicReplaceStopsAndPreservesFinalError(t *testing.T) {
	t.Run("永久错误不重试", func(t *testing.T) {
		attempts, waits := 0, 0
		want := &os.LinkError{Op: "rename", Old: "temporary", New: "target", Err: fs.ErrNotExist}
		err := replaceFileWithRetry("temporary", "target", func(string, string) error { attempts++; return want }, func(time.Duration) { waits++ }, func(err error) bool { return errors.Is(err, fs.ErrPermission) })
		if err != want || attempts != 1 || waits != 0 {
			t.Fatalf("永久错误仍重试：%d %d %v", attempts, waits, err)
		}
	})
	t.Run("临时错误耗尽后照实失败", func(t *testing.T) {
		attempts := 0
		var total time.Duration
		want := &os.LinkError{Op: "rename", Old: "temporary", New: "target", Err: fs.ErrPermission}
		err := replaceFileWithRetry("temporary", "target", func(string, string) error { attempts++; return want }, func(d time.Duration) { total += d }, func(err error) bool { return errors.Is(err, fs.ErrPermission) })
		if err != want || attempts != 5 || total != 150*time.Millisecond {
			t.Fatalf("重试未按上限停止：attempts=%d wait=%s err=%v", attempts, total, err)
		}
	})
}

func TestAtomicReplaceOnlyRetriesRecognizedWindowsErrors(t *testing.T) {
	for _, item := range []struct {
		err        error
		recognized bool
	}{
		{fs.ErrPermission, true},
		{&os.LinkError{Op: "rename", Err: syscall.Errno(32)}, true},
		{&os.LinkError{Op: "rename", Err: syscall.Errno(33)}, true},
		{fs.ErrNotExist, false},
		{errors.New("Access is denied"), false},
		{nil, false},
	} {
		want := runtime.GOOS == "windows" && item.recognized
		if got := transientReplaceError(item.err); got != want {
			t.Fatalf("重试分类错误：err=%v got=%v want=%v", item.err, got, want)
		}
	}
}
