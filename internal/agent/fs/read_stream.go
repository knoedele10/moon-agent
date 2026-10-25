package fs

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// 大文件流式读取。
//
// read 的语义是「按行号窗口查看」，不是「把文件全塞给模型」。
// 但旧实现先 os.ReadFile 再切窗口：文件 500MB 时，模型只要第 10 行也会把进程内存打满。
// 这里按行推进，只保留需要的窗口；offset 仍然要逐行跳过（文本文件没有索引），
// 但内存占用恒定为一行 + 输出窗口。

// normalizedOffset 把 offset 归一成 1 起的行号。
func normalizedOffset(offset int) int {
	if offset < 1 {
		return 1
	}
	return offset
}

// readTextWindow 从 path 读取 [offset, offset+limit) 行，返回带行号的文本。
//
// truncated 表示后面还有内容；调用方据此提示「用 offset 继续」。
// 返回的 shown 是实际读到的行数（文件比 offset 短时为 0）。
func readTextWindow(path string, offset, limit int) (text string, shown int, truncated bool, err error) {
	offset = normalizedOffset(offset)
	if limit <= 0 {
		limit = defaultReadLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var b strings.Builder
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo >= offset && shown < limit {
				b.WriteString(strconv.Itoa(lineNo))
				b.WriteByte('\t')
				b.WriteString(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
				b.WriteByte('\n')
				shown++
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", 0, false, readErr
		}
		if shown >= limit {
			// 只看有没有下一个字节，不把下一行读进内存。
			if _, peekErr := reader.Peek(1); peekErr == nil {
				return b.String(), shown, true, nil
			} else if peekErr != io.EOF {
				return "", 0, false, peekErr
			}
			return b.String(), shown, false, nil
		}
	}
	return b.String(), shown, false, nil
}
