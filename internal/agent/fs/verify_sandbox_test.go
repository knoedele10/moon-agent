package fs

// 独立验收（task-43，verifier 自写）：路径沙箱红线的**对抗性**用例。
// 与实现方的 sandbox_test.go 刻意不重叠：这里全部围绕 Windows reparse point（junction）、
// 链接链、链接指向根内（必须放行）、前缀兄弟目录、长路径前缀、ADS 冒号等实现方没碰的形态。
// 每个「拒绝」都同时断言**根外真的没有产生副作用**（只看 error 码是不够的）。

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

func vExec(t *testing.T, root string) *agent.Exec {
	t.Helper()
	return &agent.Exec{SessionID: "verify:" + t.Name(), Root: root, Signal: context.Background()}
}

func vTool(t *testing.T, name string) agent.Tool {
	t.Helper()
	for _, x := range Tools() {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("没有名为 %s 的工具", name)
	return nil
}

func vCall(t *testing.T, name string, ex *agent.Exec, args map[string]any) (agent.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return vTool(t, name).Execute(context.Background(), raw, ex)
}

func vWantCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒（%s），实际成功了", want)
	}
	if got := CodeOf(err); got != want {
		t.Fatalf("期望错误码 %s，实际 %s（%v）", want, got, err)
	}
}

func vMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("建目录 %s: %v", p, err)
	}
}

func vWrite(t *testing.T, p, content string) {
	t.Helper()
	vMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", p, err)
	}
}

func vExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// vJunction 用 mklink /J 建目录联接（不需要 SeCreateSymbolicLinkPrivilege）。
func vJunction(t *testing.T, link, target string) error {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "Junction") && !strings.Contains(string(out), "联接") {
		t.Logf("mklink 输出（未识别，但命令成功）：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// TestVerifyJunctionEscapeDenied 是**本任务的核心用例**：根内 junction 指向根外。
// 第一版实现用 filepath.EvalSymlinks 判逃逸时，这个用例会失败（根外被创建文件）。
func TestVerifyJunctionEscapeDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	j := filepath.Join(root, "esc")
	if err := vJunction(t, j, outside); err != nil {
		t.Skipf("本机建不了 junction，跳过：%v", err)
	}
	ex := vExec(t, root)

	// 1) write 穿 junction：必须被拒，且根外**不得**出现文件
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "esc/pwn.txt", "content": "pwned"}); err == nil {
		t.Fatalf("逃逸成功：write esc/pwn.txt 没被拒")
	} else if got := CodeOf(err); got != CodeSandboxDenied {
		t.Fatalf("write 穿 junction 期望 %s，实际 %s（%v）", CodeSandboxDenied, got, err)
	}
	if vExists(filepath.Join(outside, "pwn.txt")) {
		t.Fatalf("红线破了：根外被创建了 %s", filepath.Join(outside, "pwn.txt"))
	}

	// 2) read 穿 junction 读根外文件：必须被拒
	vWrite(t, filepath.Join(outside, "secret.txt"), "TOP-SECRET")
	if _, err := vCall(t, "read", ex, map[string]any{"file_path": "esc/secret.txt"}); err == nil {
		t.Fatalf("逃逸成功：read esc/secret.txt 读到了根外内容")
	} else if got := CodeOf(err); got != CodeSandboxDenied {
		t.Fatalf("read 穿 junction 期望 %s，实际 %s（%v）", CodeSandboxDenied, got, err)
	}

	// 3) 深层：esc/sub/pwn2.txt
	vMkdir(t, filepath.Join(outside, "sub"))
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "esc/sub/pwn2.txt", "content": "x"}); err == nil {
		t.Fatalf("逃逸成功：write esc/sub/pwn2.txt 没被拒")
	}
	if vExists(filepath.Join(outside, "sub", "pwn2.txt")) {
		t.Fatalf("红线破了：根外深层被创建了")
	}

	// 4) edit 穿 junction：必须被拒，且根外文件内容不变
	if _, err := vCall(t, "edit", ex, map[string]any{"file_path": "esc/secret.txt", "old_string": "TOP", "new_string": "LEAK"}); err == nil {
		t.Fatalf("逃逸成功：edit 改了根外文件")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "TOP-SECRET" {
		t.Fatalf("根外文件被改动：%q", string(b))
	}

	// 5) read_image 穿 junction 也要拒
	png := filepath.Join(outside, "pic.png")
	vWrite(t, png, "\x89PNG\r\n\x1a\n")
	if _, err := vCall(t, "read_image", ex, map[string]any{"file_path": "esc/pic.png"}); err == nil {
		t.Fatalf("逃逸成功：read_image 读到了根外图片")
	} else if got := CodeOf(err); got != CodeSandboxDenied {
		t.Fatalf("read_image 穿 junction 期望 %s，实际 %s", CodeSandboxDenied, got)
	}
}

// TestVerifyJunctionInsideRootAllowed：链接**指向根内**必须放行（否则是过度拦截，会让正常功能不可用）。
func TestVerifyJunctionInsideRootAllowed(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	vMkdir(t, real)
	vWrite(t, filepath.Join(real, "a.txt"), "hello-inside")
	link := filepath.Join(root, "inner")
	if err := vJunction(t, link, real); err != nil {
		t.Skipf("本机建不了 junction，跳过：%v", err)
	}
	ex := vExec(t, root)

	res, err := vCall(t, "read", ex, map[string]any{"file_path": "inner/a.txt"})
	if err != nil {
		t.Fatalf("指向根内的链接被误拒：%v", err)
	}
	if !strings.Contains(res.Content, "hello-inside") {
		t.Fatalf("读到的内容不对：%q", res.Content)
	}
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "inner/b.txt", "content": "via-link"}); err != nil {
		t.Fatalf("经根内链接写文件被误拒：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(real, "b.txt")); err != nil || string(b) != "via-link" {
		t.Fatalf("写没有落到真实目录：%v %q", err, string(b))
	}
}

