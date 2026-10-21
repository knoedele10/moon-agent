package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 独立复核使用真实 MySQL 和 HTTP；默认跳过，只清理本次测试用户的数据。
func TestHermesReviewSessionSearchIsolationAndPaging(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设置 AGENT_IT=1")
	}
	_, userID := agentHTTPTestSetup(t)
	foreignUser := userID + "-b"
	conversations := []model.Conversation{
		{UserID: userID, Title: "本人可检索的历史", Kind: KindWork},
		{UserID: foreignUser, Title: "其他用户的私有历史", Kind: KindWork},
	}
	if err := model.DB.Create(&conversations).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.DB.Where("conversation_id = ?", conversations[1].Id).Delete(&model.Message{})
		model.DB.Delete(&model.Conversation{}, conversations[1].Id)
	})
	const query = "查证%_Hermes"
	rows := []model.Message{
		{ConversationID: conversations[0].Id, Role: "user", Content: query + " 第一条"},
		{ConversationID: conversations[0].Id, Role: "assistant", Content: "前文 " + query + " " + strings.Repeat("后", 2000)},
		{ConversationID: conversations[0].Id, Role: "user", Content: query + " 第三条"},
		{ConversationID: conversations[0].Id, Role: roleToolTrace, Content: query + " 内部工具轨迹不得作为会话原文返回"},
		{ConversationID: conversations[0].Id, Role: "assistant", Content: "查证XXHermes，不是字面关键词的匹配"},
		{ConversationID: conversations[1].Id, Role: "assistant", Content: query + " 其他用户的秘密"},
	}
	if err := model.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	type page struct {
		Hits         []sessionSearchHit `json:"hits"`
		NextBeforeID uint               `json:"next_before_id"`
	}
	readPage := func(user string, args map[string]any) page {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		result, err := (sessionSearchTool{userID: user}).Execute(context.Background(), raw, nil)
		if err != nil || result.IsError {
			t.Fatalf("历史检索执行失败：%v %+v", err, result)
		}
		var got page
		if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
			t.Fatalf("检索结果不是合法 JSON：%v", err)
		}
		return got
	}
	first := readPage(userID, map[string]any{"query": query, "limit": 2})
	if len(first.Hits) != 2 || first.NextBeforeID == 0 {
		t.Fatalf("首屏未提供正确分页：%+v", first)
	}
	second := readPage(userID, map[string]any{"query": query, "limit": 2, "before_id": first.NextBeforeID})
	if len(second.Hits) != 1 || second.NextBeforeID != 0 {
		t.Fatalf("第二屏条数或结束游标不正确：%+v", second)
	}
	seen := make(map[uint]bool)
	previous := ^uint(0)
	for _, hit := range append(first.Hits, second.Hits...) {
		if hit.ConversationID != conversations[0].Id || hit.Role == roleToolTrace || seen[hit.MessageID] || hit.MessageID >= previous {
			t.Fatalf("发生跨用户、内部消息泄漏、重复或排序错误：%+v", hit)
		}
		if !strings.Contains(hit.Content, query) || len([]rune(hit.Content)) > 1200 {
			t.Fatalf("片段没有保留字面命中或超出上限：%+v", hit)
		}
		seen[hit.MessageID] = true
		previous = hit.MessageID
	}
	if foreign := readPage(userID, map[string]any{"conversation_id": conversations[1].Id}); len(foreign.Hits) != 0 {
		t.Fatalf("猜测他人会话编号能读到原文：%+v", foreign)
	}
	if own := readPage(foreignUser, map[string]any{"conversation_id": conversations[1].Id}); len(own.Hits) != 1 || own.Hits[0].MessageID != rows[5].Id {
		t.Fatalf("其他用户不能正常检索自己的会话：%+v", own)
	}
}

