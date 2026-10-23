package toolsys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"blog-my/internal/agent"
)

// longContent 生成约 60KB 的假工具结果（600 行 × 101 字节）。
func longContent() string {
	var builder strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&builder, "line-%04d %s\n", i, strings.Repeat("x", 90))
	}
	return builder.String()
}

// spilledPath 从结果里取落盘路径。
func spilledPath(t *testing.T, res agent.Result) string {
	t.Helper()
	if res.Display == nil {
		t.Fatalf("结果缺少 Display：%+v", res)
	}
	path, ok := res.Display["spill_path"].(string)
	if !ok || path == "" {
		t.Fatalf("结果缺少 spill_path：%+v", res.Display)
	}
	return path
}

// TestSpillOverLimitWritesFile 对应契约 §7.5：
// 60KB 假结果 → Content 只含头尾预览 + 路径，原件完整无损落盘。
func TestSpillOverLimitWritesFile(t *testing.T) {
	content := longContent()
	if len(content) <= 32*1024 {
		t.Fatalf("测试内容 %d 字节不够触发默认 32KB 阈值", len(content))
	}
	dir := t.TempDir()
	inner := &fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: content, Display: map[string]any{"summary": "大结果"}}, nil
	}}
	res := runTool(t, SpillPolicy{Dir: dir}.Wrap(inner), nil)

	if res.IsError {
		t.Fatalf("落盘不应让调用失败：%+v", res)
	}
	if len(res.Content) >= len(content) {
		t.Fatalf("回传内容 %d 字节不应包含全文 %d 字节", len(res.Content), len(content))
	}
	if len(res.Content) > 4096 {
		t.Fatalf("回传内容应只有预览+提示（%d 字节）", len(res.Content))
	}
	runes := []rune(content)
	headN, tailN := defaultSpillHeadRunes*60/100, defaultSpillHeadRunes-defaultSpillHeadRunes*60/100
	if headN != 1200 || tailN != 800 {
		t.Fatalf("默认预算应按 60/40 分成 1200/800，实际 %d/%d", headN, tailN)
	}
	head := string(runes[:headN])
	tail := string(runes[len(runes)-tailN:])
	noticeAt := strings.Index(res.Content, "【结果已落盘】")
	if noticeAt < 0 {
		t.Fatal("回传内容应包含落盘提示")
	}
	preview := res.Content[:noticeAt]
	if !strings.HasPrefix(preview, head) {
		t.Fatal("回传预览应以原结果前 60% 预算（1200 rune）开头")
	}
	if !strings.HasSuffix(preview, tail+"\n\n") {
		t.Fatal("落盘提示之前应是原结果尾部约 40% 预算（800 rune）")
	}
	omitted := len(runes) - headN - tailN
	if !strings.Contains(preview, fmt.Sprintf("…【中间省略 %d 个字符】…", omitted)) {
		t.Fatalf("头尾之间应标明省略的字符数，预览=%s", preview[len(head):len(head)+80])
	}
	mid := string(runes[len(runes)/2 : len(runes)/2+24])
	if strings.Contains(preview, mid) {
		t.Fatal("预览不应包含被省略的中段")
	}
	if !strings.Contains(res.Content, "结果已落盘") || !strings.Contains(res.Content, "read_tool_result") {
		t.Fatalf("回传内容应说明落盘与继续读方法：%s", res.Content)
	}
	if res.Display["spilled"] != true {
		t.Fatalf("Display 应标记 spilled=true：%+v", res.Display)
	}

	path := spilledPath(t, res)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件读不出来：%v", err)
	}
	if string(data) != content {
		t.Fatal("落盘内容必须与原结果逐字节一致（无损）")
	}
	if !strings.Contains(string(data), mid) {
		t.Fatal("落盘文件必须保留预览里省略的中段")
	}
	if base := filepath.Base(path); !regexp.MustCompile(`^test-session-[0-9a-f]{16}-[0-9a-f]{16}\.txt$`).MatchString(base) {
		t.Fatalf("文件名应含 session 与内容哈希，实际 %q", base)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 != 0 {
			t.Fatalf("落盘文件不可执行，实际权限 %v", info.Mode().Perm())
		}
		if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("落盘目录应为 0700，实际 %v", info.Mode().Perm())
		}
	}
}

