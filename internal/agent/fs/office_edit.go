package fs

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// Office 文档的**生成与修改**。
//
// 能力边界（必须对模型与用户讲清楚，别让人以为这是完整 Office 套件）：
//   - 新建：支持 docx / xlsx，生成的是**最小可用 OOXML**——有文字/表格/单元格，没有样式、
//     主题、页眉页脚。Word / WPS 能正常打开；
//   - pptx：**不支持从零新建**（缺少 slideMaster/slideLayout/theme 的包，PowerPoint 会报
//     “内容有问题”）。pptx 只支持修改与另存，或者先用 PowerPoint 建好模板再改。
//   - 修改：在 XML 文本节点里做字面量替换（`<w:t>` / `<a:t>` / `<t>`）。
//     一句话被拆成多个 run（Word 里很常见）时匹配不到——这种情况会明确报“没找到”。
//   - 另存：整包复制到新路径，不改内容。

// buildZip 把「路径 → 内容」打成 zip。OOXML 就是一堆 XML 的 zip，不需要额外库。
func buildZip(parts map[string]string, order []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	seen := map[string]bool{}
	write := func(name, content string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}
	for _, name := range order {
		if c, ok := parts[name]; ok {
			if err := write(name, c); err != nil {
				return nil, err
			}
		}
	}
	for name, c := range parts {
		if err := write(name, c); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// xmlEscape 转义 XML 文本（引号也要转，属性里会用到）。
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// ---------- docx ----------

const docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`

const docxRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// CreateDocx 生成一个最小可用的 Word 文档：标题（若有）+ 若干段落。
func CreateDocx(title string, paragraphs []string) ([]byte, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)

	addPara := func(text string) {
		if strings.TrimSpace(text) == "" {
			body.WriteString(`<w:p/>`)
			return
		}
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		body.WriteString(xmlEscape(text))
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	if strings.TrimSpace(title) != "" {
		addPara(title)
	}
	for _, p := range paragraphs {
		addPara(p)
	}
	if len(paragraphs) == 0 && strings.TrimSpace(title) == "" {
		addPara("")
	}
	body.WriteString(`</w:body></w:document>`)

	return buildZip(map[string]string{
		"[Content_Types].xml": docxContentTypes,
		"_rels/.rels":         docxRels,
		"word/document.xml":   body.String(),
	}, []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"})
}

// ---------- xlsx ----------

// SheetInput 是新建 Excel 时的一张表。
type SheetInput struct {
	Name string
	Rows [][]string
}

// CreateXlsx 生成一个最小可用的 Excel 工作簿（单元格用 inlineStr，不引共享字符串表）。
func CreateXlsx(sheets []SheetInput) ([]byte, error) {
	if len(sheets) == 0 {
		return nil, fmt.Errorf("至少需要一张工作表")
	}

	var types strings.Builder
	types.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	types.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	types.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	types.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	types.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	for i := range sheets {
		types.WriteString(fmt.Sprintf(`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1))
	}
	types.WriteString(`</Types>`)

	parts := map[string]string{
		"[Content_Types].xml": types.String(),
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`,
	}
	order := []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels"}

	var wbSheets, wbRels strings.Builder
	wbRels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	wbRels.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)

	for i, sh := range sheets {
		name := strings.TrimSpace(sh.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		wbSheets.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(name), i+1, i+1))
		wbRels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i+1, i+1))

		var sd strings.Builder
		sd.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
		sd.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
		for r, row := range sh.Rows {
			sd.WriteString(fmt.Sprintf(`<row r="%d">`, r+1))
			for c, val := range row {
				if val == "" {
					continue
				}
				sd.WriteString(fmt.Sprintf(`<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
					colName(c), r+1, xmlEscape(val)))
			}
			sd.WriteString(`</row>`)
		}
		sd.WriteString(`</sheetData></worksheet>`)

		parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)] = sd.String()
		order = append(order, fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1))
	}
	wbRels.WriteString(`</Relationships>`)

	parts["xl/workbook.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets>` + wbSheets.String() + `</sheets></workbook>`
	parts["xl/_rels/workbook.xml.rels"] = wbRels.String()

	return buildZip(parts, order)
}

// colName 把 0 基列号转成 A/B/.../AA。
func colName(idx int) string {
	name := ""
	for idx >= 0 {
		name = string(rune('A'+idx%26)) + name
		idx = idx/26 - 1
	}
	return name
}

// ---------- 修改（docx / xlsx / pptx 通用）----------

// ReplaceInOfficeZip 在 OOXML 包的 XML 文本节点里做字面量替换。
//
// 只替换**同一个文本节点内部**出现的串：Word 常把一句话拆进多个 <w:r>，
// 跨 run 的情况这里匹配不到，会以 replaced=0 如实返回，由上层提示用户。
func ReplaceInOfficeZip(data []byte, find, repl string, replaceAll bool) ([]byte, int, error) {
	if strings.TrimSpace(find) == "" {
		return nil, 0, fmt.Errorf("find 不能为空")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("不是有效的 Office 文件（zip 打不开）")
	}

	replaced := 0
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, 0, err
		}
		content := new(bytes.Buffer)
		if _, err := content.ReadFrom(rc); err != nil {
			rc.Close()
			return nil, 0, err
		}
		rc.Close()

		out := content.Bytes()
		// 只动 XML 部件：图片等二进制原样复制。
		if strings.HasSuffix(strings.ToLower(f.Name), ".xml") {
			text := string(out)
			if strings.Contains(text, find) {
				var n int
				if replaceAll {
					n = strings.Count(text, find)
					text = strings.ReplaceAll(text, find, xmlEscape(repl))
				} else {
					n = 1
					text = strings.Replace(text, find, xmlEscape(repl), 1)
				}
				if n > 0 {
					replaced += n
					out = []byte(text)
				}
			}
		}

		w, err := zw.Create(f.Name)
		if err != nil {
			return nil, 0, err
		}
		if _, err := w.Write(out); err != nil {
			return nil, 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	if replaced == 0 {
		return nil, 0, fmt.Errorf("在文档里没找到 %q（可能被拆成了多个文本片段，或本来就没有）", find)
	}
	return buf.Bytes(), replaced, nil
}

// officeEditable 判断这个扩展名是否支持“修改/另存”。
func officeEditable(path string) bool {
	switch officeKind(path) {
	case "docx", "xlsx", "pptx":
		return true
	}
	return false
}

// officeCreateKind 判断是否支持“从零新建”。
// pptx 明确不支持：只带 slides 的包会被 PowerPoint 判为损坏（缺 master/layout/theme）。
func officeCreateKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "docx", "word", "xlsx", "excel":
		return true
	case "pptx", "powerpoint":
		// pptx 也能新建，但**必须**有 OfficeCLI 提供合法骨架（母版/版式/主题），
		// 否则只带幻灯片的包会被 PowerPoint 判为损坏。
		return OfficeCLIPath() != ""
	}
	return false
}

// normalizeCreateKind 把用户写的别名归一。
func normalizeCreateKind(kind, path string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "word":
		return "docx"
	case "excel":
		return "xlsx"
	case "":
		return officeKind(path)
	}
	return k
}
