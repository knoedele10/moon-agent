package fs

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 新建 docx → 立刻用 read 读回来，形成“生成 → 解析”的闭环验证。
// tempWorkspace 与 t.TempDir() 的区别只有一个：清理失败不判测试失败。
// officecli 的驻留进程会短暂占用文件（Windows 上表现为删不掉临时目录），
// 那是外部工具的行为，不该让被测代码背锅。
func tempWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "blogmy-fs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestOfficeCreateDocxRoundTrip(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)

	res, err := call(OfficeEditTool{}, ex, map[string]any{
		"action":    "create",
		"file_path": "notes/report.docx",
		"kind":      "docx",
		"title":     "季度总结",
		"lines":     []string{"营收同比增长 12%", "下季度重点：海外市场"},
	})
	if err != nil {
		t.Fatalf("新建 docx 应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "Created") {
		t.Fatalf("应当报告 Created: %q", res.Content)
	}

	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "notes/report.docx"})
	if err != nil {
		t.Fatalf("读回新建的 docx 应当成功: %v", err)
	}
	for _, want := range []string{"季度总结", "营收同比增长 12%", "海外市场"} {
		if !strings.Contains(readRes.Content, want) {
			t.Fatalf("读回内容缺少 %q: %q", want, readRes.Content)
		}
	}
}

func TestOfficeCreateXlsxRoundTrip(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)

	sheets := []any{
		map[string]any{
			"name": "预算",
			"rows": []any{
				[]any{"项目", "金额"},
				[]any{"服务器", "1200"},
				[]any{"域名", "80"},
			},
		},
	}
	if _, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "book.xlsx", "kind": "xlsx", "sheets": sheets,
	}); err != nil {
		t.Fatalf("新建 xlsx 应当成功: %v", err)
	}

	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "book.xlsx"})
	if err != nil {
		t.Fatalf("读回 xlsx 应当成功: %v", err)
	}
	if !strings.Contains(readRes.Content, "项目\t金额") {
		t.Fatalf("表头应当按行列拼出: %q", readRes.Content)
	}
	if !strings.Contains(readRes.Content, "服务器\t1200") {
		t.Fatalf("数据行应当保留: %q", readRes.Content)
	}
}

// pptx 能不能新建取决于本机有没有 OfficeCLI：
//   - 装了 → 用它出骨架 + 我们注入幻灯片（正向用例见 officecli_test.go）；
//   - 没装 → 必须给出可执行的替代建议，而不是生成一个 PowerPoint 打不开的文件。
func TestOfficeCreatePptxWithoutOfficeCLI(t *testing.T) {
	if OfficeCLIPath() != "" {
		t.Skip("本机装了 OfficeCLI，pptx 新建可用；该分支由 officecli_test.go 覆盖")
	}
	root := tempWorkspace(t)
	ex := execFor(t, root)
	_, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "deck.pptx", "kind": "pptx",
	})
	mustCode(t, err, CodeInvalidArgs)
	if err == nil || !strings.Contains(err.Error(), "OfficeCLI") {
		t.Fatalf("报错里应当给出替代做法: %v", err)
	}
}

// 修改：必须先 read，改完再读回来验证真的落盘了。
func TestOfficeReplaceRequiresReadThenWorks(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)

	// 用工具之外的方式落盘：这样文件对观察表来说是“没见过”的，
	// 才能验证“没读过就不许改”这条约束（用 office_edit create 建的话，
	// 创建本身就记录了观察，改的时候自然放行）。
	data, cerr := CreateDocx("", []string{"上线窗口是周四"})
	if cerr != nil {
		t.Fatal(cerr)
	}
	if werr := os.WriteFile(filepath.Join(root, "a.docx"), data, 0o644); werr != nil {
		t.Fatal(werr)
	}

	// 没读过就改 → 拒绝
	_, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "replace", "file_path": "a.docx", "find": "周四", "replace_text": "周五",
	})
	mustCode(t, err, CodeNotObserved)

	// 读过之后再改 → 成功
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.docx"}); err != nil {
		t.Fatal(err)
	}
	res, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "replace", "file_path": "a.docx", "find": "周四", "replace_text": "周五",
	})
	if err != nil {
		t.Fatalf("替换应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "1 处替换") {
		t.Fatalf("应当报告替换处数: %q", res.Content)
	}

	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.docx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readRes.Content, "周五") || strings.Contains(readRes.Content, "周四") {
		t.Fatalf("替换结果不对: %q", readRes.Content)
	}
}

