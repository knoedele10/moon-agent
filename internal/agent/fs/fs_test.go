package fs

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/agent"
)

// ---------- read ----------

func TestReadNumberedLines(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "alpha\nbeta\ngamma\n")
	ex := execFor(t, root)

	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := "1\talpha\n2\tbeta\n3\tgamma\n"
	if res.Content != want {
		t.Fatalf("行号格式不对\n got=%q\nwant=%q", res.Content, want)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "l1\nl2\nl3\nl4\nl5\n")
	ex := execFor(t, root)

	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt", "offset": 2, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Content, "2\tl2\n3\tl3\n") {
		t.Fatalf("分页读取不对: %q", res.Content)
	}
	if !strings.Contains(res.Content, "共 5 行") {
		t.Fatalf("应提示还有更多行: %q", res.Content)
	}
}

func TestReadEmptyFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "empty.txt"), "")
	ex := execFor(t, root)
	res, err := call(ReadTool{}, ex, map[string]any{"file_path": "empty.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "空") {
		t.Fatalf("空文件应有提示: %q", res.Content)
	}
}

// ---------- 观察策略：三态 ----------

// 没见过 + 文件已存在 → write 必须拒（EEXIST → FS_NOT_OBSERVED），且**内容不变**。
func TestWriteUnobservedExistingIsRefused(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "ORIGINAL")
	ex := execFor(t, root)

	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "a.txt", "content": "OVERWRITTEN"})
	mustCode(t, err, CodeNotObserved)
	if got := readFile(t, p); got != "ORIGINAL" {
		t.Fatalf("没读过的文件被覆盖了: %q", got)
	}

	// 正向对照：先 read 再 write，覆盖成功
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(WriteTool{}, ex, map[string]any{"file_path": "a.txt", "content": "OVERWRITTEN"}); err != nil {
		t.Fatalf("读过之后应当可写: %v", err)
	}
	if got := readFile(t, p); got != "OVERWRITTEN" {
		t.Fatalf("覆盖失败: %q", got)
	}
}

// 没见过 → write 可以**创建**新文件（createIfAbsent）。
func TestWriteNewFileWithoutReadIsAllowed(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	res, err := call(WriteTool{}, ex, map[string]any{"file_path": "new.txt", "content": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Content, "Created") {
		t.Fatalf("应当报告 Created: %q", res.Content)
	}
	if got := readFile(t, filepath.Join(root, "new.txt")); got != "hello" {
		t.Fatalf("内容不对: %q", got)
	}
}

// 嵌套路径：父目录不存在时 write 应当按需创建，否则模型写 notes/a.txt 会直接 IO 失败。
func TestWriteCreatesMissingParentDirs(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	res, err := call(WriteTool{}, ex, map[string]any{"file_path": "notes/deep/a.txt", "content": "nested"})
	if err != nil {
		t.Fatalf("应当自动创建父目录: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Created") {
		t.Fatalf("应当报告 Created: %q", res.Content)
	}
	if got := readFile(t, filepath.Join(root, "notes", "deep", "a.txt")); got != "nested" {
		t.Fatalf("内容不对: %q", got)
	}
}

// 负向对照：补父目录不能变成越界通道——`..` 必须在 Resolve 阶段就被拒。
func TestWriteParentDirEscapeStillDenied(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "../escaped.txt", "content": "x"})
	mustCode(t, err, CodeSandboxDenied)
}

func TestWriteAllowsEmptyContent(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	if _, err := call(WriteTool{}, ex, map[string]any{"file_path": "e.txt", "content": ""}); err != nil {
		t.Fatalf("空内容应当允许: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "e.txt")); got != "" {
		t.Fatalf("应为空文件: %q", got)
	}
}

func TestWriteMissingContentIsRejected(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "x.txt"})
	mustCode(t, err, CodeInvalidArgs)
}

// 没见过 + edit → FS_NOT_OBSERVED（不是 FS_NOT_FOUND）。
func TestEditUnobservedIsNotObserved(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "hello world")
	ex := execFor(t, root)

	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "bye"})
	mustCode(t, err, CodeNotObserved)
	if !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("文案应照抄 DSH: %q", err.Error())
	}

	// 正向对照：read 之后同样的 edit 成功
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "bye"}); err != nil {
		t.Fatalf("读过之后应当可编辑: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a.txt")); got != "bye world" {
		t.Fatalf("编辑结果不对: %q", got)
	}
}