// TestSpillDedupeUsesFullContentNotPreview 去重哈希覆盖全文：
// 头尾相同、中间不同的两份结果预览一致，但必须落成两个文件，且文件名哈希等于全文摘要。
func TestSpillDedupeUsesFullContentNotPreview(t *testing.T) {
	const budget = 20
	headN, tailN := budget*60/100, budget-budget*60/100
	head := strings.Repeat("头", headN)
	tail := strings.Repeat("尾", tailN)
	contentA := head + strings.Repeat("甲", 30) + tail
	contentB := head + strings.Repeat("乙", 30) + tail
	dir := t.TempDir()
	policy := SpillPolicy{Dir: dir, MaxBytes: 10, HeadRunes: budget}
	wrap := func(content string) agent.Tool {
		return policy.Wrap(&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: content}, nil
		}})
	}
	resA := runTool(t, wrap(contentA), nil)
	resB := runTool(t, wrap(contentB), nil)
	previewA := strings.SplitN(resA.Content, "【结果已落盘】", 2)[0]
	previewB := strings.SplitN(resB.Content, "【结果已落盘】", 2)[0]
	if previewA != previewB {
		t.Fatalf("相同头尾与省略长度的预览应一致：\nA=%q\nB=%q", previewA, previewB)
	}
	if !strings.Contains(previewA, "中间省略 30 个字符") {
		t.Fatalf("省略计数应按 rune 而不是字节：%s", previewA)
	}
	if !strings.HasPrefix(previewA, head) || !strings.Contains(previewA, tail) {
		t.Fatalf("预览应同时包含头部与尾部：%s", previewA)
	}
	if strings.Contains(previewA, "甲") || strings.Contains(previewA, "乙") {
		t.Fatalf("预览不应泄漏中间差异：%s", previewA)
	}
	pathA := spilledPath(t, resA)
	pathB := spilledPath(t, resB)
	if pathA == pathB {
		t.Fatal("中间不同的全文不应因为预览相同而复用同一落盘文件")
	}
	again := spilledPath(t, runTool(t, wrap(contentA), nil))
	if again != pathA {
		t.Fatalf("同一份全文应复用已有文件：%q vs %q", again, pathA)
	}
	for path, content := range map[string]string{pathA: contentA, pathB: contentB} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content {
			t.Fatal("落盘文件必须是全文，不能只存预览")
		}
		sum := sha256.Sum256([]byte(content))
		want := hex.EncodeToString(sum[:])[:spillHashLen]
		base := strings.TrimSuffix(filepath.Base(path), ".txt")
		got := base[strings.LastIndex(base, "-")+1:]
		if got != want {
			t.Fatalf("文件名哈希应是全文 SHA-256 前缀，得到 %q，期望 %q", got, want)
		}
		previewSum := sha256.Sum256([]byte(previewA))
		previewHash := hex.EncodeToString(previewSum[:])[:spillHashLen]
		if got == previewHash {
			t.Fatal("文件名哈希不应等于预览摘要")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("两份不同全文应各落盘一次，目录里实际有 %d 个文件", len(entries))
	}
}

// TestSpillDedupesByContentHash 同一份内容重复落盘复用同一文件。
func TestSpillDedupesByContentHash(t *testing.T) {
	content := longContent()
	dir := t.TempDir()
	wrapped := SpillPolicy{Dir: dir}.Wrap(&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: content}, nil
	}})
	first := spilledPath(t, runTool(t, wrapped, nil))
	second := spilledPath(t, runTool(t, wrapped, nil))
	if first != second {
		t.Fatalf("相同内容应复用同一落盘文件：%q vs %q", first, second)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("相同内容不应重复落盘，目录里实际有 %d 个文件", len(entries))
	}
}

