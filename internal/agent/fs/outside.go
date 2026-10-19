package fs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 本文件是「工作区是首选目录，而不是硬沙箱」的路径策略。
//
// Exec.AllowOutside=false（默认）时行为与改造前**一字不差**：只有工作区内的路径能过
// （见 tools.go 的 resolveFor）。打开 AllowOutside 后额外放行工作区之外的路径，
// 但两条底线不动：
//  1. 外部路径必须显式给**绝对路径**——`..` 穿越、根内链接逃逸仍然拒绝，
//     这样用户确认时看到的也是确切位置；
//  2. 系统目录永远不可操作（盘符根 / 文件系统根、Windows 目录、Program Files 系），
//     跟档位无关。

// OutsideNeedsAbsPath 是「工作区外只接受绝对路径」的统一拒绝文案。
//
// 为什么单独抽成常量：模型很自然地用相对路径（blog/DESIGN.md）去碰工作区外的文件，
// 只报一句 [sandbox: 拒绝访问根目录之外的路径] 它不知道下一步该怎么改；
// web 层的确认壳复用同一句话，保证 fs 层与提示层说法一致。
const OutsideNeedsAbsPath = "工作区外只接受绝对路径，请给出完整路径（例如 E:\\目录\\文件.txt）"

// IsSystemPath 判断一个路径是否落在「永远不可操作」的系统位置。
// 调用方通常传入已经 Clean 过的绝对路径。
func IsSystemPath(abs string) bool {
	p := strings.TrimSpace(abs)
	if p == "" {
		return true
	}
	clean := filepath.Clean(p)
	// 盘符根（C:\）与文件系统根（/）：永远不给。
	if filepath.Dir(clean) == clean {
		return true
	}
	if runtime.GOOS != "windows" {
		return false
	}
	roots := []string{`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`}
	if v := strings.TrimSpace(os.Getenv("SystemRoot")); v != "" {
		roots = append(roots, v)
	}
	if v := strings.TrimSpace(os.Getenv("ProgramFiles")); v != "" {
		roots = append(roots, v)
	}
	if v := strings.TrimSpace(os.Getenv("ProgramFiles(x86)")); v != "" {
		roots = append(roots, v)
	}
	for _, r := range roots {
		if within(r, clean) {
			return true
		}
	}
	return false
}

// resolveOutside 是 AllowOutside=true 时的路径解析。
//
// 先按老规矩做工作区内解析：能过就说明这条路径本来就在工作区里，行为与默认档一致。
// 只有「绝对路径 + 落在工作区外 + 不是系统目录」才额外放行，
// 返回 filepath.Clean 与链接解析之后的真实路径；展示路径用绝对形式（/ 分隔），
// 用户与模型都需要看清到底动了哪个文件。
func resolveOutside(root, p string) (target, display string, err error) {
	if strings.TrimSpace(root) == "" {
		return "", strings.TrimSpace(p), &Error{Code: CodeSandboxDenied, Reason: "未配置可操作的根目录"}
	}
	if t, e := Resolve(root, p); e == nil {
		return t, DisplayPath(root, t), nil
	}
	if strings.TrimSpace(p) == "" {
		return "", strings.TrimSpace(p), &Error{Code: CodeInvalidArgs, Reason: "file_path 不能为空"}
	}
	if !filepath.IsAbs(p) {
		// 工作区外的相对路径（../ 穿越、根内链接逃逸）：不放行。
		// 要碰外面的文件，必须把绝对路径明确写出来。
		return "", strings.TrimSpace(p), &Error{Code: CodeSandboxDenied, Reason: OutsideNeedsAbsPath}
	}
	abs := filepath.Clean(p)
	if IsSystemPath(abs) {
		return "", filepath.ToSlash(abs), &Error{Code: CodeSandboxDenied, Reason: "系统目录永远不可操作: " + filepath.ToSlash(abs)}
	}
	real, rerr := resolveLinks(abs)
	if rerr != nil {
		return "", filepath.ToSlash(abs), &Error{Code: CodeSandboxDenied, Reason: "路径无法解析"}
	}
	if IsSystemPath(real) {
		return "", filepath.ToSlash(real), &Error{Code: CodeSandboxDenied, Reason: "系统目录永远不可操作: " + filepath.ToSlash(real)}
	}
	return real, filepath.ToSlash(real), nil
}