// 确认不存在（read 失败记的 absent）→ edit 报 FS_NOT_FOUND，write 可创建。
func TestReadMissingRecordsAbsent(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)

	_, err := call(ReadTool{}, ex, map[string]any{"file_path": "gone.txt"})
	mustCode(t, err, CodeNotFound)

	// edit：absent → FS_NOT_FOUND（**不是** FS_NOT_OBSERVED）
	_, err = call(EditTool{}, ex, map[string]any{"file_path": "gone.txt", "old_string": "a", "new_string": "b"})
	mustCode(t, err, CodeNotFound)

	// write：absent → 可创建
	if _, err := call(WriteTool{}, ex, map[string]any{"file_path": "gone.txt", "content": "now exists"}); err != nil {
		t.Fatalf("确认不存在后应当可创建: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "gone.txt")); got != "now exists" {
		t.Fatalf("创建失败: %q", got)
	}
}

// ---------- 版本守卫 ----------

func TestEditAfterExternalChangeIsStale(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "hello world")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	// 外部改动（old_string 仍然能匹配上，所以不是「找不到匹配」）
	mustWriteFile(t, p, "hello world EXTERNAL")

	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "bye"})
	mustCode(t, err, CodeStaleVersion)
	if !strings.Contains(err.Error(), "re-read the file, then retry") {
		t.Fatalf("文案应带 re-read 提示: %q", err.Error())
	}
	if got := readFile(t, p); got != "hello world EXTERNAL" {
		t.Fatalf("陈旧编辑改动了文件: %q", got)
	}

	// 正向对照：重新 read 之后同一编辑成功
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "bye"}); err != nil {
		t.Fatalf("重读之后应当可编辑: %v", err)
	}
}

// **关键负向对照**：版本守卫必须在字面量匹配之前。
// 外部改动让 old_string 不再存在时，edit 仍必须报 FS_STALE_VERSION，绝不能报 FS_NO_MATCH。
func TestStaleGuardPrecedesLiteralMatch(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "hello world")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	// 外部把内容整体换掉：old_string="hello" 已经不存在了
	mustWriteFile(t, p, "completely different")

	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "bye"})
	if err == nil {
		t.Fatal("应当失败")
	}
	if got := CodeOf(err); got != CodeStaleVersion {
		t.Fatalf("必须报 FS_STALE_VERSION（不能是 %s），实际: %v", got, err)
	}
}

func TestWriteAfterExternalChangeIsStale(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "v1")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, p, "v2-external")
	_, err := call(WriteTool{}, ex, map[string]any{"file_path": "a.txt", "content": "v3"})
	mustCode(t, err, CodeStaleVersion)
	if got := readFile(t, p); got != "v2-external" {
		t.Fatalf("陈旧写入改动了文件: %q", got)
	}
}

// ---------- 原子性 / 并发 ----------

// 并发两个 edit 同一文件：一个赢，另一个必须 FS_STALE_VERSION（不能双双成功）。
func TestConcurrentEditsOneWins(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "hello world")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = call(EditTool{}, ex, map[string]any{
				"file_path": "a.txt", "old_string": "hello", "new_string": fmt.Sprintf("X%d", i),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	wins, stale := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case CodeOf(err) == CodeStaleVersion:
			stale++
		default:
			t.Fatalf("第 %d 个 edit 出现意外错误: %v", i, err)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("期望 1 成功 1 陈旧，实际 wins=%d stale=%d（errs=%v）", wins, stale, errs)
	}

	// 文件里只能有一个赢家的结果，不能是两次替换叠加
	got := readFile(t, p)
	if got != "X0 world" && got != "X1 world" {
		t.Fatalf("文件内容被写坏: %q", got)
	}

	// 负向对照：重新 read 之后，输的那次编辑（用新内容）可以成功——
	// 证明刚才的失败来自「版本陈旧」，不是编辑路径坏了。
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "world", "new_string": "earth"}); err != nil {
		t.Fatalf("重读之后应当可编辑: %v", err)
	}
	if got := readFile(t, p); !strings.HasSuffix(got, " earth") {
		t.Fatalf("第二次编辑没生效: %q", got)
	}
}

// ---------- edit 校验 ----------

func TestEditValidation(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "hello")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "", "new_string": "x"})
	mustCode(t, err, CodeInvalidArgs)
	_, err = call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "hello", "new_string": "hello"})
	mustCode(t, err, CodeInvalidArgs)
}

func TestEditAmbiguousAndReplaceAll(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "x\nx\n")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "x", "new_string": "y"})
	mustCode(t, err, CodeAmbiguous)

	res, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "2 处") {
		t.Fatalf("应报告替换 2 处: %q", res.Content)
	}
	if got := readFile(t, p); got != "y\ny\n" {
		t.Fatalf("replace_all 结果不对: %q", got)
	}
}