// TestSpillUnderLimitUnchanged 未超限结果原样返回，不落盘。
// 即便 rune 数已经超过预览预算，只要字节数未过 MaxBytes，也不能改写成头尾预览。
func TestSpillUnderLimitUnchanged(t *testing.T) {
	dir := t.TempDir()
	res := runTool(t, SpillPolicy{Dir: dir}.Wrap(&fakeTool{name: "small", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: "短内容"}, nil
	}}), nil)
	if res.Content != "短内容" {
		t.Fatalf("未超限内容不应改动，实际 %q", res.Content)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("未超限不应落盘，目录里有 %d 个文件", len(entries))
	}

	// 长于预览预算、短于落盘阈值：必须逐字节保留，不能插入省略标记。
	longButUnder := strings.Repeat("头", 30) + strings.Repeat("中", 40) + strings.Repeat("尾", 30)
	res = runTool(t, SpillPolicy{Dir: dir, MaxBytes: len(longButUnder), HeadRunes: 20}.Wrap(
		&fakeTool{name: "medium", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: longButUnder}, nil
		}}), nil)
	if res.Content != longButUnder {
		t.Fatalf("未达字节阈值的内容不应改写，实际长度 %d", len(res.Content))
	}
	if strings.Contains(res.Content, "中间省略") || strings.Contains(res.Content, "结果已落盘") {
		t.Fatal("未超限不应出现预览省略或落盘提示")
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("刚好等于阈值不应落盘，目录里有 %d 个文件", len(entries))
	}
}

// TestSpillFallsBackWhenDirUnavailable 落盘目录不可用时退化为截断，不报错。
func TestSpillFallsBackWhenDirUnavailable(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := longContent()
	res := runTool(t, SpillPolicy{Dir: filepath.Join(blocker, "sub")}.Wrap(
		&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: content}, nil
		}}), nil)
	if res.IsError {
		t.Fatalf("落盘失败不应让调用失败：%+v", res)
	}
	if !strings.Contains(res.Content, "落盘失败") {
		t.Fatalf("应说明落盘失败：%s", res.Content)
	}
	if len(res.Content) >= len(content) {
		t.Fatal("落盘失败时也应截断")
	}
	runes := []rune(content)
	headN := defaultSpillHeadRunes * 60 / 100
	tailN := defaultSpillHeadRunes - headN
	head := string(runes[:headN])
	tail := string(runes[len(runes)-tailN:])
	if !strings.HasPrefix(res.Content, head) {
		t.Fatal("落盘失败时预览仍应保留头部")
	}
	tailAt := strings.Index(res.Content, tail)
	failAt := strings.Index(res.Content, "落盘失败")
	if tailAt < 0 || failAt < 0 || tailAt > failAt {
		t.Fatal("落盘失败时预览尾部应出现在失败说明之前")
	}
}

// TestSpillWithinPreviewBudgetKeepsFullText 字节数已超限、但 rune 数仍装得进预览预算时，
// 回传全文（只追加落盘提示），不插入头尾省略标记；磁盘上仍是全文。
func TestSpillWithinPreviewBudgetKeepsFullText(t *testing.T) {
	content := strings.Repeat("全", 30)
	dir := t.TempDir()
	res := runTool(t, SpillPolicy{Dir: dir, MaxBytes: 10, HeadRunes: 100}.Wrap(
		&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: content}, nil
		}}), nil)
	if !strings.HasPrefix(res.Content, content) {
		t.Fatalf("未超过预览预算时应回传全文，实际 %q", res.Content)
	}
	if strings.Contains(res.Content, "中间省略") {
		t.Fatal("未超过预览预算不应插入省略标记")
	}
	if !strings.Contains(res.Content, "结果已落盘") {
		t.Fatal("超字节阈值仍应落盘并提示")
	}
	data, err := os.ReadFile(spilledPath(t, res))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatal("落盘文件应是全文")
	}
}

