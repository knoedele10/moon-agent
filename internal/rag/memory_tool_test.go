package rag

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// fakeBackend 同时满足 MemorySearcher 与 MemoryWriter，用来构造整组记忆工具。
type fakeBackend struct {
	searcher fakeSearcher
	writer   fakeWriter
}

func (f *fakeBackend) Search(ctx context.Context, userID, query string, topK int) ([]Hit, error) {
	return f.searcher.Search(ctx, userID, query, topK)
}

func (f *fakeBackend) IngestText(ctx context.Context, userID, title, text string) (Result, error) {
	return f.writer.IngestText(ctx, userID, title, text)
}

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("构造测试参数失败：%v", err)
	}
	return b
}

// 工具定义要能被注册表接受，并暴露给模型（required/边界值都要对）。
func TestMemoryToolDefs(t *testing.T) {
	b := &fakeBackend{}
	tools := NewMemoryTools(b, "u1")
	if len(tools) != 2 {
		t.Fatalf("应返回 2 个工具，实际 %d 个", len(tools))
	}
	if tools[0].Name() != "memory_search" || tools[1].Name() != "memory_write" {
		t.Fatalf("工具名不对：%s / %s", tools[0].Name(), tools[1].Name())
	}
	for _, tool := range tools {
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("工具 %s 的 Description 不能为空（模型靠它判断何时调用）", tool.Name())
		}
	}

	// memory_search
	sp := tools[0].Parameters()
	if sp["type"] != "object" {
		t.Fatalf("Parameters 必须是 object schema，实际 %v", sp["type"])
	}
	req, ok := sp["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Fatalf("memory_search 的 required 应为 [query]，实际 %v", sp["required"])
	}
	props := sp["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Fatal("memory_search 缺少 query 参数定义")
	}
	lim := props["limit"].(map[string]any)
	if lim["maximum"] != MaxMemoryTopK {
		t.Fatalf("limit 上界应是 %d，实际 %v", MaxMemoryTopK, lim["maximum"])
	}

	// memory_write
	wp := tools[1].Parameters()
	wreq, ok := wp["required"].([]string)
	if !ok || len(wreq) != 1 || wreq[0] != "text" {
		t.Fatalf("memory_write 的 required 应为 [text]，实际 %v", wp["required"])
	}

	// 能被 agent.Registry 接受 + Defs 输出（这就是 web 接线层要做的事）
	reg := agent.NewRegistry(tools...)
	if reg.Len() != 2 {
		t.Fatalf("注册后应有 2 个工具，实际 %d", reg.Len())
	}
	if _, ok := reg.Lookup("memory_search"); !ok {
		t.Fatal("注册表里查不到 memory_search")
	}
	if _, ok := reg.Lookup("memory_write"); !ok {
		t.Fatal("注册表里查不到 memory_write")
	}
	defs := reg.Defs()
	if len(defs) != 2 {
		t.Fatalf("Defs 应返回 2 条，实际 %d 条", len(defs))
	}
	for _, d := range defs {
		if d.Name == "" || d.Parameters == nil {
			t.Fatalf("Defs 条目不完整：%+v", d)
		}
	}
}

// memory_search：命中时把正文给模型，Display 里给结构化摘要。
func TestMemorySearchExecuteOK(t *testing.T) {
	s := &fakeSearcher{hits: sampleHits()}
	tool := NewMemorySearchTool(s, "u1")
	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"query": "咖啡", "limit": 2}), nil)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if res.IsError {
		t.Fatalf("成功调用不应是 is_error：%s", res.Content)
	}
	if !strings.Contains(res.Content, "用户喜欢喝美式咖啡") {
		t.Fatalf("Content 应包含命中正文，实际 %q", res.Content)
	}
	if res.Display["count"] != 2 {
		t.Fatalf("Display.count 应为 2，实际 %v", res.Display["count"])
	}
	if s.gotTopK != 2 || s.gotUser != "u1" {
		t.Fatalf("参数未正确透传：topK=%d user=%q", s.gotTopK, s.gotUser)
	}
	hits, ok := res.Display["hits"].([]map[string]any)
	if !ok || len(hits) != 2 {
		t.Fatalf("Display.hits 应是 2 条摘要，实际 %v", res.Display["hits"])
	}
	if hits[0]["preview"] == "" {
		t.Fatal("摘要里应带 preview")
	}
}

// memory_search：没命中不是错误（模型应得到"想不起来"这个事实）。
func TestMemorySearchExecuteNoHits(t *testing.T) {
	tool := NewMemorySearchTool(&fakeSearcher{}, "u1")
	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"query": "从没聊过的话题"}), nil)
	if err != nil {
		t.Fatalf("没命中不应报错：%v", err)
	}
	if res.IsError {
		t.Fatal("没命中不是 is_error")
	}
	if !strings.Contains(res.Content, "没有找到") {
		t.Fatalf("Content 应说明没找到，实际 %q", res.Content)
	}
	if res.Display["count"] != 0 {
		t.Fatalf("Display.count 应为 0，实际 %v", res.Display["count"])
	}
}

