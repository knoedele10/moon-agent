package web

// task-40 的 HTTP 层端到端测试：**真的走 /api/chat、真的落库**（真 MySQL），
// 模型走 mock 后端（无需凭证与联网）。默认跳过，要跑就：
//
//	$env:AGENT_IT="1"; go test ./internal/web/ -run TestChatHTTP -count=1 -v
//
// 与 agent_test.go 的分工：那边在 runAgentChat 这一层用 nil DB 跑，快且无依赖；
// 这边补齐 rag-eng 指出的缺口——"tools=true 时 SSE 里到底有没有 tool_call/tool_result"，
// 以及向后兼容在 HTTP 层是否成立。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

// sseFrames 把原始 SSE 响应体解析成 (事件名, 负载) 序列。
func sseFrames(t *testing.T, raw string) []agentTestEvent {
	t.Helper()
	var out []agentTestEvent
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name string
		var data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		var m map[string]any
		if data != "" {
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("SSE 负载不是合法 JSON（事件 %s）: %q", name, data)
			}
		}
		out = append(out, agentTestEvent{Name: name, Data: m})
	}
	return out
}

// agentHTTPTestSetup 返回 (路由, 本次测试的 userID)。
//
// **必须把 userID 带出来**：message 表是全队共享的真 MySQL，不按会话过滤就会捞到
// 别的测试/别的会话的行（verifier 独立验收时实测捞到别人的 tool_trace，导致本用例假红）。
func agentHTTPTestSetup(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = godotenv.Load("../../.env")
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	if model.DB == nil {
		t.Fatal("model.DB 未初始化，检查 .env 的 DB_* 配置")
	}

	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	registry = reg
	agent.SetStreamer(agent.RegistryStreamer(reg))
	t.Cleanup(func() {
		agent.SetStreamer(nil)
		llm.ClearMockScript()
		registry = nil
	})

	userID := "it-agent-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(ctxUserIDKey, userID)
		c.Next()
	})
	r.POST("/api/chat", postChat)

	t.Cleanup(func() {
		var convs []model.Conversation
		model.DB.Where("user_id = ?", userID).Find(&convs)
		for _, cv := range convs {
			model.DB.Where("conversation_id = ?", cv.Id).Delete(&model.Message{})
			model.DB.Delete(&model.Conversation{}, cv.Id)
		}
	})
	return r, userID
}

// agentHTTPConversationIDs 取本次测试那个用户的全部会话 id。
func agentHTTPConversationIDs(t *testing.T, userID string) []uint {
	t.Helper()
	var convs []model.Conversation
	model.DB.Where("user_id = ?", userID).Find(&convs)
	ids := make([]uint, 0, len(convs))
	for _, cv := range convs {
		ids = append(ids, cv.Id)
	}
	return ids
}

