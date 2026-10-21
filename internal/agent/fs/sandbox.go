package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Resolve 把模型给的 file_path 解析成**根目录之内**的绝对路径（契约 §4.1，红线）。
//
// 三步，缺一不可：
//  1. 词法归一（Clean/Join），先拒 `..` 穿越与根外绝对路径；
//  2. **逐段**解析路径、跟随途中的每一个 reparse point（符号链接与 Windows junction），
//     目标不存在时把剩余部分原样拼回 —— 这是 write 创建新文件的正常情况；
//  3. 解析**之后**再判一次前缀：这一步专门拦符号链接/junction 逃逸。
//
// 违反 → FS_SANDBOX_DENIED（文案 [sandbox: 拒绝访问根目录之外的路径]）。
func Resolve(root, p string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", &Error{Code: CodeSandboxDenied, Reason: "未配置可操作的根目录"}
	}
	if strings.TrimSpace(p) == "" {
		return "", &Error{Code: CodeInvalidArgs, Reason: "file_path 不能为空"}
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", &Error{Code: CodeSandboxDenied, Reason: "根目录不可解析"}
	}
	// 根目录本身也可能在链接之下（macOS 的 /tmp → /private/tmp；Windows 上还可能是 junction）：
	// 用同一套逐段解析归一，否则第 3 步的前缀比较会把合法路径误判成逃逸。
	rootReal, err := resolveLinks(rootAbs)
	if err != nil {
		rootReal = rootAbs // 根目录尚不存在时退化为词法根
	}

	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Join(rootReal, p)
	}
	if !within(rootReal, target) {
		return "", &Error{Code: CodeSandboxDenied}
	}

	real, err := resolveLinks(target)
	if err != nil {
		return "", &Error{Code: CodeSandboxDenied, Reason: "路径无法解析"}
	}
	if !within(rootReal, real) {
		return "", &Error{Code: CodeSandboxDenied}
	}
	return real, nil
}

// ResolveWorkspaces 在多个工作区中解析路径。普通相对路径仍落在主目录；
// @workspaceN/xxx 用来明确访问第 N 个已启用目录。绝对路径若位于任一根内也有效。
func ResolveWorkspaces(primary string, roots []string, p string) (string, string, error) {
	all := make([]string, 0, 1+len(roots))
	if strings.TrimSpace(primary) != "" {
		all = append(all, primary)
	}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		duplicate := false
		for _, existing := range all {
			if samePath(existing, root) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			all = append(all, root)
		}
	}
	if len(all) == 0 {
		return "", strings.TrimSpace(p), &Error{Code: CodeSandboxDenied, Reason: "未配置可操作的根目录"}
	}
	p = strings.TrimSpace(p)
	if strings.HasPrefix(strings.ToLower(p), "@workspace") {
		selector, rest, hasSlash := strings.Cut(p, "/")
		if !hasSlash {
			selector, rest, _ = strings.Cut(p, "\\")
		}
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(selector), "@workspace"))
		if err != nil || n < 1 || n > len(all) {
			return "", p, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("工作区编号无效；当前有 %d 个活动工作区，请使用 @workspace1 到 @workspace%d", len(all), len(all))}
		}
		if rest == "" {
			rest = "."
		}
		target, err := Resolve(all[n-1], rest)
		if err != nil {
			return "", fmt.Sprintf("workspace%d/%s", n, filepath.ToSlash(rest)), err
		}
		return target, fmt.Sprintf("workspace%d/%s", n, DisplayPath(all[n-1], target)), nil
	}
	if filepath.IsAbs(p) {
		for i, root := range all {
			if target, err := Resolve(root, p); err == nil {
				label := DisplayPath(root, target)
				if i > 0 {
					label = fmt.Sprintf("workspace%d/%s", i+1, label)
				}
				return target, label, nil
			}
		}
		return "", p, &Error{Code: CodeSandboxDenied}
	}
	target, err := Resolve(all[0], p)
	if err != nil {
		return "", p, err
	}
	return target, DisplayPath(all[0], target), nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(filepath.Clean(a))
	bb, errB := filepath.Abs(filepath.Clean(b))
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// IsWithinWorkspaces 判断目标是否位于任一启用目录内，用于创建父目录等后续安全检查。
func IsWithinWorkspaces(primary string, roots []string, target string) bool {
	all := append([]string{primary}, roots...)
	for _, root := range all {
		if strings.TrimSpace(root) == "" {
			continue
		}
		rootAbs, err := filepath.Abs(root)
		if err == nil {
			if real, rerr := resolveLinks(rootAbs); rerr == nil {
				rootAbs = real
			}
		}
		if err == nil && within(rootAbs, target) {
			return true
		}
	}
	return false
}

