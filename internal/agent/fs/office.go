package fs

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Office 文档（docx / xlsx）本质是 zip + XML，所以不引第三方库也能抽文本。
// 能力边界：只抽文字，不还原样式、公式、图表、批注；图片与嵌入对象一律忽略。
// 抽不出文本时明确报错，不返回空字符串假装成功。

// officeKind 按扩展名判断是不是我们能解析的 Office 文档。
func officeKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	}
	return ""
}

func readZipEntry(zr *zip.Reader, name string) ([]byte, bool) {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			rc, err := f.Open()
			if err != nil {
				return nil, false
			}
			defer rc.Close()
			b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
			if err != nil {
				return nil, false
			}
			return b, true
		}
	}
	return nil, false
}

// ExtractDocxText 抽取 Word 正文：word/document.xml 里的 <w:t> 文本，段落之间换行。
func ExtractDocxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("不是有效的 docx（zip 打不开）")
	}
	doc, ok := readZipEntry(zr, "word/document.xml")
	if !ok {
		return "", fmt.Errorf("docx 里找不到 word/document.xml")
	}

	dec := xml.NewDecoder(bytes.NewReader(doc))
	var (
		b          strings.Builder
		inText     bool
		sawAny     bool
		inCell     bool // 在表格单元格里：段落结尾不换行，否则单元格间会多出空行
		cellsInRow int  // 同一行已有几个单元格，用来插 " | " 分隔符
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("解析 docx 正文失败: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br":
				b.WriteByte('\n')
			case "tbl":
				b.WriteString("\n[表格]\n")
			case "tr":
				cellsInRow = 0
			case "tc":
				if cellsInRow > 0 {
					b.WriteString(" | ")
				}
				cellsInRow++
				inCell = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if !inCell {
					b.WriteByte('\n')
				}
			case "tc":
				inCell = false
			case "tr":
				b.WriteByte('\n')
			case "tbl":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write([]byte(t))
				sawAny = true
			}
		}
	}
	out := strings.TrimSpace(b.String())
	if !sawAny || out == "" {
		return "", fmt.Errorf("docx 里没有抽到文字（可能是空文档或全是图片）")
	}
	return out, nil
}

// ExtractXlsxText 抽取 Excel 文本：共享字符串 + 每个工作表的单元格，按行/列用制表符拼。
func ExtractXlsxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("不是有效的 xlsx（zip 打不开）")
	}

	shared := parseSharedStrings(zr)

	// 收集所有工作表，按文件名排序，保证输出顺序稳定。
	var sheets []string
	for _, f := range zr.File {
		n := f.Name
		if strings.HasPrefix(n, "xl/worksheets/sheet") && strings.HasSuffix(n, ".xml") {
			sheets = append(sheets, n)
		}
	}
	sort.Strings(sheets)
	if len(sheets) == 0 {
		return "", fmt.Errorf("xlsx 里找不到工作表")
	}

	var out strings.Builder
	for i, name := range sheets {
		body, ok := readZipEntry(zr, name)
		if !ok {
			continue
		}
		if len(sheets) > 1 {
			fmt.Fprintf(&out, "[工作表 %d]\n", i+1)
		}
		out.WriteString(parseSheet(body, shared))
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", fmt.Errorf("xlsx 里没有抽到文字（可能全是空表或纯公式）")
	}
	return text, nil
}

// parseSharedStrings 解析 xl/sharedStrings.xml：每个 <si> 是一个共享字符串。
func parseSharedStrings(zr *zip.Reader) []string {
	body, ok := readZipEntry(zr, "xl/sharedStrings.xml")
	if !ok {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var (
		out    []string
		cur    strings.Builder
		inText bool
		inSi   bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSi = true
				cur.Reset()
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "si":
				inSi = false
				out = append(out, cur.String())
			}
		case xml.CharData:
			if inSi && inText {
				cur.Write([]byte(t))
			}
		}
	}
	return out
}

// parseSheet 解析一个工作表：<row> 换行，<c> 用制表符分隔。
func parseSheet(body []byte, shared []string) string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var (
		out      strings.Builder
		rowCells []string
		cur      strings.Builder
		inValue  bool
		inText   bool
		cellType string
		rowOpen  bool
	)
	flushRow := func() {
		if rowOpen {
			out.WriteString(strings.Join(rowCells, "\t"))
			out.WriteByte('\n')
			rowCells = rowCells[:0]
			rowOpen = false
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				rowOpen = true
			case "c":
				cellType = ""
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
				}
				cur.Reset()
			case "v":
				inValue = true
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				flushRow()
			case "c":
				val := cur.String()
				switch cellType {
				case "s": // 共享字符串下标
					if idx, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && idx >= 0 && idx < len(shared) {
						val = shared[idx]
					}
				case "inlineStr":
					// 已在 <t> 里累积
				}
				rowCells = append(rowCells, val)
				cur.Reset()
			case "v":
				inValue = false
			case "t":
				inText = false
			}
		case xml.CharData:
			if inValue || inText {
				cur.Write([]byte(t))
			}
		}
	}
	flushRow()
	return out.String()
}
