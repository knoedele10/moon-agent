package fs

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestPDF 造一个最小可解析的 PDF：文本流用 FlateDecode 压缩，
// 正是 Word/LaTeX 这类工具最常见的输出形态。
func buildTestPDF(t *testing.T, text string) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write([]byte("BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	buf.WriteString("1 0 obj\n<< /Length 0 /Filter /FlateDecode >>\nstream\n")
	buf.Write(z.Bytes())
	buf.WriteString("\nendstream\nendobj\n%%EOF\n")
	return buf.Bytes()
}

func TestExtractPDFTextFromFlateStream(t *testing.T) {
	pdf := buildTestPDF(t, "Hello PDF")
	got, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("应当抽出文本: %v", err)
	}
	if !strings.Contains(got, "Hello PDF") {
		t.Fatalf("抽出的文本不对: %q", got)
	}
}

func TestExtractPDFTextRejectsNonPDF(t *testing.T) {
	if _, err := ExtractPDFText([]byte("just a text file")); err == nil {
		t.Fatal("非 PDF 应当报错")
	}
}

func TestExtractPDFTextReportsScannedPdf(t *testing.T) {
	// 只有图片、没有文本操作符的 PDF：必须明确报“抽不到文本层”，不能返回空字符串假装成功。
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< >>\nstream\n\x00\x01\x02binary\nendstream\nendobj\n%%EOF\n")
	if _, err := ExtractPDFText(pdf); err == nil {
		t.Fatal("没有文本层时应当报错")
	}
}

// read 工具遇到 .pdf 要走抽取路径，而不是把二进制当文本按行号吐出来。
func TestReadToolExtractsPDF(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "doc.pdf")
	if err := os.WriteFile(p, buildTestPDF(t, "From read tool"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := execFor(t, root)

	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "doc.pdf"})
	if err != nil {
		t.Fatalf("读取 PDF 应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "From read tool") {
		t.Fatalf("应当返回抽取出的文本: %q", res.Content)
	}
	if res.Display["pdf"] != true {
		t.Fatalf("Display 应当标记 pdf=true: %v", res.Display)
	}
}
