package fs

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDocx 造一个最小 docx：正文段落放在 word/document.xml。
func buildDocx(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		b.WriteString(`<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`)
	}
	b.WriteString(`</w:body></w:document>`)
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildXlsx 造一个最小 xlsx：共享字符串 + 一个工作表（含共享字符串引用与数字）。
func buildXlsx(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	shared, _ := zw.Create("xl/sharedStrings.xml")
	_, _ = shared.Write([]byte(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<si><t>姓名</t></si><si><t>张三</t></si></sst>`))

	sheet, _ := zw.Create("xl/worksheets/sheet1.xml")
	_, _ = sheet.Write([]byte(`<worksheet><sheetData>` +
		`<row><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
		`<row><c r="A2"><v>42</v></c></row>` +
		`</sheetData></worksheet>`))

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDocxText(t *testing.T) {
	got, err := ExtractDocxText(buildDocx(t, "第一段", "第二段"))
	if err != nil {
		t.Fatalf("应当抽出文本: %v", err)
	}
	if !strings.Contains(got, "第一段") || !strings.Contains(got, "第二段") {
		t.Fatalf("段落内容不对: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("段落之间应当有换行: %q", got)
	}
}

func TestExtractXlsxText(t *testing.T) {
	got, err := ExtractXlsxText(buildXlsx(t))
	if err != nil {
		t.Fatalf("应当抽出文本: %v", err)
	}
	if !strings.Contains(got, "姓名\t张三") {
		t.Fatalf("共享字符串应当按行列拼出: %q", got)
	}
	if !strings.Contains(got, "42") {
		t.Fatalf("数字单元格也应当保留: %q", got)
	}
}

func TestExtractOfficeRejectsGarbage(t *testing.T) {
	if _, err := ExtractDocxText([]byte("not a zip")); err == nil {
		t.Fatal("非 zip 应当报错")
	}
	if _, err := ExtractXlsxText([]byte("not a zip")); err == nil {
		t.Fatal("非 zip 应当报错")
	}
	// 是合法 zip，但缺 document.xml：必须明确报错，不能返回空串假装成功。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("hello.txt")
	_, _ = w.Write([]byte("hi"))
	_ = zw.Close()
	if _, err := ExtractDocxText(buf.Bytes()); err == nil {
		t.Fatal("缺 word/document.xml 应当报错")
	}
}

// read 工具遇到 .docx / .xlsx 要走抽取路径。
func TestReadToolExtractsOffice(t *testing.T) {
	root := t.TempDir()
	docx := filepath.Join(root, "doc.docx")
	if err := os.WriteFile(docx, buildDocx(t, "合同正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	xlsx := filepath.Join(root, "book.xlsx")
	if err := os.WriteFile(xlsx, buildXlsx(t), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := execFor(t, root)

	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "doc.docx"})
	if err != nil {
		t.Fatalf("读取 docx 应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "合同正文") || res.Display["office"] != "docx" {
		t.Fatalf("docx 抽取不对: content=%q display=%v", res.Content, res.Display)
	}

	res, err = call(ReadTool{}, ex, map[string]any{"file_path": "book.xlsx"})
	if err != nil {
		t.Fatalf("读取 xlsx 应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "姓名") || res.Display["office"] != "xlsx" {
		t.Fatalf("xlsx 抽取不对: content=%q display=%v", res.Content, res.Display)
	}
}