// within 判断 target 是否落在 root 之内（含 root 本身）。
// filepath.Rel 在 Windows 上对根部分大小写不敏感（sameWord=EqualFold），
// 所以 C:\Root 与 c:\root\a.txt 不会被误判成逃逸。
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// maxLinkDepth 限制链接跟随深度，避免 a → b → a 这类环把解析卡死。
const maxLinkDepth = 40

// resolveLinks **逐段**解析路径，跟随途中遇到的每一个 reparse point，
// 最后把不存在的尾部原样拼回。
//
// 为什么不用 filepath.EvalSymlinks：
//
//	Windows 上它**不解析 junction**（目录联接）。实测（Go 1.25 / Win11）：
//	  os.Lstat(junction).Mode() 不带 ModeSymlink；os.Readlink(junction) 能拿到目标；
//	  filepath.EvalSymlinks(junction) 原样返回链接路径；
//	  filepath.EvalSymlinks(junction/文件) 直接报「找不到路径」；
//	  但 os.ReadFile(junction/文件) 能成功穿过 junction。
//	只信 EvalSymlinks 的沙箱会因此被 junction 逃逸（本包的单测抓到了这个洞）。
//
// 逐段解析对「目标还不存在」也天然成立：遇到第一个 Lstat 失败的段，
// 就把该段及其后所有段原样拼回当前已解析的目录。
func resolveLinks(p string) (string, error) {
	return resolveLinksDepth(p, 0)
}

// ntPathPrefixes 是 Windows 上可能出现在路径/链接目标里的 NT 命名空间前缀。
// os.Readlink 对 junction 返回的是 `\\?\Global\C:\...` 或 `\??\C:\...` 这类形态。
var ntPathPrefixes = []string{`\\?\Global\`, `\\?\UNC\`, `\\?\`, `\??\`}

// normalizeNTPath 剥掉 NT 前缀，把路径还原成 filepath 能正确识别卷名的形态。
//
// 不做的后果（本包单测抓到过）：`\\?\Global\C:\...` 交给 filepath.VolumeName
// 只会得到 `\\?\Global`，逐段重建时盘符 `C:` 变成普通目录名，分隔符丢失，
// within() 因此恒为 false —— 工作区内的 junction 全被判成越界。
func normalizeNTPath(p string) string {
	upper := strings.ToUpper(p)
	for _, prefix := range ntPathPrefixes {
		// 前缀本身含混合大小写（Global / UNC），必须一并转大写再比较：
		// 否则 `\\?\Global\E:\...` 只命中 `\\?\`，剩下的 `Global\E:` 会被当成相对路径段。
		if !strings.HasPrefix(upper, strings.ToUpper(prefix)) {
			continue
		}
		rest := p[len(prefix):]
		if prefix == `\\?\UNC\` {
			return `\\` + rest
		}
		return rest
	}
	return p
}

func resolveLinksDepth(p string, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", errors.New("符号链接层数过深")
	}
	p = normalizeNTPath(p)
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	rest := strings.TrimPrefix(p[len(vol):], string(filepath.Separator))
	if rest == "" {
		return cur, nil
	}
	parts := strings.Split(rest, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if err != nil {
			// 这一段起就不存在了：把剩余部分拼回已解析的目录即可。
			return filepath.Join(append([]string{cur}, parts[i:]...)...), nil
		}
		// 对任何一段都试 Readlink：真符号链接与 Windows junction 都会返回目标，
		// 普通文件/目录返回错误（非 Windows 上是 EINVAL），据此区分。
		if link, lerr := os.Readlink(next); lerr == nil && link != "" {
			if !filepath.IsAbs(link) {
				link = filepath.Join(cur, link)
			}
			// 顺序很重要：必须先剥 NT 前缀再 Clean。
			// `\\?\Global\E:\dir` 直接交给 filepath.Clean 会得到 `\Global\E:dir`，
			// 把 `\\?\` 前缀与盘符一起吃掉，normalizeNTPath 之后再也认不出来。
			resolved, rerr := resolveLinksDepth(filepath.Clean(normalizeNTPath(link)), depth+1)
			if rerr != nil {
				return "", rerr
			}
			cur = resolved
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// 标记是链接却读不出目标：宁拒绝，不放行。
			return "", errors.New("无法解析符号链接: " + next)
		}
		cur = next
	}
	return cur, nil
}

// DisplayPath 给模型看的路径：根目录相对形式，统一用 /，绝不泄露绝对路径。
func DisplayPath(root, target string) string {
	if rootAbs, err := filepath.Abs(root); err == nil {
		if rel, err := filepath.Rel(rootAbs, target); err == nil {
			rel = filepath.ToSlash(rel)
			if rel != ".." && !strings.HasPrefix(rel, "../") {
				return rel
			}
		}
	}
	return filepath.ToSlash(filepath.Base(target))
}
