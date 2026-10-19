package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSearcher 是 MemorySearcher 的确定性替身：既可返回命中，也可返回错误。
// 有了它，"检索失败"这条分支不需要真的把数据库弄坏就能测。
type fakeSearcher struct {
	hits []Hit
	err  error

	calls    int
	gotUser  string
	gotQuery string
	gotTopK  int
}

func (f *fakeSearcher) Search(_ context.Context, userID, query string, topK int) ([]Hit, error) {
	f.calls++
	f.gotUser, f.gotQuery, f.gotTopK = userID, query, topK
	return f.hits, f.err
}

type fakeWriter struct {
	res Result
	err error

	calls    int
	gotUser  string
	gotTitle string
	gotText  string
}

func (f *fakeWriter) IngestText(_ context.Context, userID, title, text string) (Result, error) {
	f.calls++
	f.gotUser, f.gotTitle, f.gotText = userID, title, text
	return f.res, f.err
}

func sampleHits() []Hit {
	return []Hit{
		{ChunkID: 1, DocumentID: 7, Seq: 0, Kind: "text", Title: "偏好", Text: "用户喜欢喝美式咖啡", Score: 0.812},
		{ChunkID: 2, DocumentID: 8, Seq: 0, Kind: "image_caption", Title: "截图", Text: "【描述】一张报错截图", Score: 0.556},
	}
}

// 空库（kb_document / kb_chunk 都是 0 行）必须不报错、不产生块。
func TestMemoryPromptEmptyStore(t *testing.T) {
	s := &fakeSearcher{hits: nil, err: nil}
	block, err := MemoryPrompt(context.Background(), s, "u1", "还记得我喜欢喝什么吗", 0)
	if err != nil {
		t.Fatalf("空库检索不应返回错误，得到 %v", err)
	}
	if block != "" {
		t.Fatalf("空库不应产生 <memory> 块，得到 %q", block)
	}
	if s.calls != 1 {
		t.Fatalf("应检索 1 次，实际 %d 次", s.calls)
	}
	if want := memoryCandidateLimit(DefaultMemoryTopK); s.gotTopK != want {
		t.Fatalf("注入检索应多取候选（%d 条），实际 %d", want, s.gotTopK)
	}
	if s.gotUser != "u1" {
		t.Fatalf("userID 未透传，实际 %q", s.gotUser)
	}
}

// <memory> 块的形状：成对标签 + 命中正文 + 工具提示。
func TestMemoryPromptShape(t *testing.T) {
	s := &fakeSearcher{hits: sampleHits()}
	block, err := MemoryPrompt(context.Background(), s, "u1", "我喜欢喝什么", 3)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.HasPrefix(block, memoryOpenTag) {
		t.Fatalf("块应以 %s 开头，实际开头 %q", memoryOpenTag, clipRunes(block, 40))
	}
	if !strings.HasSuffix(block, memoryCloseTag) {
		t.Fatalf("块应以 %s 结尾，实际结尾 %q", memoryCloseTag, block[max(0, len(block)-40):])
	}
	for _, want := range []string{"用户喜欢喝美式咖啡", "偏好", "截图", "memory_search"} {
		if !strings.Contains(block, want) {
			t.Fatalf("块里应包含 %q，实际：%s", want, block)
		}
	}
	if want := memoryCandidateLimit(3); s.gotTopK != want {
		t.Fatalf("注入检索应多取候选（%d 条），实际 %d", want, s.gotTopK)
	}
	if got := strings.Count(block, memoryOpenTag); got != 1 {
		t.Fatalf("应恰好一个 %s 开标签，实际 %d 个", memoryOpenTag, got)
	}
}

// 契约 §5 的硬要求：**检索失败不得阻断对话**。
//
// 这条测试同时是负向对照的目标：把 MemoryPrompt 里的降级去掉（失败即 return err），
// 它必须转红 —— 见本文件末的说明与 docs 里的记录。
func TestMemoryPromptDegradesOnSearchError(t *testing.T) {
	boom := errors.New("模拟数据库故障：connection refused")
	s := &fakeSearcher{err: boom}

	// 先证明失败是真的：严格版必须如实报错，且检索器确实被调用了。
	if _, err := MemoryBlock(context.Background(), s, "u1", "任意问题", 5); err == nil {
		t.Fatal("MemoryBlock 在检索失败时应返回错误（否则本测试失去意义）")
	}
	if s.calls == 0 {
		t.Fatal("检索器根本没被调用，本测试失去意义")
	}

	// 再验降级：自动注入这条路绝不能把错误抛给对话。
	block, err := MemoryPrompt(context.Background(), s, "u1", "任意问题", 5)
	if err != nil {
		t.Fatalf("检索失败必须降级为「无记忆」，不能返回错误；得到 %v", err)
	}
	if block != "" {
		t.Fatalf("检索失败时不应产生记忆块，得到 %q", block)
	}
}