func TestEditNoMatch(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "hello")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(EditTool{}, ex, map[string]any{"file_path": "a.txt", "old_string": "nope", "new_string": "y"})
	mustCode(t, err, CodeNoMatch)
}

// ---------- read_image ----------

func TestReadImageReturnsBytes(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if err := os.WriteFile(filepath.Join(root, "p.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ex := execFor(t, root)

	res, err := call(ReadImageTool{}, ex, map[string]any{"file_path": "p.png"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image == nil {
		t.Fatal("Result.Image 必须非空（循环要靠它回灌图片）")
	}
	if res.Image.MIME != "image/png" {
		t.Fatalf("MIME 不对: %q", res.Image.MIME)
	}
	if !bytes.Equal(res.Image.Data, raw) {
		t.Fatalf("图片字节不一致: got %d bytes, want %d", len(res.Image.Data), len(raw))
	}
	if !strings.Contains(res.Content, "已读取图片") {
		t.Fatalf("应有文本确认: %q", res.Content)
	}
}

func TestReadImageRejectsNonImage(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "t.txt"), "not an image")
	ex := execFor(t, root)
	_, err := call(ReadImageTool{}, ex, map[string]any{"file_path": "t.txt"})
	mustCode(t, err, CodeInvalidArgs)
}

// ---------- 注册表 / schema ----------

func TestToolsNamesAndSchema(t *testing.T) {
	tools := Tools()
	reg := agent.NewRegistry(tools...)
	want := []string{"read", "write", "edit", "read_image"}
	names := reg.Names()
	if len(names) != len(want) {
		t.Fatalf("工具数不对: %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("工具顺序/名字不对: got %v want %v", names, want)
		}
	}
	for _, tl := range tools {
		p := tl.Parameters()
		if p["type"] != "object" {
			t.Fatalf("%s 的 Parameters 必须是 JSON Schema object", tl.Name())
		}
		if tl.Description() == "" {
			t.Fatalf("%s 缺少 description", tl.Name())
		}
	}
	if defs := reg.Defs(); len(defs) != 4 {
		t.Fatalf("Defs 数量不对: %d", len(defs))
	}
}

// ---------- 版本令牌 ----------

func TestVersionTokenChangesOnExternalWrite(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "one")
	fi1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	v1 := versionOf(fi1)
	if strings.Count(v1, ":") != 4 {
		t.Fatalf("版本令牌格式应为 dev:ino:size:mtimeNs:ctimeNs，实际 %q", v1)
	}
	mustWriteFile(t, p, "two-longer")
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if v2 := versionOf(fi2); v2 == v1 {
		t.Fatalf("外部写入后版本令牌必须变化: %q", v1)
	}
}

// ---------- list ----------

func TestListRootAndSubdir(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), "B")
	ex := execFor(t, root)

	res, err := call(ListTool{}, ex, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "a.txt") || !strings.Contains(res.Content, "sub/") {
		t.Fatalf("根目录列表应含 a.txt 与 sub/: %q", res.Content)
	}

	res, err = call(ListTool{}, ex, map[string]any{"dir_path": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "b.txt") {
		t.Fatalf("子目录列表应含 b.txt: %q", res.Content)
	}
}

func TestListSecondaryWorkspaceAlias(t *testing.T) {
	primary, secondary := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(primary, "primary.txt"), "one")
	mustWriteFile(t, filepath.Join(secondary, "secondary.txt"), "two")
	ex := execFor(t, primary)
	ex.Roots = []string{secondary}

	res, err := call(ListTool{}, ex, map[string]any{"dir_path": "@workspace2/"})
	if err != nil {
		t.Fatalf("@workspace2/ should list the second active directory: %v", err)
	}
	if !strings.Contains(res.Content, "secondary.txt") || strings.Contains(res.Content, "primary.txt") {
		t.Fatalf("secondary workspace listing is incorrect: %q", res.Content)
	}
}

func TestListRejectsFileAndEscape(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	ex := execFor(t, root)

	_, err := call(ListTool{}, ex, map[string]any{"dir_path": "a.txt"})
	mustCode(t, err, CodeInvalidArgs)

	_, err = call(ListTool{}, ex, map[string]any{"dir_path": ".."})
	mustCode(t, err, CodeSandboxDenied)
}

// ---------- delete ----------

