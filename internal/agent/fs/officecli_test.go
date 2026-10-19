package fs

import (
	"path/filepath"
	"strings"
	"testing"
)

// 这套用例需要本机装有 OfficeCLI（用来出 pptx 骨架 + 校验）。
// 没装就跳过——它测的是"接入 OfficeCLI 之后"的行为，不装也该让全量测试通过。
func requireOfficeCLI(t *testing.T) {
	t.Helper()
	if OfficeCLIPath() == "" {
		t.Skip("本机没有 OfficeCLI，跳过 pptx 新建用例")
	}
}

// 决定性用例：新建 pptx → 能被读回 → 能通过 OfficeCLI 的 OpenXML 校验。
//
// 注意 officeCreate 内部已经在写盘后调了 validate，校验不过会把文件删掉并报错，
// 所以这个用例一旦通过，就说明生成的是**schema 合法**的 pptx，而不是"看起来像"。
func TestOfficeCreatePptxWithOfficeCLI(t *testing.T) {
	requireOfficeCLI(t)
	root := tempWorkspace(t)
	ex := execFor(t, root)

	slides := []any{
		map[string]any{"title": "季度复盘", "bullets": []any{"营收 +12%", "海外市场是重点"}},
		map[string]any{"title": "下一步", "bullets": []any{"扩编两人"}},
	}
	res, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "deck.pptx", "kind": "pptx", "slides": slides,
	})
	if err != nil {
		t.Fatalf("新建 pptx 应当成功（含 OpenXML 校验）: %v", err)
	}
	if !strings.Contains(res.Content, "Created") {
		t.Fatalf("应当报告 Created: %q", res.Content)
	}

	// 读回来：验证我们注入的幻灯片真的在里面。
	readRes, err := call(ReadTool{}, ex, map[string]any{"file_path": "deck.pptx"})
	if err != nil {
		t.Fatalf("读回 pptx 应当成功: %v", err)
	}
	for _, want := range []string{"[第 1 张幻灯片]", "季度复盘", "营收 +12%", "[第 2 张幻灯片]", "扩编两人"} {
		if !strings.Contains(readRes.Content, want) {
			t.Fatalf("读回内容缺少 %q: %q", want, readRes.Content)
		}
	}

	// 再独立校验一次（保险：万一将来有人把 create 里的校验去掉，这条会红）。
	if out, verr := OfficeCLIValidate(t.Context(), filepath.Join(root, "deck.pptx")); verr != nil {
		t.Fatalf("生成的 pptx 没通过 OpenXML 校验: %v\n%s", verr, out)
	}
}

// docx/xlsx 仍然走内置实现，但同样要过 officecli 的校验。
func TestOfficeCreateDocxPassesValidation(t *testing.T) {
	requireOfficeCLI(t)
	root := tempWorkspace(t)
	ex := execFor(t, root)
	if _, err := call(OfficeEditTool{}, ex, map[string]any{
		"action": "create", "file_path": "a.docx", "kind": "docx", "lines": []string{"第一段", "第二段"},
	}); err != nil {
		t.Fatalf("新建 docx 应当成功: %v", err)
	}
	if out, verr := OfficeCLIValidate(t.Context(), filepath.Join(root, "a.docx")); verr != nil {
		t.Fatalf("内置生成的 docx 没通过校验: %v\n%s", verr, out)
	}
}