func TestOfficeReplaceNoMatchReportsClearly(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)
	if _, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "a.docx", "kind": "docx", "lines": []string{"原文"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.docx"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "replace", "file_path": "a.docx", "find": "不存在的词", "replace_text": "x",
	})
	mustCode(t, err, CodeNoMatch)
}

// 另存：内容一致，且不覆盖已存在的目标。
func TestOfficeSaveAs(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)
	if _, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "src.docx", "kind": "docx", "lines": []string{"内容ABC"},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "save_as", "file_path": "src.docx", "to_path": "backup/src-copy.docx",
	})
	if err != nil {
		t.Fatalf("另存应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "Saved") {
		t.Fatalf("应当报告 Saved: %q", res.Content)
	}
	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "backup/src-copy.docx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readRes.Content, "内容ABC") {
		t.Fatalf("另存内容应当一致: %q", readRes.Content)
	}

	// 目标已存在 → 拒绝，不覆盖
	mustWriteFile(t, filepath.Join(root, "exists.docx"), "x")
	_, err = call(OfficeEditTool{}, ex, map[string]any{
		"action": "save_as", "file_path": "src.docx", "to_path": "exists.docx",
	})
	mustCode(t, err, CodeInvalidArgs)
}

// pptx 不能新建，但**能改**：造一个最小 pptx 包，验证替换与按页读取。
func TestOfficeReplacePptx(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ppt/slides/slide1.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><p:sld xmlns:p="p" xmlns:a="a"><p:cSld><a:p><a:r><a:t>旧标题</a:t></a:r></a:p></p:cSld></p:sld>`))
	w2, _ := zw.Create("ppt/slides/slide2.xml")
	_, _ = w2.Write([]byte(`<?xml version="1.0"?><p:sld xmlns:p="p" xmlns:a="a"><p:cSld><a:p><a:r><a:t>第二页</a:t></a:r></a:p></p:cSld></p:sld>`))
	_ = zw.Close()
	if err := os.WriteFile(filepath.Join(root, "deck.pptx"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "deck.pptx"})
	if err != nil {
		t.Fatalf("读 pptx 应当成功: %v", err)
	}
	if !strings.Contains(readRes.Content, "[第 1 张幻灯片]") || !strings.Contains(readRes.Content, "旧标题") {
		t.Fatalf("应当按页抽到文本: %q", readRes.Content)
	}

	res, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "replace", "file_path": "deck.pptx", "find": "旧标题", "replace_text": "新标题",
	})
	if err != nil {
		t.Fatalf("替换 pptx 文本应当成功: %v", err)
	}
	if !strings.Contains(res.Content, "1 处替换") {
		t.Fatalf("应当报告替换处数: %q", res.Content)
	}
	readRes2, err := call(ReadTool{}, ex, map[string]any{"file_path": "deck.pptx"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readRes2.Content, "新标题") {
		t.Fatalf("替换结果没生效: %q", readRes2.Content)
	}
}

// 沙箱红线：Office 工具同样只允许在工作区内操作。
func TestOfficeEditSandboxDenied(t *testing.T) {
	root := tempWorkspace(t)
	ex := execFor(t, root)
	if _, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "../escaped.docx", "kind": "docx", "lines": []string{"x"},
	}); err == nil {
		t.Fatal("越界路径应当被拒绝")
	}
	mustCode(t, func() error {
		_, err := call(OfficeEditTool{}, ex, map[string]any{
			"action": "create", "file_path": "../escaped.docx", "kind": "docx", "lines": []string{"x"},
		})
		return err
	}(), CodeSandboxDenied)
}
