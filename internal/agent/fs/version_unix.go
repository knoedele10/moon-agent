//go:build linux

package fs

import (
	"os"
	"syscall"
)

// Linux：*syscall.Stat_t 的 Dev/Ino/Size/Mtim/Ctim。
func versionParts(fi os.FileInfo) versionPartsT {
	p := versionPartsT{size: fi.Size()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		p.dev = uint64(st.Dev)
		p.ino = uint64(st.Ino)
		p.size = st.Size
		p.mtimeNs = st.Mtim.Nano()
		p.ctimeNs = st.Ctim.Nano()
	}
	return p
}
