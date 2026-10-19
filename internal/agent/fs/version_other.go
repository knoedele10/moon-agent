//go:build !windows && !linux

package fs

import "os"

// 其它平台（darwin 等）：没有可移植的 inode 取法时退化为 size+mtime。
func versionParts(fi os.FileInfo) versionPartsT {
	return versionPartsT{size: fi.Size(), mtimeNs: fi.ModTime().UnixNano()}
}
