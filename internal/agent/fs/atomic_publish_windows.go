package fs

import (
	"io/fs"
	"os"
	"syscall"
)

// MoveFileW 在同一卷内原子移动，目标已存在时拒绝覆盖。
// 这也适用于禁止创建硬链接但允许普通文件移动的 Windows 工作区。
func publishNewFile(source, target string) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := syscall.MoveFile(from, to); err != nil {
		if os.IsExist(err) {
			return fs.ErrExist
		}
		return err
	}
	return nil
}