// TestSpillPreviewBudgetEdges 锁定预算切分与「装得下就不改写」。
func TestSpillPreviewBudgetEdges(t *testing.T) {
	if spillPreview("原文", 0) != "" {
		t.Fatal("非正预算应得到空预览")
	}
	exact := strings.Repeat("中", 10)
	if got := spillPreview(exact, 10); got != exact {
		t.Fatalf("刚好等于预算应原样返回，实际 %q", got)
	}
	short := "短"
	if got := spillPreview(short, 10); got != short {
		t.Fatalf("短于预算应原样返回，实际 %q", got)
	}

	cases := []struct{ budget, head, tail int }{
		{2000, 1200, 800},
		{10, 6, 4},
		{5, 3, 2},
		{2, 1, 1},
		{1, 0, 1},
	}
	for _, tc := range cases {
		body := strings.Repeat("头", tc.head) + strings.Repeat("甲", 9) + strings.Repeat("尾", max(tc.tail, 1))
		got := spillPreview(body, tc.budget)
		if !utf8.ValidString(got) {
			t.Fatalf("budget=%d 的预览不是合法 UTF-8", tc.budget)
		}
		if tc.head > 0 && !strings.HasPrefix(got, strings.Repeat("头", tc.head)) {
			t.Fatalf("budget=%d 头部应为 %d 个「头」，实际 %q", tc.budget, tc.head, got)
		}
		if tc.tail > 0 && !strings.HasSuffix(got, strings.Repeat("尾", tc.tail)) {
			t.Fatalf("budget=%d 尾部应为 %d 个「尾」，实际 %q", tc.budget, tc.tail, got)
		}
		if strings.Contains(got, "甲") {
			t.Fatalf("budget=%d 不应包含中间：%q", tc.budget, got)
		}
		if !strings.Contains(got, "中间省略 9 个字符") {
			t.Fatalf("budget=%d 应标明省略 9 个字符：%q", tc.budget, got)
		}
	}
}

// TestReadToolResultReadsLaterPart 对应契约 §7.5：read_tool_result 能读到头部之后的内容。
func TestReadToolResultReadsLaterPart(t *testing.T) {
	content := longContent()
	dir := t.TempDir()
	path := spilledPath(t, runTool(t, SpillPolicy{Dir: dir}.Wrap(
		&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: content}, nil
		}}), nil))
	reader := ReadResultTool(SpillPolicy{Dir: dir})

	res := runTool(t, reader, map[string]any{"file_path": path, "offset": 500, "limit": 10})
	if res.IsError {
		t.Fatalf("读取后段失败：%+v", res)
	}
	if !strings.Contains(res.Content, "line-0499") || !strings.Contains(res.Content, "line-0508") {
		t.Fatalf("offset=500 limit=10 应读到 line-0499..line-0508：%s", res.Content)
	}
	if strings.Contains(res.Content, "line-0000") {
		t.Fatal("读取后段不应包含文件开头")
	}
	if res.Display["next_offset"] != 510 {
		t.Fatalf("next_offset=%v，期望 510", res.Display["next_offset"])
	}

	head := runTool(t, reader, map[string]any{"file_path": path, "offset": 1, "limit": 5})
	if !strings.Contains(head.Content, "line-0000") || strings.Contains(head.Content, "line-0050") {
		t.Fatalf("offset=1 limit=5 只应读到开头 5 行：%s", head.Content)
	}

	// offset 超出末行：不当作失败，给可修复提示。
	out := runTool(t, reader, map[string]any{"file_path": path, "offset": 99999})
	if out.IsError {
		t.Fatalf("offset 越界不应判失败：%+v", out)
	}
	if out.Display["code"] != "SPILL_OFFSET_OUT_OF_RANGE" {
		t.Fatalf("offset 越界 code=%v", out.Display["code"])
	}
}

