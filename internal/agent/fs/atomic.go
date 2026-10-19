package fs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// writeFileAtomic 按契约 §4.4 原子落盘：同目录暂存 → Sync → 发布。
//
//	createIfAbsent=true  → publishNewFile：原子的「不覆盖创建」；EEXIST → FS_NOT_OBSERVED
//	createIfAbsent=false → os.Rename：覆盖写（Windows 上 os.Rename 覆盖已存在目标）
//
// 暂存文件与目标同目录，保证发布操作在同一卷完成。
// **提交后清理暂存文件失败要吞掉**：一次成功的写不能因为清理失败变成失败。
func writeFileAtomic(target, display string, data []byte, createIfAbsent bool) (version string, created bool, err error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)

	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	tmpName := tmp.Name()
	// 无论走哪条分支都尝试清理；错误一律吞掉（Rename 后 temp 已不存在，Remove 会 ENOENT）。
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if err := tmp.Close(); err != nil {
		return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}

	if createIfAbsent {
		if err := publishNewFile(tmpName, target); err != nil {
			if errors.Is(err, fs.ErrExist) {
				// 契约 §4.4：不能覆盖没读过的文件。
				return "", false, &Error{Code: CodeNotObserved, Path: display}
			}
			return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
		}
		created = true
	} else {
		if err := os.Rename(tmpName, target); err != nil {
			return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
		}
	}

	fi, err := os.Stat(target)
	if err != nil {
		return "", created, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	return versionOf(fi), created, nil
}
