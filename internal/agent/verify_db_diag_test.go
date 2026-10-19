package agent_test

// 诊断用（默认跳过）：证明共享真 MySQL 上「全局查询」会取到别的用例/别人的 tool_trace。
//   $env:VERIFY_DB_DIAG="1"; go test ./internal/agent/ -run TestVerifyDBIsolationDiag -count=1 -v

import (
	"os"
	"testing"

	"blog-my/internal/model"

	"github.com/joho/godotenv"
)

func TestVerifyDBIsolationDiag(t *testing.T) {
	if os.Getenv("VERIFY_DB_DIAG") != "1" {
		t.Skip("设 VERIFY_DB_DIAG=1 才跑（只读诊断）")
	}
	_ = godotenv.Load("../../.env")
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	if model.DB == nil {
		t.Fatal("model.DB 未初始化")
	}

	var traces []model.Message
	if err := model.DB.Where("role = ?", "tool_trace").Order("id desc").Limit(6).Find(&traces).Error; err != nil {
		t.Fatalf("查 tool_trace 失败：%v", err)
	}
	t.Logf("库里最近 %d 条 tool_trace：", len(traces))
	for _, m := range traces {
		var conv model.Conversation
		model.DB.Where("id = ?", m.ConversationID).First(&conv)
		r := []rune(m.Content)
		preview := string(r)
		if len(r) > 80 {
			preview = string(r[:80])
		}
		t.Logf("  msg#%d conv=%d user=%q content=%q", m.Id, m.ConversationID, conv.UserID, preview)
	}

	// 实现方 web/agent_http_it_test.go 的查询形态：Order("id asc").Find(&msgs)（无会话过滤）
	var all []model.Message
	model.DB.Order("id asc").Find(&all)
	for _, m := range all {
		if m.Role == "tool_trace" {
			r := []rune(m.Content)
			preview := string(r)
			if len(r) > 80 {
				preview = string(r[:80])
			}
			t.Logf("全局 Order(id asc) 的第一条 tool_trace：msg#%d conv=%d content=%q", m.Id, m.ConversationID, preview)
			break
		}
	}
}

// TestVerifyDBCleanAcceptanceMemory 清掉验收用例写进知识库的文档（标题前缀），并打印删除行数。
//
//	$env:VERIFY_DB_CLEAN="1"; go test ./internal/agent/ -run TestVerifyDBCleanAcceptanceMemory -count=1 -v
//
// 为什么要 DB 级清理：kb_document.user_id 是按会话用户隔离的，用例每次跑用的是新 uid，
// 跨进程清理做不到；而验收期我确实写过这些文档，必须自己收拾干净。
func TestVerifyDBCleanAcceptanceMemory(t *testing.T) {
	if os.Getenv("VERIFY_DB_CLEAN") != "1" {
		t.Skip("设 VERIFY_DB_CLEAN=1 才跑（会删数据）")
	}
	_ = godotenv.Load("../../.env")
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	if model.DB == nil {
		t.Fatal("model.DB 未初始化")
	}
	const prefix = "验收记忆 VERIFY-%"

	var docs []model.KBDocument
	model.DB.Where("title LIKE ?", prefix).Find(&docs)
	ids := make([]uint, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.Id)
	}
	t.Logf("匹配到的验收文档：%d 条 %v", len(ids), ids)
	if len(ids) == 0 {
		return
	}
	chunks := model.DB.Where("document_id IN ?", ids).Delete(&model.KBChunk{})
	if chunks.Error != nil {
		t.Fatalf("删 chunk 失败：%v", chunks.Error)
	}
	del := model.DB.Where("id IN ?", ids).Delete(&model.KBDocument{})
	if del.Error != nil {
		t.Fatalf("删 document 失败：%v", del.Error)
	}
	t.Logf("已删除：chunk %d 行，document %d 行", chunks.RowsAffected, del.RowsAffected)

	var left int64
	model.DB.Model(&model.KBDocument{}).Where("title LIKE ?", prefix).Count(&left)
	if left != 0 {
		t.Fatalf("清理后仍剩 %d 条", left)
	}
	t.Log("清理后剩余 = 0 ✓")
}
