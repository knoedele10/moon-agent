// Package fs 实现契约 §4 的四个文件工具（read / write / edit / read_image）。
//
// 三条最容易写错的语义，这里刻意做成了结构性约束：
//  1. 观察策略是**三态**（没见过 / 确认不存在 / 存在），且 read 失败也要记 absent（§4.2）；
//  2. edit 的版本守卫在**字面量匹配之前**（§4.6），陈旧编辑绝不报「找不到匹配」；
//  3. createIfAbsent 用 os.Link 做原子「不覆盖创建」，EEXIST → FS_NOT_OBSERVED（§4.4）。
package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 错误码：原样保留供机器路由（契约 §4.5）。
const (
	CodeSandboxDenied = "FS_SANDBOX_DENIED"
	CodeNotObserved   = "FS_NOT_OBSERVED"
	CodeStaleVersion  = "FS_STALE_VERSION"
	CodeNotFound      = "FS_NOT_FOUND"
	CodeInvalidArgs   = "FS_INVALID_ARGS"
	CodeIOError       = "FS_IO_ERROR"
	CodeNoMatch       = "FS_NO_MATCH"
	CodeAmbiguous     = "FS_AMBIGUOUS_MATCH"
	CodeImageTooLarge = "FS_IMAGE_TOO_LARGE"
)

// Error 是 fs 工具的统一错误。
//
// Code 供机器路由；Error() 返回的文案**直接给模型看**（契约 §4.5：provider 只管原因，
// 给模型看的话由工具层统一）。
type Error struct {
	Code   string
	Path   string // 展示用路径（根目录相对形式），不含绝对路径
	Reason string // 具体原因；FS_STALE_VERSION 时带 expected/found
}

func (e *Error) Error() string {
	switch e.Code {
	case CodeNotObserved:
		return fmt.Sprintf("cannot modify \"%s\": file has not been read — read the file, then retry", e.Path)
	case CodeStaleVersion:
		reason := e.Reason
		if reason == "" {
			reason = fmt.Sprintf("cannot modify \"%s\": the file changed on disk since it was read", e.Path)
		}
		return reason + " — re-read the file, then retry"
	case CodeNotFound:
		if e.Reason != "" {
			return e.Reason // read 场景自带文案
		}
		return fmt.Sprintf("cannot edit \"%s\": file does not exist — create it with write first", e.Path)
	case CodeSandboxDenied:
		if e.Reason != "" {
			return "[sandbox: " + e.Reason + "]"
		}
		return "[sandbox: 拒绝访问根目录之外的路径]"
	case CodeInvalidArgs:
		return "invalid arguments: " + e.Reason
	case CodeNoMatch:
		return fmt.Sprintf("cannot edit \"%s\": old_string not found in the file", e.Path)
	case CodeAmbiguous:
		return fmt.Sprintf("cannot edit \"%s\": old_string appears %s — pass replace_all=true or use a longer unique string", e.Path, e.Reason)
	case CodeImageTooLarge:
		return fmt.Sprintf("cannot read image \"%s\": %s", e.Path, e.Reason)
	case CodeIOError:
		return fmt.Sprintf("io error on \"%s\": %s", e.Path, e.Reason)
	}
	if e.Reason != "" {
		return e.Reason
	}
	return e.Code
}

// absPathRe 匹配盘符绝对路径（C:\ 或 C:/）与 UNC 路径（\\server\share）。
var absPathRe = regexp.MustCompile(`(?i)[a-z]:[\\/][^\s"']*|\\\\[^\s"']*`)

// ioCause 提取 IO 错误的**不含绝对路径**的原因文本。
//
// 契约 §4.5 要求面向模型的文本只出现相对路径，而 os.PathError / os.LinkError 的
// Error() 会把绝对路径拼进消息里（例如
// `open C:\root\.a.txt:stream.123.tmp: The filename, directory name, or volume label syntax is incorrect.`），
// 直接塞给模型就是路径泄漏。所以优先取它们的 .Err —— 那里只有原因、没有路径。
func ioCause(err error, target, display string) string {
	if err == nil {
		return ""
	}
	var pe *os.PathError
	var le *os.LinkError
	var msg string
	switch {
	case errors.As(err, &pe) && pe.Err != nil:
		msg = pe.Err.Error()
	case errors.As(err, &le) && le.Err != nil:
		msg = le.Err.Error()
	default:
		msg = err.Error()
	}
	msg = stripAbsPaths(msg, target, display)
	// 操作系统 ACL / 只读目录错误不是 agent 沙箱拒绝。把两者区分开，
	// 告诉用户该改工作区位置或 OS 文件权限，而不是反复切换 agent 档位。
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "access is denied") || strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "operation not permitted") || strings.Contains(lower, "eacces") || strings.Contains(lower, "eperm") {
		msg += "；当前路径可能受系统访问控制限制；若是在写入，请在设置中换一个可写工作区，或检查该目录的系统文件权限"
	}
	return msg
}

// stripAbsPaths 是最后一道防线：即便底层错误文本里带了路径，
// 也把 target 换成展示用相对路径、把同目录前缀与残留的绝对路径抹掉。
func stripAbsPaths(msg, target, display string) string {
	if target != "" && display != "" {
		msg = strings.ReplaceAll(msg, target, display)
	}
	if dir := filepath.Dir(target); dir != "" && dir != "." {
		msg = strings.ReplaceAll(msg, dir+string(filepath.Separator), "")
		msg = strings.ReplaceAll(msg, filepath.ToSlash(dir)+"/", "")
		msg = strings.ReplaceAll(msg, dir, ".")
	}
	return absPathRe.ReplaceAllString(msg, "[path]")
}

// AsError 取出 fs 工具的结构化错误（web/agent 层路由用）。
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf 返回错误码；非 fs 错误返回空串。
func CodeOf(err error) string {
	if e, ok := AsError(err); ok {
		return e.Code
	}
	return ""
}
