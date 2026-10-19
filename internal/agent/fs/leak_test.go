package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// F4：IO 错误文本不得泄漏根目录的绝对路径（契约 §4.5：面向模型的文本只给相对路径）。
//
// 触发点都是「失败关闭」的场景：写不存在的目录、冒号/ADS 形态的文件名。
// 关键是错误文案里不能出现 C:\...\root\... 这类绝对路径。
func TestIOErrorTextHasNoAbsolutePath(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)

	// 造一个「父路径被普通文件占用」的场景：父目录不存在会自动创建，
	// 但 MkdirAll 撞上同名文件仍会失败，正好覆盖新增的补目录分支。
	mustWriteFile(t, filepath.Join(root, "blocker"), "x")
	cases := []struct {
		name string
		tool agent.Tool
		args map[string]any
	}{
		{"write 到被文件占用的目录", WriteTool{}, map[string]any{"file_path": "blocker/a.txt", "content": "x"}},
		{"write 冒号/ADS 形态", WriteTool{}, map[string]any{"file_path": "a.txt:stream", "content": "x"}},
		{"read 冒号/ADS 形态", ReadTool{}, map[string]any{"file_path": "a.txt:stream"}},
		{"read_image 冒号/ADS 形态", ReadImageTool{}, map[string]any{"file_path": "p.png:stream"}},
		{"read 到不存在的目录", ReadTool{}, map[string]any{"file_path": "nodir/b.txt"}},
	}

	for _, c := range cases {
		_, err := call(c.tool, ex, c.args)
		if err == nil {
			t.Logf("%s: 本次未报错，跳过", c.name)
			continue
		}
		msg := err.Error()
		t.Logf("%s -> %s", c.name, msg)
		if strings.Contains(msg, root) {
			t.Errorf("%s 的错误文本泄漏了根目录绝对路径: %s", c.name, msg)
		}
		if absPathRe.MatchString(msg) {
			t.Errorf("%s 的错误文本仍含绝对路径: %s", c.name, msg)
		}
	}
}

// 结构化兜底：错误对象里的 Path 字段本身也只能是相对路径。
func TestErrorStructPathIsRelative(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	// 父目录不存在现在会自动创建，不再是错误；改用「目标是已存在的目录」造错，
	// 仍然覆盖「错误对象的 Path 必须是相对路径」这条红线。
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "d", "content": "x"})
	if err == nil {
		t.Fatal("应当报错")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("应为 *fs.Error: %v", err)
	}
	if filepath.IsAbs(e.Path) || strings.Contains(e.Path, root) {
		t.Fatalf("Error.Path 必须是相对路径: %q", e.Path)
	}
}

// ioCause 的三条分支都要干净：PathError / LinkError 取 .Err（只有原因），
// 其它错误走兜底替换。这里用合成错误把三条分支都覆盖到 ——
// os.Rename 失败（LinkError，Old/New 都是绝对路径）在真实文件系统上很难稳定构造。
func TestIoCauseStripsAbsolutePaths(t *testing.T) {
	root := `C:\root\sub`
	target := root + `\a.txt`
	display := "a.txt"
	cases := []struct {
		name string
		err  error
	}{
		{"PathError（os.Stat/os.ReadFile/os.CreateTemp）", &os.PathError{Op: "open", Path: target, Err: errors.New("boom")}},
		{"LinkError（os.Link/os.Rename，Old/New 都带绝对路径）", &os.LinkError{Op: "rename", Old: root + `\.a.txt.1.tmp`, New: target, Err: errors.New("boom")}},
		{"普通错误（兜底替换）", errors.New("plain " + target + " tail")},
	}
	for _, c := range cases {
		msg := ioCause(c.err, target, display)
		if msg == "" {
			t.Errorf("%s: 原因不该为空", c.name)
		}
		if strings.Contains(msg, root) || strings.Contains(msg, `C:\`) {
			t.Errorf("%s: 仍含绝对路径: %s", c.name, msg)
		}
		if absPathRe.MatchString(msg) {
			t.Errorf("%s: 正则仍匹配到绝对路径: %s", c.name, msg)
		}
	}
	// 兜底分支要把 target 换成展示路径，而不是抹成 [path]
	if got := ioCause(cases[2].err, target, display); got != "plain a.txt tail" {
		t.Errorf("兜底替换结果不对: %q", got)
	}
}

func TestIoCauseExplainsOperatingSystemPermissionDenial(t *testing.T) {
	err := &os.PathError{Op: "open", Path: `C:\readonly\notes.txt`, Err: errors.New("Access is denied.")}
	got := ioCause(err, `C:\readonly\notes.txt`, "notes.txt")
	if !strings.Contains(got, "换一个可写工作区") || !strings.Contains(got, "系统文件权限") {
		t.Fatalf("权限错误应与 agent 档位拒绝区分，并给出可操作建议，got %q", got)
	}
	if strings.Contains(got, `C:\readonly`) {
		t.Fatalf("权限错误不得泄漏绝对路径，got %q", got)
	}
}

// 结构回归：面向模型的错误文本一律走 ioCause，源码里不允许再出现 `Reason: err.Error()`。
// 这条防的是「以后有人新加一处直接拼底层错误」——那种写法就会把绝对路径带给模型。
func TestNoRawErrorTextInModelFacingMessages(t *testing.T) {
	files := []string{"atomic.go", "tools.go", "sandbox.go", "observation.go", "errors.go"}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		if strings.Contains(string(b), "Reason: err.Error()") {
			t.Errorf("%s 里仍有 `Reason: err.Error()`：底层错误会带绝对路径，请改用 ioCause(err, target, display)", f)
		}
	}
}