// TestVerifyJunctionChainEscapeDenied：链接链（根内 j1 → 根内 j2 → 根外）。
func TestVerifyJunctionChainEscapeDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := vJunction(t, filepath.Join(root, "j2"), outside); err != nil {
		t.Skipf("本机建不了 junction，跳过：%v", err)
	}
	if err := vJunction(t, filepath.Join(root, "j1"), filepath.Join(root, "j2")); err != nil {
		t.Skipf("本机建不了第二层 junction，跳过：%v", err)
	}
	ex := vExec(t, root)
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": "j1/chained.txt", "content": "x"}); err == nil {
		t.Fatalf("链接链逃逸成功")
	}
	if vExists(filepath.Join(outside, "chained.txt")) {
		t.Fatalf("红线破了：链接链在根外创建了文件")
	}
}

// TestVerifyTraversalSpellings：各种 '..' 写法 + 前缀兄弟目录。
func TestVerifyTraversalSpellings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "AAA")
	vMkdir(t, root)
	sibling := filepath.Join(base, "AAA-evil") // 与 root 同前缀的兄弟目录
	vMkdir(t, sibling)
	ex := vExec(t, root)

	for _, p := range []string{
		"../out.txt",
		"..\\out.txt",
		"a/../../out.txt",
		"./../out.txt",
		"..",
		"../AAA-evil/x.txt",
	} {
		if _, err := vCall(t, "write", ex, map[string]any{"file_path": p, "content": "x"}); err == nil {
			t.Fatalf("穿越写法 %q 没被拒", p)
		} else if got := CodeOf(err); got != CodeSandboxDenied {
			t.Fatalf("穿越写法 %q 期望 %s，实际 %s（%v）", p, CodeSandboxDenied, got, err)
		}
	}
	if vExists(filepath.Join(base, "out.txt")) {
		t.Fatalf("红线破了：父目录被创建了 out.txt")
	}
	// 用绝对路径指名兄弟目录：必须被拒（前缀判断不能只看字符串前缀）
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": filepath.Join(sibling, "x.txt"), "content": "x"}); err == nil {
		t.Fatalf("绝对路径指向前缀兄弟目录没被拒")
	}
	if vExists(filepath.Join(sibling, "x.txt")) {
		t.Fatalf("红线破了：前缀兄弟目录被写入")
	}
}

// TestVerifyAbsoluteAndLongPathPrefix：根外绝对路径与 \\?\ 长路径前缀形式。
func TestVerifyAbsoluteAndLongPathPrefix(t *testing.T) {
	root := t.TempDir()
	ex := vExec(t, root)
	outside := t.TempDir()
	for _, p := range []string{
		filepath.Join(outside, "abs.txt"),
		"C:\\Windows\\win.ini",
		"\\\\?\\" + filepath.Join(outside, "long.txt"),
	} {
		if _, err := vCall(t, "write", ex, map[string]any{"file_path": p, "content": "x"}); err == nil {
			t.Fatalf("根外绝对路径 %q 没被拒", p)
		} else if got := CodeOf(err); got != CodeSandboxDenied {
			t.Fatalf("根外绝对路径 %q 期望 %s，实际 %s（%v）", p, CodeSandboxDenied, got, err)
		}
	}
	if vExists(filepath.Join(outside, "abs.txt")) || vExists(filepath.Join(outside, "long.txt")) {
		t.Fatalf("红线破了：根外绝对路径被写入")
	}
}

// TestVerifyNonRegularTargetRejected：目录不是普通文件，必须拒。
func TestVerifyNonRegularTargetRejected(t *testing.T) {
	root := t.TempDir()
	vMkdir(t, filepath.Join(root, "adir"))
	vWrite(t, filepath.Join(root, "afile.txt"), "x")
	ex := vExec(t, root)
	for _, name := range []string{"read", "edit", "read_image"} {
		var err error
		if name == "edit" {
			_, err = vCall(t, name, ex, map[string]any{"file_path": "adir", "old_string": "a", "new_string": "b"})
		} else {
			_, err = vCall(t, name, ex, map[string]any{"file_path": "adir"})
		}
		if err == nil {
			t.Fatalf("%s 作用于目录居然成功了", name)
		}
		t.Logf("%s 作用于目录 -> %s（%v）", name, CodeOf(err), err)
	}
}

// TestVerifyColonInPath（ADS 形态）：带冒号的名字既不许逃逸，也不许静默写成别的东西。
func TestVerifyColonInPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ex := vExec(t, root)
	for _, p := range []string{"sub.txt:evil", "esc:../outside.txt"} {
		res, err := vCall(t, "write", ex, map[string]any{"file_path": p, "content": "x"})
		t.Logf("write %q -> err=%v result=%q", p, err, res.Content)
	}
	if vExists(filepath.Join(outside, "outside.txt")) || vExists(filepath.Join(outside, "evil")) {
		t.Fatalf("红线破了：冒号形态在根外产生了文件")
	}
	// 绝对路径 + ADS：C:\<outside>\x.txt:stream 必须被拒
	abs := filepath.Join(outside, "x.txt") + ":s"
	if _, err := vCall(t, "write", ex, map[string]any{"file_path": abs, "content": "x"}); err == nil {
		t.Fatalf("绝对路径 + ADS 没被拒")
	}
	if vExists(filepath.Join(outside, "x.txt")) {
		t.Fatalf("红线破了：ADS 形态在根外创建了文件")
	}
}