// 空消息不发检索请求，也不产生块。
func TestMemoryPromptBlankQuerySkipsSearch(t *testing.T) {
	s := &fakeSearcher{hits: sampleHits()}
	for _, q := range []string{"", "   ", "\n\t "} {
		block, err := MemoryPrompt(context.Background(), s, "u1", q, 5)
		if err != nil {
			t.Fatalf("空 query（%q）不应报错：%v", q, err)
		}
		if block != "" {
			t.Fatalf("空 query（%q）不应产生块，得到 %q", q, block)
		}
	}
	if s.calls != 0 {
		t.Fatalf("空 query 不应调用检索器，实际调用 %d 次", s.calls)
	}
}

// limit 收敛：<=0 用默认，>20 封顶，中间原样。
func TestClampMemoryLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, DefaultMemoryTopK}, {0, DefaultMemoryTopK}, {1, 1}, {3, 3},
		{DefaultMemoryTopK, DefaultMemoryTopK}, {MaxMemoryTopK, MaxMemoryTopK},
		{MaxMemoryTopK + 1, MaxMemoryTopK}, {1 << 20, MaxMemoryTopK},
	}
	for _, c := range cases {
		if got := ClampMemoryLimit(c.in); got != c.want {
			t.Fatalf("ClampMemoryLimit(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 检索器为 nil 是编程错误：严格版报错，注入版仍然降级。
func TestMemoryNilSearcher(t *testing.T) {
	if _, err := MemoryBlock(context.Background(), nil, "u1", "问题", 5); err == nil {
		t.Fatal("MemoryBlock(nil searcher) 应报错")
	}
	block, err := MemoryPrompt(context.Background(), nil, "u1", "问题", 5)
	if err != nil || block != "" {
		t.Fatalf("MemoryPrompt(nil searcher) 应降级为（空串,nil），实际（%q,%v）", block, err)
	}
}

// memory_search 工具的业务逻辑。
func TestRunMemorySearch(t *testing.T) {
	s := &fakeSearcher{hits: sampleHits()}
	hits, err := RunMemorySearch(context.Background(), s, "u1", "  咖啡  ", 0)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应原样返回 2 条命中，实际 %d 条", len(hits))
	}
	if s.gotQuery != "咖啡" {
		t.Fatalf("query 应去除首尾空白后透传，实际 %q", s.gotQuery)
	}
	if s.gotTopK != DefaultMemoryTopK {
		t.Fatalf("limit=0 时应回落到默认 %d，实际 %d", DefaultMemoryTopK, s.gotTopK)
	}

	// 空 query 是工具的参数错误（应作为 is_error 结果回灌给模型，而不是静默成功）
	if _, err := RunMemorySearch(context.Background(), s, "u1", "   ", 5); err == nil {
		t.Fatal("空 query 应报错")
	}
	if _, err := RunMemorySearch(context.Background(), nil, "u1", "咖啡", 5); err == nil {
		t.Fatal("nil 检索器应报错")
	}

	// 检索失败如实上报（工具层要把错误变成 is_error 结果回灌，让模型自己决定下一步）
	sErr := &fakeSearcher{err: errors.New("boom")}
	if _, err := RunMemorySearch(context.Background(), sErr, "u1", "咖啡", 5); err == nil {
		t.Fatal("检索失败应返回错误给工具层")
	}
}

// memory_write 工具的业务逻辑。
func TestRunMemoryWrite(t *testing.T) {
	w := &fakeWriter{res: Result{DocumentID: 42, Kind: "text", ChunkCount: 1}}
	res, err := RunMemoryWrite(context.Background(), w, "u1", "  用户喜欢喝美式咖啡  ", []string{"偏好", "#咖啡"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if res.DocumentID != 42 {
		t.Fatalf("应原样返回入库结果，实际 id=%d", res.DocumentID)
	}
	if w.gotText != "用户喜欢喝美式咖啡" {
		t.Fatalf("text 应去除首尾空白，实际 %q", w.gotText)
	}
	if !strings.Contains(w.gotTitle, "#偏好") || !strings.Contains(w.gotTitle, "#咖啡") {
		t.Fatalf("tags 应编进标题（去重 # 前缀），实际标题 %q", w.gotTitle)
	}
	if !strings.Contains(w.gotTitle, "用户喜欢喝美式咖啡") {
		t.Fatalf("标题应包含正文首行，实际 %q", w.gotTitle)
	}

	if _, err := RunMemoryWrite(context.Background(), w, "u1", "   ", nil); err == nil {
		t.Fatal("空 text 应报错")
	}
	if _, err := RunMemoryWrite(context.Background(), nil, "u1", "有内容", nil); err == nil {
		t.Fatal("nil 写入器应报错")
	}

	wErr := &fakeWriter{err: errors.New("boom")}
	if _, err := RunMemoryWrite(context.Background(), wErr, "u1", "有内容", nil); err == nil {
		t.Fatal("写入失败应返回错误给工具层")
	}
}

// 标题：长度必须收敛在 200 字符内（title 列 255，留余量），且不切坏多字节字符。
func TestMemoryTitle(t *testing.T) {
	long := strings.Repeat("很长的一段记忆内容", 40) // 320 个字符
	title := MemoryTitle(long, []string{"tag-a", "tag-b"})
	if n := len([]rune(title)); n > memoryTitleRunes {
		t.Fatalf("标题应截断到 %d 字符以内，实际 %d", memoryTitleRunes, n)
	}
	if !strings.HasPrefix(title, "#tag-a #tag-b ") {
		t.Fatalf("标题应以 tags 开头，实际 %q", clipRunes(title, 40))
	}
	if strings.Contains(title, "\uFFFD") {
		t.Fatal("截断把 UTF-8 字符切坏了（出现替换字符）")
	}

	// 没有 tags → 只有正文首行
	if got := MemoryTitle("第一行\n第二行", nil); got != "第一行" {
		t.Fatalf("无 tags 时标题应为正文首行，实际 %q", got)
	}
	// 全是空白 tag → 等同于没有 tags
	if got := MemoryTitle("正文", []string{" ", "#", "\t"}); got != "正文" {
		t.Fatalf("空白 tag 应被忽略，实际 %q", got)
	}
}

// 没有命中时 FormatMemoryBlock 返回空串，而不是一对空标签。
func TestFormatMemoryBlockNoHits(t *testing.T) {
	if got := FormatMemoryBlock(nil, 4000); got != "" {
		t.Fatalf("无命中应返回空串，实际 %q", got)
	}
}

// token 预算：注入块要按预算贪心选取，并写清「共命中 N 条、省略 M 条」。
func TestMemoryBlockTokenBudgetAndNote(t *testing.T) {
	hits := []Hit{
		{ChunkID: 1, DocumentID: 1, Title: "一", Text: strings.Repeat("甲", 900)},
		{ChunkID: 2, DocumentID: 2, Title: "二", Text: strings.Repeat("乙", 900)},
	}
	s := &fakeSearcher{hits: hits}

	// 预算 1000：两条各约 901 token，只装得下第一条。
	block, err := MemoryBlockWithOptions(context.Background(), s, "u1", "任意问题", MemoryQueryOptions{TopK: 5, Budget: 1000})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.Contains(block, "本次共命中 2 条") || !strings.Contains(block, "省略 1 条") {
		t.Fatalf("块里应保留命中/省略说明，实际 %q", clipRunes(block, 120))
	}
	if strings.Count(block, "甲") == 0 {
		t.Fatal("应注入第一条记忆")
	}
	if strings.Count(block, "乙") > 0 {
		t.Fatal("超预算的第二条不应被注入")
	}

	// 预算连第一条都装不下：仍要保留第一条，避免注入空块。
	tiny, err := MemoryBlockWithOptions(context.Background(), s, "u1", "任意问题", MemoryQueryOptions{TopK: 5, Budget: 10})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if tiny == "" || !strings.Contains(tiny, "甲") {
		t.Fatalf("预算再小也应保留第一条，实际 %q", clipRunes(tiny, 80))
	}
}

// 预算从环境变量读：未设/非法/<=0 一律回落到默认值。
func TestMemoryTokenBudgetEnv(t *testing.T) {
	t.Setenv("AGENT_MEMORY_TOKEN_BUDGET", "")
	if got := MemoryTokenBudget(); got != DefaultMemoryTokenBudget {
		t.Fatalf("未设时应回落到默认 %d，实际 %d", DefaultMemoryTokenBudget, got)
	}
	t.Setenv("AGENT_MEMORY_TOKEN_BUDGET", "500")
	if got := MemoryTokenBudget(); got != 500 {
		t.Fatalf("显式合法值应被采用，实际 %d", got)
	}
	for _, bad := range []string{"abc", "0", "-3", "   "} {
		t.Setenv("AGENT_MEMORY_TOKEN_BUDGET", bad)
		if got := MemoryTokenBudget(); got != DefaultMemoryTokenBudget {
			t.Fatalf("非法值 %q 应回落到默认 %d，实际 %d", bad, DefaultMemoryTokenBudget, got)
		}
	}
}