// TestReadToolResultRejectsOutsidePath 只能读落盘目录内的文件，不能当任意读文件后门。
func TestReadToolResultRejectsOutsidePath(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("机密"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, ReadResultTool(SpillPolicy{Dir: dir}), map[string]any{"file_path": outside})
	if !res.IsError || displayCode(res) != "SPILL_PATH_DENIED" {
		t.Fatalf("越界路径应被拒绝：%+v", res)
	}
	if !strings.Contains(res.Content, "只能读取落盘结果目录") {
		t.Fatalf("拒绝文案应说明范围：%s", res.Content)
	}
	// 相对路径的 ../ 逃逸同样拒绝。
	res = runTool(t, ReadResultTool(SpillPolicy{Dir: dir}), map[string]any{"file_path": "../secret.txt"})
	if !res.IsError || displayCode(res) != "SPILL_PATH_DENIED" {
		t.Fatalf("相对路径逃逸应被拒绝：%+v", res)
	}
}

// TestReadToolResultMissingFile 目录内不存在的文件给出 SPILL_NOT_FOUND。
func TestReadToolResultMissingFile(t *testing.T) {
	dir := t.TempDir()
	res := runTool(t, ReadResultTool(SpillPolicy{Dir: dir}),
		map[string]any{"file_path": filepath.Join(dir, "nope.txt")})
	if !res.IsError || displayCode(res) != "SPILL_NOT_FOUND" {
		t.Fatalf("不存在的结果应报 SPILL_NOT_FOUND：%+v", res)
	}
}

// TestReadToolResultMissingArg 缺 file_path 要可修复。
func TestReadToolResultMissingArg(t *testing.T) {
	res := runTool(t, ReadResultTool(SpillPolicy{Dir: t.TempDir()}), map[string]any{})
	if !res.IsError || displayCode(res) != "SPILL_PATH_MISSING" {
		t.Fatalf("缺 file_path 应报 SPILL_PATH_MISSING：%+v", res)
	}
}

// TestSpillPolicyDefaults 默认值：32KB / 2000 rune / 用户配置目录/blog-my/spill。
func TestSpillPolicyDefaults(t *testing.T) {
	p := SpillPolicy{}.normalized()
	if p.MaxBytes != 32*1024 || p.HeadRunes != 2000 {
		t.Fatalf("默认策略=%+v，期望 32KB/2000", p)
	}
	dir := defaultSpillDir()
	if !strings.Contains(dir, filepath.Join("blog-my", "spill")) {
		t.Fatalf("默认落盘目录=%q，期望包含 blog-my/spill", dir)
	}
}

// TestSpillSessionNameSanitized session id 里的非法字符要替换，不能形成路径穿越。
func TestSpillSessionNameSanitized(t *testing.T) {
	content := longContent()
	dir := t.TempDir()
	wrapped := SpillPolicy{Dir: dir}.Wrap(&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: content}, nil
	}})
	res, err := wrapped.Execute(context.Background(), nil, &agent.Exec{SessionID: "../a/b:c d"})
	if err != nil {
		t.Fatal(err)
	}
	path := spilledPath(t, res)
	if !strings.HasPrefix(filepath.Base(path), "a_b_c_d-") {
		t.Fatalf("session 应被清洗后进文件名，实际 %q", filepath.Base(path))
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("落盘文件必须留在策略目录内：%q", path)
	}
}

// TestSpillNilExec session 为空时使用稳定占位名。
func TestSpillNilExec(t *testing.T) {
	content := longContent()
	dir := t.TempDir()
	wrapped := SpillPolicy{Dir: dir}.Wrap(&fakeTool{name: "big", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{Content: content}, nil
	}})
	res, err := wrapped.Execute(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(spilledPath(t, res)); !strings.HasPrefix(base, "nosession-") {
		t.Fatalf("空 session 应使用 nosession 前缀，实际 %q", base)
	}
}
