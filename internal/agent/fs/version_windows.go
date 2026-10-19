//go:build windows

package fs

import (
	"os"
	"syscall"
)

// Windows：os.FileInfo.Sys() 是 *syscall.Win32FileAttributeData，
// 取 LastWriteTime / CreationTime（100ns 精度；Filetime.Nanoseconds() 已换算成 ns）。
// Windows 没有可移植的 dev/ino 概念，置 0。
func versionParts(fi os.FileInfo) versionPartsT {
	p := versionPartsT{size: fi.Size()}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		p.mtimeNs = d.LastWriteTime.Nanoseconds()
		p.ctimeNs = d.CreationTime.Nanoseconds()
	}
	return p
}
