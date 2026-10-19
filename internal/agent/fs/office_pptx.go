package fs

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ExtractPptxText 抽取 PowerPoint 文本：按 slideN.xml 的数字顺序逐页输出。
//
// 能力边界：只抽文字（标题、正文、文本框），**不还原版式**，也不抽图片/图表/动画。
// 表格里的文字会按出现顺序抽出来，但拿不到行列结构——pptx 的表格没有像 docx 那样
// 规范的 <w:tc> 层级（用的是 a:tbl/a:tr/a:tc，实现里会尽量按行拼，但不保证）。
func ExtractPptxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("不是有效的 pptx（zip 打不开）")
	}

	type slideRef struct {
		n    int
		name string
	}
	var slides []slideRef
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "ppt/slides/slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		slides = append(slides, slideRef{n: n, name: name})
	}
	if len(slides) == 0 {
		return "", fmt.Errorf("pptx 里找不到幻灯片")
	}
	// 按页码排：zip 里的顺序不保证，slide2 排在 slide10 前面才是人们期望的。
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })

	var out strings.Builder
	for _, s := range slides {
		body, ok := readZipEntry(zr, s.name)
		if !ok {
			continue
		}
		texts := extractSlideTexts(body)
		if len(texts) == 0 {
			continue
		}
		fmt.Fprintf(&out, "[第 %d 张幻灯片]\n", s.n)
		for _, t := range texts {
			out.WriteString(t)
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}

	res := strings.TrimSpace(out.String())
	if res == "" {
		return "", fmt.Errorf("pptx 里没有抽到文字（可能全是图片或图表）")
	}
	return res, nil
}

// extractSlideTexts 按出现顺序取幻灯片里的文本段落。
//
// 用 a:p（段落）收口而不是 a:t：一个段落里可能有多个 run（<a:t> 分片），
// 按段落拼接才不会把一句话拆成好几行。
func extractSlideTexts(body []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out []string
	var para strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				para.Reset()
			case "t":
				inText = true
			case "br":
				para.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if s := strings.TrimSpace(para.String()); s != "" {
					out = append(out, s)
				}
			}
		case xml.CharData:
			if inText {
				para.Write([]byte(t))
			}
		}
	}
	return out
}