func TestHermesReviewPendingWriteRespectsChangedConversationKind(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设置 AGENT_IT=1")
	}
	hermesClearEnv(t)
	t.Setenv("AGENT_MCP_CONFIG", "")
	t.Setenv("AGENT_MCP_JSON", "")
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_ENGINE", "eino")
	t.Setenv("AGENT_AUTO_MEMORY", "0")
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")
	r, userID := permHTTPEngine(t)
	root := t.TempDir()
	permHTTPSetWorkspace(t, r, root, ModeConfirm)
	target := filepath.Join(root, "kind-switch.txt")
	llm.SetMockScript([]llm.MockTurn{permWriteCall(t, "pending-kind", "kind-switch.txt", "不应越过日常聊天的工具边界")})
	first := permHTTPChat(t, r, 0, "请写入 kind-switch.txt")
	if len(permNeedInputs(first)) != 1 {
		t.Fatalf("工作会话未进入等待确认：%v", first)
	}
	permAssertAbsent(t, target)
	convID := permOnlyConversationID(t, userID)
	var pendingCount int64
	if err := model.DB.Model(&model.AgentPendingTool{}).Where("conversation_id = ? AND user_id = ?", convID, userID).Count(&pendingCount).Error; err != nil || pendingCount != 1 {
		t.Fatalf("待确认调用没有可靠保存：count=%d err=%v", pendingCount, err)
	}
	llm.SetMockScript([]llm.MockTurn{{Text: "当前是日常聊天，写入工具不可用。"}})
	// 在真正的下一条 HTTP 请求中切换用途并确认，而不是只测工具名单。
	second := sseFrames(t, agentHTTPPost(t, r, fmt.Sprintf(`{"model":"mock-echo","conversation_id":%d,"kind":"chat","content":"确认","tools":true}`, convID)))
	permAssertAbsent(t, target)
	results := permToolResults(second)
	if len(results) != 1 || results[0]["name"] != "write" || results[0]["ok"] != false {
		t.Fatalf("切换用途后应拒绝重放原写入：%v", results)
	}
	if err := model.DB.Model(&model.AgentPendingTool{}).Where("conversation_id = ? AND user_id = ?", convID, userID).Count(&pendingCount).Error; err != nil || pendingCount != 0 {
		t.Fatalf("被拒绝的待确认调用仍有残留：count=%d err=%v", pendingCount, err)
	}
}

func TestHermesReviewPendingSpillCanBeReadByContinuingConversation(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设置 AGENT_IT=1")
	}
	hermesClearEnv(t)
	t.Setenv("AGENT_MCP_CONFIG", "")
	t.Setenv("AGENT_MCP_JSON", "")
	// 很小的阈值即可用普通写入结果覆盖长结果重放，无需启动第三方进程。
	t.Setenv("AGENT_TOOL_SPILL_BYTES", "1")
	_, userID := agentHTTPTestSetup(t)
	conv := model.Conversation{UserID: userID, Title: "待确认长结果回读", Kind: KindWork}
	if err := model.DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var spilled string
	msgs, entry := runPendingTool(context.Background(), userID, root, ModeConfirm, &model.AgentPendingTool{
		ConversationID: conv.Id, UserID: userID, ToolID: "pending-spill", ToolName: "write",
		Arguments: `{"file_path":"spill.txt","content":"confirmed"}`,
	}, func(name string, payload any) {
		if name != "tool_result" {
			return
		}
		blob, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Display struct {
				SpillPath string `json:"spill_path"`
			} `json:"display"`
		}
		if err := json.Unmarshal(blob, &result); err != nil {
			t.Fatal(err)
		}
		spilled = result.Display.SpillPath
	})
	if len(msgs) != 2 || entry == nil || !entry.OK || spilled == "" {
		t.Fatalf("确认重放没有产生完整落盘结果：entry=%+v, spill=%q", entry, spilled)
	}
	tools, _ := buildAgentToolSourceForUser(nil, userID)
	reader, found := agent.NewRegistry(tools...).Lookup("read_tool_result")
	if !found {
		t.Fatal("继续对话没有长结果读取工具")
	}
	raw, _ := json.Marshal(map[string]any{"file_path": spilled})
	result, err := reader.Execute(context.Background(), raw, &agent.Exec{SessionID: strconv.FormatUint(uint64(conv.Id), 10)})
	if err != nil || result.IsError || !strings.Contains(result.Content, "spill.txt") {
		t.Fatalf("主会话无法读回已确认调用的完整结果：err=%v result=%+v", err, result)
	}
}
