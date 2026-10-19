package rag

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"blog-my/internal/model"

	"github.com/joho/godotenv"
)

// TestMemoryIntegrationRealStore 是记忆系统的真实集成测试：真的 MySQL + 真的 Ollama 嵌入模型。
//
// 默认跳过（go test ./... 不会碰数据库）；要跑就：
//
//	$env:KB_IT="1"; go test ./internal/rag/ -run TestMemoryIntegration -count=1 -v
//
// 它用**独立的临时 userID** 写入并检索，跑完把文档删掉，不污染别人的数据。
func TestMemoryIntegrationRealStore(t *testing.T) {
	if os.Getenv("KB_IT") != "1" {
		t.Skip("需要真实 MySQL + Ollama：设 KB_IT=1 才跑（见 rag/memory_it_test.go 顶部说明）")
	}
	// 和服务同源地读配置：先加载仓库根的 .env，再用系统环境变量覆盖。
	_ = godotenv.Load("../../.env")
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}

	cfg := OllamaConfig{
		BaseURL:     os.Getenv("OLLAMA_BASE_URL"),
		EmbedModel:  os.Getenv("KB_EMBED_MODEL"),
		VisionModel: os.Getenv("KB_VISION_MODEL"),
	}
	ing := NewIngester(NewStore(model.DB), NewOllamaEmbedder(cfg), NewOllamaVision(cfg))
	ctx := context.Background()

	runTag := strconv.FormatInt(time.Now().UnixNano(), 10)
	userID := "it-memory-" + runTag // 独立用户：检索只会看到自己的 + 公共的
	marker := "ITMEM-" + runTag
	memory := "用户的验收暗号是 " + marker + "。这条记忆由 rag 集成测试写入，用来验证 memory_write 之后 memory_search 能召回。"

	// ---- 1) 空库不报错：全新用户检索，命中 0 条且不报错 ----
	emptyHits, err := RunMemorySearch(ctx, ing, "it-empty-"+runTag, "随便问一句", 5)
	if err != nil {
		t.Fatalf("空库检索不应报错：%v", err)
	}
	if len(emptyHits) != 0 {
		t.Fatalf("全新用户不应检到任何记忆，实际 %d 条", len(emptyHits))
	}
	block, err := MemoryPrompt(ctx, ing, "it-empty-"+runTag, "随便问一句", DefaultMemoryTopK)
	if err != nil || block != "" {
		t.Fatalf("空库自动注入应降级为（空串,nil），实际（%q,%v）", block, err)
	}
	t.Logf("[空库] 检索 0 条、注入块为空，均未报错")

	// ---- 2) memory_write 工具：真的写进库 ----
	writeTool := NewMemoryWriteTool(ing, userID)
	writeRes, err := writeTool.Execute(ctx, json.RawMessage(`{"text":`+strconv.Quote(memory)+`,"tags":["it","记忆"]}`), nil)
	if err != nil {
		t.Fatalf("memory_write 失败：%v", err)
	}
	if writeRes.IsError {
		t.Fatalf("memory_write 不应是 is_error：%s", writeRes.Content)
	}
	docID, _ := writeRes.Display["id"].(uint)
	if docID == 0 {
		t.Fatalf("memory_write 没有回传文档 id：%v", writeRes.Display)
	}
	if dup, _ := writeRes.Display["duplicated"].(bool); dup {
		t.Fatalf("唯一 marker 的写入不应命中去重（id=%d）", docID)
	}
	t.Logf("[写入] 工具结果：%s", writeRes.Content)

	// 无论后面断言是否失败，都要把这条测试数据删干净
	defer func() {
		ok, err := ing.DeleteDocument(ctx, docID, userID)
		if err != nil || !ok {
			t.Errorf("清理失败：id=%d ok=%v err=%v", docID, ok, err)
			return
		}
		left, err := ing.ListDocuments(ctx, userID, 10)
		if err != nil {
			t.Errorf("清理后复查失败：%v", err)
			return
		}
		if len(left) != 0 {
			t.Errorf("清理后该用户仍有 %d 个文档", len(left))
		}
		t.Logf("[清理] 已删除 id=%d，该用户剩余文档 0", docID)
	}()

	// ---- 3) memory_search 工具能把刚写的东西召回来 ----
	searchTool := NewMemorySearchTool(ing, userID)
	args := json.RawMessage(`{"query":` + strconv.Quote(marker) + `,"limit":5}`)
	searchRes, err := searchTool.Execute(ctx, args, nil)
	if err != nil {
		t.Fatalf("memory_search 失败：%v", err)
	}
	if !strings.Contains(searchRes.Content, marker) {
		t.Fatalf("检索结果里应含暗号 %s，实际：%s", marker, searchRes.Content)
	}
	if n, _ := searchRes.Display["count"].(int); n == 0 {
		t.Fatalf("Display.count 应 > 0，实际 %v", searchRes.Display["count"])
	}
	t.Logf("[检索] 命中 %v 条，Content 含暗号 %s", searchRes.Display["count"], marker)

	// ---- 4) 自动注入：同一条用户消息应该能拼出 <memory> 块 ----
	prompt, err := MemoryPrompt(ctx, ing, userID, marker, DefaultMemoryTopK)
	if err != nil {
		t.Fatalf("自动注入不应报错：%v", err)
	}
	if !strings.Contains(prompt, "<memory>") || !strings.Contains(prompt, marker) {
		t.Fatalf("注入块应含 <memory> 与暗号，实际：%q", clipRunes(prompt, 200))
	}
	t.Logf("[注入] <memory> 块长度 %d 字符，含暗号", len([]rune(prompt)))

	// ---- 5) 幂等：同一段文本再写一次 → 去重命中，不新建 ----
	again, err := writeTool.Execute(ctx, json.RawMessage(`{"text":`+strconv.Quote(memory)+`,"tags":["it","记忆"]}`), nil)
	if err != nil {
		t.Fatalf("重复写入失败：%v", err)
	}
	if dup, _ := again.Display["duplicated"].(bool); !dup {
		t.Fatalf("同一段文本重复写入应命中去重，实际 Display=%v", again.Display)
	}
	if againID, _ := again.Display["id"].(uint); againID != docID {
		t.Fatalf("去重后 id 应保持 %d，实际 %v", docID, againID)
	}
	t.Logf("[去重] 重复写入返回：%s", again.Content)
}
