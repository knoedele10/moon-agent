package fs

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

// ---------- 测试脚手架 ----------

func execFor(t *testing.T, root string) *agent.Exec {
	t.Helper()
	return &agent.Exec{SessionID: t.Name(), Root: root, Signal: context.Background()}
}

// call 执行一次工具调用。**不调用 t.Fatal**：并发用例里会在 goroutine 中使用。
func call(tool agent.Tool, ex *agent.Exec, args map[string]any) (agent.Result, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return agent.Result{}, err
	}
	return tool.Execute(context.Background(), raw, ex)
}

func mustCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际成功", want)
	}
	if got := CodeOf(err); got != want {
		t.Fatalf("期望错误码 %s，实际 %s（%v）", want, got, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	return string(b)
}

// outsideDir 造一个**根目录之外**的目录（与根同级）。
func outsideDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(filepath.Dir(t.TempDir()), "blogmy-outside-")
	if err != nil {
		t.Fatalf("建根外目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// makeDirLink 在 link 处创建一个指向 target 的目录链接。
// 优先 os.Symlink；Windows 普通用户没有 SeCreateSymbolicLinkPrivilege 时退化为 junction
// （junction 同样是 reparse point，filepath.EvalSymlinks 会解析它）。
func makeDirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("环境无法创建符号链接/junction（%v）: %s", err, strings.TrimSpace(string(out)))
	}
}

// ---------- 沙箱红线：三类逃逸全部被拒（每条都配正向对照）----------

func TestSandboxRejectsDotDotTraversal(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	mustWriteFile(t, filepath.Join(out, "secret.txt"), "TOP SECRET")
	ex := execFor(t, root)

	// 负向对照 1：纯 .. 穿越
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "../secret.txt"})
	mustCode(t, err, CodeSandboxDenied)
	// 负向对照 2：先下钻再穿越（词法上不在根内）
	_, err = call(ReadTool{}, ex, map[string]any{"file_path": "a/b/../../../secret.txt"})
	mustCode(t, err, CodeSandboxDenied)
	// 负向对照 3：write 也要拦（不能靠工具各自实现）
	_, err = call(WriteTool{}, ex, map[string]any{"file_path": "../pwn.txt", "content": "x"})
	mustCode(t, err, CodeSandboxDenied)
	// 负向对照 4：read_image 也要拦
	_, err = call(ReadImageTool{}, ex, map[string]any{"file_path": "../secret.txt"})
	mustCode(t, err, CodeSandboxDenied)

	// 正向对照：根内的相对路径正常可读
	mustWriteFile(t, filepath.Join(root, "ok.txt"), "hello")
	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "ok.txt"})
	if err != nil {
		t.Fatalf("根内路径应当可读: %v", err)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Fatalf("内容不对: %q", res.Content)
	}
}

func TestSandboxRejectsAbsolutePathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	secret := filepath.Join(out, "secret.txt")
	mustWriteFile(t, secret, "TOP SECRET")
	ex := execFor(t, root)

	_, err := call(ReadTool{}, ex, map[string]any{"file_path": secret})
	mustCode(t, err, CodeSandboxDenied)
	_, err = call(WriteTool{}, ex, map[string]any{"file_path": secret, "content": "overwrite"})
	mustCode(t, err, CodeSandboxDenied)
	if got := readFile(t, secret); got != "TOP SECRET" {
		t.Fatalf("根外文件被改写了: %q", got)
	}

	// 正向对照：根内的**绝对路径**是允许的（只拒根外）
	inside := filepath.Join(root, "in.txt")
	mustWriteFile(t, inside, "inside")
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": inside}); err != nil {
		t.Fatalf("根内绝对路径应当可读: %v", err)
	}
}

func TestSandboxRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	out := outsideDir(t)
	mustWriteFile(t, filepath.Join(out, "secret.txt"), "TOP SECRET")
	makeDirLink(t, filepath.Join(root, "escape"), out)
	ex := execFor(t, root)

	// 负向对照 1：经由根内链接读到根外文件
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "escape/secret.txt"})
	mustCode(t, err, CodeSandboxDenied)
	// 负向对照 2：经由链接往根外写
	_, err = call(WriteTool{}, ex, map[string]any{"file_path": "escape/pwn.txt", "content": "x"})
	mustCode(t, err, CodeSandboxDenied)
	if _, statErr := os.Stat(filepath.Join(out, "pwn.txt")); statErr == nil {
		t.Fatal("根外文件被创建了：符号链接逃逸没拦住")
	}

	// 正向对照：指向**根内**的链接是允许的（只拦逃逸，不拦符号链接本身）
	mustWriteFile(t, filepath.Join(root, "real", "ok.txt"), "inside-link")
	makeDirLink(t, filepath.Join(root, "inside-link"), filepath.Join(root, "real"))
	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "inside-link/ok.txt"})
	if err != nil {
		t.Fatalf("指向根内的链接应当可读: %v", err)
	}
	if !strings.Contains(res.Content, "inside-link") {
		t.Fatalf("内容不对: %q", res.Content)
	}
}

func TestSandboxRejectsNonRegularFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := execFor(t, root)

	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "adir"})
	mustCode(t, err, CodeSandboxDenied)
	_, err = call(ReadImageTool{}, ex, map[string]any{"file_path": "adir"})
	mustCode(t, err, CodeSandboxDenied)

	// 正向对照：同目录下的普通文件可读
	mustWriteFile(t, filepath.Join(root, "adir", "f.txt"), "ok")
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "adir/f.txt"}); err != nil {
		t.Fatalf("普通文件应当可读: %v", err)
	}
}

func TestSandboxRejectsEmptyRoot(t *testing.T) {
	ex := &agent.Exec{SessionID: t.Name(), Root: "", Signal: context.Background()}
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"})
	mustCode(t, err, CodeSandboxDenied)
}

func TestSandboxRejectsBlankPath(t *testing.T) {
	ex := execFor(t, t.TempDir())
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "   "})
	mustCode(t, err, CodeInvalidArgs)
}

func TestSandboxDeniedMessage(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "../x.txt"})
	if err == nil {
		t.Fatal("应当被拒")
	}
	if !strings.Contains(err.Error(), "[sandbox:") {
		t.Fatalf("文案应形如 [sandbox: ...]，实际 %q", err.Error())
	}
}