// 参数是原始 JSON，由工具自己校验（契约 §3）。
func TestMemorySearchExecuteBadArgs(t *testing.T) {
	tool := NewMemorySearchTool(&fakeSearcher{}, "u1")
	cases := []struct {
		name string
		args string
	}{
		{"非法 JSON", "{not json"},
		{"空参数", ""},
		{"缺 query", `{"limit":3}`},
		{"query 全空白", `{"query":"   "}`},
	}
	for _, c := range cases {
		res, err := tool.Execute(context.Background(), json.RawMessage(c.args), nil)
		if err == nil {
			t.Fatalf("%s：应报错让模型改参数重试", c.name)
		}
		if res.Display["code"] == nil {
			t.Fatalf("%s：失败时应带上机器可路由的 code，实际 %v", c.name, res.Display)
		}
	}
}

// limit 省略 → 默认；超界 → 封顶。
func TestMemorySearchExecuteClampsLimit(t *testing.T) {
	s := &fakeSearcher{}
	tool := NewMemorySearchTool(s, "u1")

	if _, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"query": "x"}), nil); err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if s.gotTopK != DefaultMemoryTopK {
		t.Fatalf("省略 limit 应回落默认 %d，实际 %d", DefaultMemoryTopK, s.gotTopK)
	}
	if _, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"query": "x", "limit": 9999}), nil); err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if s.gotTopK != MaxMemoryTopK {
		t.Fatalf("limit 超界应封顶到 %d，实际 %d", MaxMemoryTopK, s.gotTopK)
	}
}

// 检索失败 → 返回 error（循环会折成 is_error 回灌，对话继续），而不是 panic 或静默成功。
func TestMemorySearchExecuteSearchFailure(t *testing.T) {
	tool := NewMemorySearchTool(&fakeSearcher{err: errors.New("模拟 DB 故障")}, "u1")
	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"query": "x"}), nil)
	if err == nil {
		t.Fatal("检索失败应返回 error，交给循环折成 is_error 结果")
	}
	if !strings.Contains(err.Error(), "记忆检索失败") {
		t.Fatalf("错误文案应说明是记忆检索失败，实际 %v", err)
	}
	if res.Display["code"] != "MEMORY_SEARCH_FAILED" {
		t.Fatalf("应带 MEMORY_SEARCH_FAILED，实际 %v", res.Display)
	}
}

// memory_write：写成功后标题带 tags，Display 回传 id。
func TestMemoryWriteExecuteOK(t *testing.T) {
	w := &fakeWriter{res: Result{DocumentID: 42, Kind: "text", Title: "#偏好 #饮食 用户不吃香菜", ChunkCount: 1}}
	tool := NewMemoryWriteTool(w, "u1")
	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{
		"text": "用户不吃香菜",
		"tags": []string{"偏好", "饮食"},
	}), nil)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.Contains(res.Content, "已记住") || !strings.Contains(res.Content, "42") {
		t.Fatalf("Content 应确认已记住并带 id，实际 %q", res.Content)
	}
	if res.Display["id"] != uint(42) {
		t.Fatalf("Display.id 应为 42，实际 %v", res.Display["id"])
	}
	if !strings.Contains(w.gotTitle, "#偏好") {
		t.Fatalf("tags 应进入标题，实际 %q", w.gotTitle)
	}
}

// 重复写入（内容 sha256 命中）→ 明确告诉模型"已经存在"，而不是假装又记了一遍。
func TestMemoryWriteExecuteDuplicated(t *testing.T) {
	w := &fakeWriter{res: Result{DocumentID: 7, Kind: "text", Title: "旧记忆", ChunkCount: 1, Duplicated: true}}
	tool := NewMemoryWriteTool(w, "u1")
	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{"text": "旧记忆"}), nil)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !strings.Contains(res.Content, "已经存在") {
		t.Fatalf("Content 应说明已存在，实际 %q", res.Content)
	}
	if res.Display["duplicated"] != true {
		t.Fatalf("Display.duplicated 应为 true，实际 %v", res.Display["duplicated"])
	}
}

// memory_write 的参数校验与失败路径。
func TestMemoryWriteExecuteBadArgs(t *testing.T) {
	tool := NewMemoryWriteTool(&fakeWriter{}, "u1")
	for _, args := range []string{"{not json", "", `{"text":"  "}`} {
		res, err := tool.Execute(context.Background(), json.RawMessage(args), nil)
		if err == nil {
			t.Fatalf("参数 %q 应报错", args)
		}
		if res.Display["code"] == nil {
			t.Fatalf("参数 %q 失败时应带 code", args)
		}
	}

	failing := NewMemoryWriteTool(&fakeWriter{err: errors.New("模拟写入故障")}, "u1")
	if _, err := failing.Execute(context.Background(), mustArgs(t, map[string]any{"text": "内容"}), nil); err == nil {
		t.Fatal("写入失败应返回 error")
	}
}

// Exec 为 nil 时不应 panic：工具不依赖 Exec（userID 在构造时绑定）。
func TestMemoryToolsTolerateNilExec(t *testing.T) {
	b := &fakeBackend{}
	b.searcher.hits = sampleHits()
	b.writer.res = Result{DocumentID: 1, Kind: "text", Title: "t", ChunkCount: 1}
	for _, tool := range NewMemoryTools(b, "u1") {
		var args json.RawMessage
		if tool.Name() == "memory_search" {
			args = mustArgs(t, map[string]any{"query": "x"})
		} else {
			args = mustArgs(t, map[string]any{"text": "x"})
		}
		if _, err := tool.Execute(context.Background(), args, nil); err != nil {
			t.Fatalf("%s 传 nil Exec 不应报错：%v", tool.Name(), err)
		}
	}
}
