package fs

import (
	"fmt"
	"os"
)

// versionPartsT 是版本令牌的原料；各平台文件分别填（契约 §4.3）。
type versionPartsT struct {
	dev, ino uint64
	size     int64
	mtimeNs  int64
	ctimeNs  int64
}

// versionOf 生成契约 §4.3 的版本令牌：dev:ino:size:mtimeNs:ctimeNs。
//
// 令牌是 edit/write 的 CAS 基准：外部只要动过文件（内容、大小、时间），令牌就会变。
func versionOf(fi os.FileInfo) string {
	p := versionParts(fi)
	if p.size == 0 {
		p.size = fi.Size()
	}
	if p.mtimeNs == 0 {
		p.mtimeNs = fi.ModTime().UnixNano()
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", p.dev, p.ino, p.size, p.mtimeNs, p.ctimeNs)
}
