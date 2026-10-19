package fs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 本文件覆盖「工作区是首选目录，而不是硬沙箱」这条改造（Exec.AllowOutside）：
//   - 默认（false）行为必须与改造前完全一致：工作区外一律拒绝；
//   - true 时放行工作区外的**绝对路径**，并做 Clean 归一化；
//   - 工作区外的相对路径（../ 穿越）仍然拒绝；
//   - 系统目录（盘符根、Windows 目录、Program Files 系）永远拒绝，跟档位无关。

func TestOutsideDisabledStillDenied(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	secret := filepath.Join(out, "secret.txt")
	mustWriteFile(t, secret, "TOP SECRET")
	ex := execFor(t, root) // AllowOutside 默认 false

	_, err := call(ReadTool{}, ex, map[string]any{"file_path": secret})
	mustCode(t, err, CodeSandboxDenied)
	_, err = call(WriteTool{}, ex, map[string]any{"file_path": filepath.Join(out, "pwn.txt"), "content": "x"})
	mustCode(t, err, CodeSandboxDenied)
	if _, statErr := os.Stat(filepath.Join(out, "pwn.txt")); statErr == nil {
		t.Fatal("AllowOutside=false 时根外文件不该被创建")
	}
	if got := readFile(t, secret); got != "TOP SECRET" {
		t.Fatalf("根外文件被改写了: %q", got)
	}
}

func TestOutsideAllowedForAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	mustWriteFile(t, filepath.Join(out, "data", "a.txt"), "hello outside")

	ex := execFor(t, root)
	ex.AllowOutside = true

	// 读：带冗余段（sub/..）的绝对路径必须 Clean 归一化后读到。
	// 手工拼接而不是 filepath.Join——Join 会先 Clean，测不出归一化这一步。
	sep := string(filepath.Separator)
	messy := out + sep + "data" + sep + "sub" + sep + ".." + sep + "a.txt"
	res, err := call(ReadTool{}, ex, map[string]any{"file_path": messy})
	if err != nil {
		t.Fatalf("AllowOutside=true 时外部绝对路径应当可读: %v", err)
	}
	if !strings.Contains(res.Content, "hello outside") {
		t.Fatalf("读到的内容不对: %q", res.Content)
	}

	// 写：外部绝对路径可创建（父目录已存在），展示路径是归一化后的绝对路径。
	target := filepath.Join(out, "new.txt")
	res, err = call(WriteTool{}, ex, map[string]any{"file_path": target, "content": "written"})
	if err != nil {
		t.Fatalf("AllowOutside=true 时外部绝对路径应当可写: %v", err)
	}
	if got := readFile(t, target); got != "written" {
		t.Fatalf("外部文件内容不对: %q", got)
	}
	if got, _ := res.Display["file_path"].(string); got != filepath.ToSlash(target) {
		t.Fatalf("展示路径应为归一化后的绝对路径 %q，got %q", filepath.ToSlash(target), got)
	}

	// 正向对照：工作区内的相对路径仍然照旧可读写。
	mustWriteFile(t, filepath.Join(root, "in.txt"), "inside")
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "in.txt"}); err != nil {
		t.Fatalf("工作区内相对路径应当照旧可读: %v", err)
	}
}

func TestOutsideRelativeEscapeStillDenied(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	ex.AllowOutside = true

	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "../pwn.txt", "content": "x"})
	mustCode(t, err, CodeSandboxDenied)
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "pwn.txt")); statErr == nil {
		t.Fatal("AllowOutside=true 也不该放行工作区外的相对路径")
	}
}

func TestOutsideSystemDirsAlwaysDenied(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	ex.AllowOutside = true

	// 盘符根 / 文件系统根：永远拒绝。
	driveRoot := filepath.VolumeName(root) + string(filepath.Separator)
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": driveRoot})
	mustCode(t, err, CodeSandboxDenied)

	if runtime.GOOS != "windows" {
		return
	}
	// Windows 系统目录与 Program Files 系：读/写都拒绝。
	for _, p := range []string{
		filepath.Join(`C:\Windows`, "System32", "drivers", "etc", "hosts"),
		filepath.Join(`C:\Program Files`, "pwn.txt"),
		filepath.Join(`C:\Program Files (x86)`, "pwn.txt"),
	} {
		if _, err := call(ReadTool{}, ex, map[string]any{"file_path": p}); err == nil {
			t.Fatalf("系统目录 %q 不该被放行", p)
		} else if got := CodeOf(err); got != CodeSandboxDenied {
			t.Fatalf("系统目录 %q 期望 %s，实际 %s（%v）", p, CodeSandboxDenied, got, err)
		}
	}
	if _, err := call(WriteTool{}, ex, map[string]any{"file_path": filepath.Join(`C:\Windows`, "pwn.txt"), "content": "x"}); err == nil {
		t.Fatal("系统目录不该被写入")
	}
}