func agentHTTPPost(t *testing.T, r *gin.Engine, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，got %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestChatHTTPToolsEndToEnd 是 task-40 的**最强证据**：
// tools=true → SSE 里真的出现 tool_call/tool_result/done，且文件真的被写出来。
func TestChatHTTPToolsEndToEnd(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设 AGENT_IT=1 才跑（见本文件顶部说明）")
	}
	r, userID := agentHTTPTestSetup(t)

	root := t.TempDir()
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", root)
	// 默认档位是 confirm：不显式设成 auto，write 会被确认壳拦下，文件根本不会落盘。
	// 本用例验的是「tools=true 的工具循环与 SSE 事件」，不是确认流程（确认流程见 agent_perm_http_it_test.go）。
	t.Setenv("AGENT_MODE", ModeAuto)
	// 记忆检索指向不可达端口：连接被拒 → 注入降级为「无记忆」，不让测试挂在等 Ollama 上。
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")

	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_http_1",
			Name:      "write",
			Arguments: `{"file_path":"http.txt","content":"written over HTTP"}`,
		}}},
		{Text: "已经通过 HTTP 写好了。"},
	})

	raw := agentHTTPPost(t, r, `{"model":"mock-echo","content":"把 written over HTTP 写进 http.txt","tools":true}`)
	frames := sseFrames(t, raw)

	var names []string
	for _, f := range frames {
		names = append(names, f.Name)
	}
	if len(names) == 0 || names[len(names)-1] != "done" {
		t.Fatalf("最后一个 SSE 事件应为 done，实际: %v", names)
	}

	var sawCall, sawResult bool
	for _, f := range frames {
		switch f.Name {
		case "tool_call":
			sawCall = true
			if f.Data["name"] != "write" {
				t.Errorf("tool_call.name 应为 write，got %v", f.Data["name"])
			}
			if _, ok := f.Data["arguments"].(map[string]any); !ok {
				t.Errorf("arguments 应为对象，got %T %v", f.Data["arguments"], f.Data["arguments"])
			}
		case "tool_result":
			sawResult = true
			if f.Data["ok"] != true {
				t.Errorf("tool_result.ok 应为 true，got %v", f.Data["ok"])
			}
			if s, _ := f.Data["summary"].(string); strings.TrimSpace(s) == "" {
				t.Errorf("tool_result.summary 不应为空（前端要靠它显示一行摘要）")
			}
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("SSE 里应同时出现 tool_call 与 tool_result，实际: %v", names)
	}

	got, err := os.ReadFile(filepath.Join(root, "http.txt"))
	if err != nil {
		t.Fatalf("http.txt 应被写出，读取失败: %v", err)
	}
	if string(got) != "written over HTTP" {
		t.Fatalf("http.txt 内容不对: %q", got)
	}

	// 落库：assistant 回复 + 工具轨迹各一条。
	// **按会话过滤**：message 表是全队共享的真 MySQL，全局 Find 会捞到别人的行。
	var msgs []model.Message
	model.DB.Where("conversation_id IN ?", agentHTTPConversationIDs(t, userID)).
		Order("id asc").Find(&msgs)
	var sawTrace, sawAssistant bool
	for _, m := range msgs {
		switch m.Role {
		case roleToolTrace:
			sawTrace = true
			var entries []agentTraceEntry
			if err := json.Unmarshal([]byte(m.Content), &entries); err != nil {
				t.Errorf("tool_trace 内容不是合法 JSON: %v", err)
			} else if len(entries) == 0 || entries[0].Name != "write" {
				t.Errorf("tool_trace 应记录 write，got %+v", entries)
			}
		case "assistant":
			sawAssistant = true
		}
	}
	if !sawTrace {
		t.Errorf("应落一条 role=%s 的轨迹消息", roleToolTrace)
	}
	if !sawAssistant {
		t.Errorf("应落一条 assistant 回复")
	}
}

// TestChatHTTPBackwardCompat 是**向后兼容的 HTTP 层证据**：
// 同一个端点、同一个 mock 模型，**不带 tools** 时事件序列里不得出现任何工具事件，
// 行为与改造前一致（纯 delta → done）。
func TestChatHTTPBackwardCompat(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设 AGENT_IT=1 才跑（见本文件顶部说明）")
	}
	r, _ := agentHTTPTestSetup(t)

	// 故意把工具全开：如果兼容性破了，是因为请求没带 tools 才不该走 agent。
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", t.TempDir())
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")
	llm.ClearMockScript()

	raw := agentHTTPPost(t, r, `{"model":"mock-echo","content":"你好"}`)
	frames := sseFrames(t, raw)

	var names []string
	for _, f := range frames {
		names = append(names, f.Name)
		if f.Name == "tool_call" || f.Name == "tool_result" {
			t.Fatalf("不带 tools 的请求不得出现 %s 事件，实际序列: %v", f.Name, names)
		}
	}
	if len(names) == 0 || names[len(names)-1] != "done" {
		t.Fatalf("应以 done 收尾，实际: %v", names)
	}
	if names[0] != "delta" {
		t.Errorf("应先是 delta，实际: %v", names)
	}
}

// TestChatHTTPToolsRequestedButServerDisabled 钉住另一半兼容性：
// 客户端要 tools 但服务端没开 AGENT_TOOLS → 仍走老路径，不得出现工具事件。
func TestChatHTTPToolsRequestedButServerDisabled(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设 AGENT_IT=1 才跑（见本文件顶部说明）")
	}
	r, _ := agentHTTPTestSetup(t)

	t.Setenv("AGENT_TOOLS", "")
	t.Setenv("AGENT_FS_ROOT", t.TempDir())
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")
	llm.ClearMockScript()

	raw := agentHTTPPost(t, r, `{"model":"mock-echo","content":"你好","tools":true}`)
	frames := sseFrames(t, raw)

	var names []string
	for _, f := range frames {
		names = append(names, f.Name)
		if f.Name == "tool_call" || f.Name == "tool_result" {
			t.Fatalf("服务端未开 AGENT_TOOLS 时不得出现 %s 事件，实际序列: %v", f.Name, names)
		}
	}
	if len(names) == 0 || names[len(names)-1] != "done" {
		t.Fatalf("应以 done 收尾，实际: %v", names)
	}
}
