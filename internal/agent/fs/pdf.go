package fs

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 能力边界（必须对模型说清，别让模型以为 PDF 万能）：
//   - 只处理未加密、文本用 Tj/TJ 操作符的常见 PDF；
//   - 支持 FlateDecode（zlib / raw deflate）压缩的流；
//   - 扫描件（图片型 PDF）没有文本层，抽不出来——那种要用 read_image；
//   - 不做字体编码映射，CID 字体可能抽出乱码。
//
// 不引第三方 PDF 库是刻意的：一个文本抽取功能不值得为它加一份依赖和供应链风险。

var (
	pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n`)
	pdfEndRe    = regexp.MustCompile(`(?s)\r?\nendstream`)
	pdfTjRe     = regexp.MustCompile(`(?s)\((?:[^()\\]|\\.)*\)\s*Tj`)
	pdfTJRe     = regexp.MustCompile(`(?s)\[(?:[^\[\]\\]|\\.)*\]\s*TJ`)
	pdfParenRe  = regexp.MustCompile(`(?s)\((?:[^()\\]|\\.)*\)`)
	pdfHexRe    = regexp.MustCompile(`(?s)<([0-9A-Fa-f\s]+)>\s*Tj`)
)

// isPDF 判断要不要走 PDF 文本抽取：看扩展名，也看魔数（改了名的 PDF 也能读）。
func isPDF(path string, data []byte) bool {
	if strings.EqualFold(filepath.Ext(path), ".pdf") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimLeft(data, "\xef\xbb\xbf \r\n\t"), []byte("%PDF"))
}

// ExtractPDFText 尽力从 PDF 字节里抽出文本层。
func ExtractPDFText(data []byte) (string, error) {
	trimmed := bytes.TrimLeft(data, "\xef\xbb\xbf \r\n\t")
	if !bytes.HasPrefix(trimmed, []byte("%PDF")) {
		return "", fmt.Errorf("不是 PDF 文件")
	}

	var parts []string
	offset := 0
	for {
		loc := pdfStreamRe.FindIndex(data[offset:])
		if loc == nil {
			break
		}
		streamStart := offset + loc[1]
		endLoc := pdfEndRe.FindIndex(data[streamStart:])
		if endLoc == nil {
			break
		}
		raw := data[streamStart : streamStart+endLoc[0]]
		offset = streamStart + endLoc[1]

		content := raw
		if isFlate(data, offset, streamStart) {
			if inflated, err := inflateStream(raw); err == nil {
				content = inflated
			}
		}
		if text := extractTextOperators(content); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}

	if len(parts) == 0 {
		return "", fmt.Errorf("没有抽到文本层（可能是扫描件、加密 PDF，或用了不支持的编码）")
	}
	return strings.Join(parts, "\n"), nil
}

// isFlate 回头看这段 stream 之前一小段字典，判断有没有 /FlateDecode。
func isFlate(data []byte, _ int, streamStart int) bool {
	start := streamStart - 600
	if start < 0 {
		start = 0
	}
	dict := data[start:streamStart]
	return bytes.Contains(dict, []byte("/FlateDecode"))
}

func inflateStream(raw []byte) ([]byte, error) {
	if r, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer r.Close()
		if out, err := io.ReadAll(io.LimitReader(r, 8<<20)); err == nil {
			return out, nil
		}
	}
	// 有些 PDF 用 raw deflate（没有 zlib 头）。
	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	return io.ReadAll(io.LimitReader(fr, 8<<20))
}

// extractTextOperators 从内容流里抓 Tj / TJ 的字符串。
func extractTextOperators(content []byte) string {
	var b strings.Builder
	// 按出现顺序处理两类操作符：TJ 数组整体替换成它的字符串拼接。
	type piece struct {
		at   int
		text string
	}
	var pieces []piece

	for _, m := range pdfTjRe.FindAllIndex(content, -1) {
		raw := content[m[0]:m[1]]
		if s := pdfParenRe.Find(raw); s != nil {
			pieces = append(pieces, piece{at: m[0], text: decodePDFString(s)})
		}
	}
	for _, m := range pdfTJRe.FindAllIndex(content, -1) {
		raw := content[m[0]:m[1]]
		var sb strings.Builder
		for _, s := range pdfParenRe.FindAll(raw, -1) {
			sb.WriteString(decodePDFString(s))
		}
		if sb.Len() > 0 {
			pieces = append(pieces, piece{at: m[0], text: sb.String()})
		}
	}
	// 十六进制字符串形式：<48656C6C6F> Tj
	for _, m := range pdfHexRe.FindAllSubmatchIndex(content, -1) {
		hex := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, string(content[m[2]:m[3]]))
		if len(hex)%2 != 0 {
			hex = "0" + hex
		}
		if decoded, derr := hexToBytes(hex); derr == nil {
			pieces = append(pieces, piece{at: m[0], text: string(decoded)})
		}
	}

	// 按位置排序，保持原文顺序。
	for i := 1; i < len(pieces); i++ {
		for j := i; j > 0 && pieces[j-1].at > pieces[j].at; j-- {
			pieces[j-1], pieces[j] = pieces[j], pieces[j-1]
		}
	}
	for i, p := range pieces {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.text)
	}
	return b.String()
}

func hexToBytes(s string) ([]byte, error) {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		v, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out = append(out, byte(v))
	}
	return out, nil
}

// decodePDFString 解开 PDF 字面量字符串里的转义。
func decodePDFString(raw []byte) string {
	s := string(raw)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = s[1 : len(s)-1]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '(', ')', '\\':
			b.WriteByte(s[i])
		case '\n':
			// 续行
		default:
			// 八进制转义 \ddd
			if s[i] >= '0' && s[i] <= '7' {
				j := i
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					j++
				}
				if v, err := strconv.ParseUint(s[i:j], 8, 16); err == nil {
					b.WriteByte(byte(v))
					i = j - 1
					continue
				}
			}
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