// 删除是不可逆的：没见过（没 read 过）就不许删。负向对照必须验证文件还在。
func TestDeleteRequiresRead(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	mustWriteFile(t, p, "A")
	ex := execFor(t, root)

	_, err := call(DeleteTool{}, ex, map[string]any{"file_path": "a.txt"})
	mustCode(t, err, CodeNotObserved)
	if _, serr := os.Stat(p); serr != nil {
		t.Fatalf("没读过的文件被删掉了: %v", serr)
	}

	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := call(DeleteTool{}, ex, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatalf("读过之后应当可删: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Deleted") {
		t.Fatalf("应当报告 Deleted: %q", res.Content)
	}
	if _, serr := os.Stat(p); !os.IsNotExist(serr) {
		t.Fatalf("文件应当已被删除, stat err=%v", serr)
	}
}

func TestDeleteRejectsDirectoryAndEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := execFor(t, root)

	_, err := call(DeleteTool{}, ex, map[string]any{"file_path": "d"})
	mustCode(t, err, CodeSandboxDenied)
	if _, serr := os.Stat(filepath.Join(root, "d")); serr != nil {
		t.Fatalf("目录不应被删除: %v", serr)
	}

	_, err = call(DeleteTool{}, ex, map[string]any{"file_path": "../escaped.txt"})
	mustCode(t, err, CodeSandboxDenied)
}

func TestDeleteMissingFileIsNotFound(t *testing.T) {
	root := t.TempDir()
	ex := execFor(t, root)
	_, err := call(DeleteTool{}, ex, map[string]any{"file_path": "nope.txt"})
	mustCode(t, err, CodeNotFound)
}

// ---------- move ----------

func TestMoveRenamesFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "CONTENT")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}

	res, err := call(MoveTool{}, ex, map[string]any{"from_path": "a.txt", "to_path": "renamed.txt"})
	if err != nil {
		t.Fatalf("重命名应当成功: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Moved") {
		t.Fatalf("应当报告 Moved: %q", res.Content)
	}
	if _, serr := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(serr) {
		t.Fatalf("旧路径应当不存在, stat err=%v", serr)
	}
	if got := readFile(t, filepath.Join(root, "renamed.txt")); got != "CONTENT" {
		t.Fatalf("内容应当随文件移动: %q", got)
	}
}

// 重命名到已有文件名属于不可逆操作：一律拒绝，且两个文件都不能被改动。
func TestMoveRefusesExistingTarget(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	mustWriteFile(t, filepath.Join(root, "b.txt"), "B")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}

	_, err := call(MoveTool{}, ex, map[string]any{"from_path": "a.txt", "to_path": "b.txt"})
	mustCode(t, err, CodeInvalidArgs)
	if got := readFile(t, filepath.Join(root, "a.txt")); got != "A" {
		t.Fatalf("源文件不应被改动: %q", got)
	}
	if got := readFile(t, filepath.Join(root, "b.txt")); got != "B" {
		t.Fatalf("目标文件不应被覆盖: %q", got)
	}
}

func TestMoveRequiresReadAndCreatesParentDirs(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	ex := execFor(t, root)

	_, err := call(MoveTool{}, ex, map[string]any{"from_path": "a.txt", "to_path": "notes/a.txt"})
	mustCode(t, err, CodeNotObserved)

	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(MoveTool{}, ex, map[string]any{"from_path": "a.txt", "to_path": "notes/a.txt"}); err != nil {
		t.Fatalf("目标父目录应自动创建: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "notes", "a.txt")); got != "A" {
		t.Fatalf("移动后内容不对: %q", got)
	}
}

func TestMoveOutsideRootDenied(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	ex := execFor(t, root)
	if _, err := call(ReadTool{}, ex, map[string]any{"file_path": "a.txt"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(MoveTool{}, ex, map[string]any{"from_path": "a.txt", "to_path": "../escaped.txt"})
	mustCode(t, err, CodeSandboxDenied)
}

// ---------- 新增工具的注册表 ----------

func TestToolsWithMutationNamesAndSchema(t *testing.T) {
	tools := ToolsWithMutation()
	reg := agent.NewRegistry(tools...)
	want := []string{"read", "list", "write", "edit", "move", "delete", "read_image", "office_edit"}
	names := reg.Names()
	if len(names) != len(want) {
		t.Fatalf("工具数不对: %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("工具顺序/名字不对: got %v want %v", names, want)
		}
	}
	if defs := reg.Defs(); len(defs) != len(want) {
		t.Fatalf("Defs 数量不对: %d", len(defs))
	}
}
