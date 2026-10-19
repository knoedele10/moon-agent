package agent_test

// 独立验收（task-43，verifier 自写）：HTTP/SSE 层，走**真路由 + 真 MySQL**，模型走 mock 后端。
// 默认跳过，跑法：
//
//	$env:AGENT_IT="1"; go test ./internal/agent/ -run TestVerifyHTTP -count=1 -v
//
// 与实现方 web/agent_http_it_test.go 的区别：那边在 web 包内直接挂 postChat；这里用导出的
// web.Setup 装**完整路由**（含 authRequired 中间件），session cookie 按 middleware.go 的算法
// 自己签出来 —— 顺带独立复核了会话签名这条路真的能过鉴权。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
	"blog-my/internal/web"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type vFrame struct {
	Name string
	Data map[string]any
}

func vSSE(t *testing.T, raw string) []vFrame {
	t.Helper()
	var out []vFrame
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		m := map[string]any{}
		if data != "" {
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("事件 %s 的负载不是合法 JSON：%q", name, data)
			}
		}
		out = append(out, vFrame{Name: name, Data: m})
	}
	return out
}

func vNames(fs []vFrame) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// vCookie 复刻 web/middleware.go 的 makeSession：base64url(uid) + "." + base64url(HMAC-SHA256(APP_SECRET, uid))
func vCookie(t *testing.T, uid string) string {
	t.Helper()
	secret := os.Getenv("APP_SECRET")
	if secret == "" {
		t.Fatal("APP_SECRET 未配置（.env），签不出会话 cookie")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(uid))
	return base64.RawURLEncoding.EncodeToString([]byte(uid)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func vSetup(t *testing.T) (*gin.Engine, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = godotenv.Load("../../.env")
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	if model.DB == nil {
		t.Fatal("model.DB 未初始化，检查 .env 的 DB_* 配置")
	}
	uid := fmt.Sprintf("verify-http-%d", time.Now().UnixNano())
	cookie := vCookie(t, uid)
	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	r := gin.New()
	web.Setup(r, reg)
	// 关键：测试**不替它装缝**。装了就会把「web.Setup 漏装」这个真缺口盖住
	// （cua-ui 真机抓到过一次：生产 tools=true 直接返回 ErrNoStreamer）。
	if agent.Streamer() == nil {
		t.Fatal("web.Setup 之后 agent.Streamer() 仍是 nil：生产 tools=true 会返回 agent: 未安装模型调用实现")
	}
	t.Cleanup(func() {
		agent.SetStreamer(nil)
		llm.ClearMockScript()
		var convs []model.Conversation
		model.DB.Where("user_id = ?", uid).Find(&convs)
		for _, cv := range convs {
			model.DB.Where("conversation_id = ?", cv.Id).Delete(&model.Message{})
			model.DB.Delete(&model.Conversation{}, cv.Id)
		}
	})
	return r, cookie, uid
}

// vConvIDs 只取本用例这个 uid 的会话 —— 真 MySQL 是所有用例共享的，全局查询会被污染。
func vConvIDs(t *testing.T, uid string) []uint {
	t.Helper()
	var convs []model.Conversation
	model.DB.Where("user_id = ?", uid).Order("id asc").Find(&convs)
	ids := make([]uint, 0, len(convs))
	for _, c := range convs {
		ids = append(ids, c.Id)
	}
	return ids
}

func vPost(t *testing.T, r *gin.Engine, cookie, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session="+cookie)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，got %d：%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func vNeedIT(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真 MySQL：设 AGENT_IT=1 才跑")
	}
}

// TestVerifyHTTPBackCompatToolsDefault：契约 §9.3 —— tools 缺省时事件序列与改造前一致。
// 改造前（HEAD:web/chat.go）只发 error/delta/done；所以「一个字节不差」的可观测形式就是：
// 事件名只能落在这三个里，且必须有 delta、以 done 收尾。
func TestVerifyHTTPBackCompatToolsDefault(t *testing.T) {
	vNeedIT(t)
	r, cookie, _ := vSetup(t)
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1") // 记忆检索不可达：不许挂住对话
	t.Setenv("AGENT_TOOLS", "")                       // 服务端开关缺省（关闭）

	for _, body := range []string{
		`{"model":"mock-echo","content":"你好"}`,
		`{"model":"mock-echo","content":"你好","tools":false}`,
	} {
		names := vNames(vSSE(t, vPost(t, r, cookie, body)))
		if len(names) == 0 || names[len(names)-1] != "done" {
			t.Fatalf("应以 done 收尾：%v", names)
		}
		sawDelta := false
		for _, n := range names {
			switch n {
			case "delta":
				sawDelta = true
			case "tool_call", "tool_result":
				t.Fatalf("AGENT_TOOLS 没开却出现工具事件：%v", names)
			case "done", "error":
			default:
				t.Fatalf("出现老路径不该有的事件 %q：%v", n, names)
			}
		}
		if !sawDelta {
			t.Fatalf("一个 delta 都没有，模型根本没被调用：%v", names)
		}
		t.Logf("老路径事件序列 = %v", names)
	}

	// 服务端开了、但请求没要 tools：仍然走老路径
	t.Setenv("AGENT_TOOLS", "1")
	names := vNames(vSSE(t, vPost(t, r, cookie, `{"model":"mock-echo","content":"你好"}`)))
	for _, n := range names {
		if n == "tool_call" || n == "tool_result" {
			t.Fatalf("请求没带 tools 却出现工具事件：%v", names)
		}
	}
	t.Logf("AGENT_TOOLS=1 但请求不带 tools = %v", names)
}

// TestVerifyHTTPToolsGateNegativeControl：上面那条断言必须**有灵敏度**——真要了 tools 且开关开着，
// 工具事件必须出现。否则「没出现工具事件」是永远为真的假证据。
func TestVerifyHTTPToolsGateNegativeControl(t *testing.T) {
	vNeedIT(t)
	r, cookie, _ := vSetup(t)
	root := t.TempDir()
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", root)
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")
	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "nc1", Name: "write", Arguments: `{"file_path":"nc.txt","content":"negative-control"}`}}},
		{Text: "写好了"},
	})
	frames := vSSE(t, vPost(t, r, cookie, `{"model":"mock-echo","content":"写一下","tools":true}`))
	names := vNames(frames)
	sawCall, sawResult := false, false
	for _, f := range frames {
		switch f.Name {
		case "tool_call":
			sawCall = true
		case "tool_result":
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("tools=true 且开关开着，必须出现 tool_call/tool_result：%v", names)
	}
	if b, err := os.ReadFile(filepath.Join(root, "nc.txt")); err != nil || string(b) != "negative-control" {
		t.Fatalf("负向对照的文件没真写出来：%v %q", err, string(b))
	}
	t.Logf("tools=true 事件序列 = %v", names)
}

// TestVerifyHTTPReadWriteLoop：契约 §9.4 —— 一次 read + 一次 write，文件系统真的变了、事件序列正确、轨迹落库。
func TestVerifyHTTPReadWriteLoop(t *testing.T) {
	vNeedIT(t)
	r, cookie, uid := vSetup(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "src.txt"), []byte("hello-from-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", root)
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")

	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{ID: "r1", Name: "read", Arguments: `{"file_path":"src.txt"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "w1", Name: "write", Arguments: `{"file_path":"dst.txt","content":"copied-by-tool"}`}}},
		{Text: "复制完了"},
	})
	raw := vPost(t, r, cookie, `{"model":"mock-echo","content":"把 src.txt 抄到 dst.txt","tools":true}`)
	frames := vSSE(t, raw)
	names := vNames(frames)

	// 事件顺序：read 的 call/result 必须早于 write 的 call/result，最后 done
	idx := map[string][]int{}
	for i, f := range frames {
		if f.Name == "tool_call" || f.Name == "tool_result" {
			nm, _ := f.Data["name"].(string)
			idx[f.Name+":"+nm] = append(idx[f.Name+":"+nm], i)
		}
	}
	if len(idx["tool_call:read"]) == 0 || len(idx["tool_result:read"]) == 0 ||
		len(idx["tool_call:write"]) == 0 || len(idx["tool_result:write"]) == 0 {
		t.Fatalf("read/write 的 tool_call 与 tool_result 都要出现：%v", names)
	}
	if !(idx["tool_call:read"][0] < idx["tool_result:read"][0] &&
		idx["tool_result:read"][0] < idx["tool_call:write"][0] &&
		idx["tool_call:write"][0] < idx["tool_result:write"][0]) {
		t.Fatalf("工具事件顺序不对：%v", idx)
	}
	if names[len(names)-1] != "done" {
		t.Fatalf("应以 done 收尾：%v", names)
	}
	// read 的结果里得有文件内容（工具真的读到了盘上的东西）
	for _, f := range frames {
		if f.Name == "tool_result" && f.Data["name"] == "read" {
			if s, _ := f.Data["detail"].(string); !strings.Contains(s, "hello-from-disk") {
				t.Errorf("read 的 detail 应含文件内容，实际 %q", s)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "dst.txt")); err != nil || string(b) != "copied-by-tool" {
		t.Fatalf("dst.txt 没被写对：%v %q", err, string(b))
	}
	// 落库：assistant 回复 + tool_trace（含 read/write 两条）
	ids := vConvIDs(t, uid)
	if len(ids) == 0 {
		t.Fatal("本用例没有建出会话，无法按会话过滤")
	}
	var msgs []model.Message
	model.DB.Where("conversation_id IN ?", ids).Where("role = ?", "assistant").Order("id desc").Limit(1).Find(&msgs)
	if len(msgs) == 0 || !strings.Contains(msgs[0].Content, "复制完了") {
		t.Fatalf("assistant 回复没落库：%+v", msgs)
	}
	var traces []model.Message
	model.DB.Where("conversation_id IN ?", ids).Where("role = ?", "tool_trace").Order("id desc").Limit(1).Find(&traces)
	if len(traces) == 0 {
		t.Fatalf("tool_trace 没落库")
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(traces[0].Content), &entries); err != nil {
		t.Fatalf("tool_trace 不是合法 JSON：%v", err)
	}
	gotNames := ""
	for _, e := range entries {
		gotNames += fmt.Sprintf("%v,", e["name"])
	}
	if len(entries) != 2 || !strings.Contains(gotNames, "read") || !strings.Contains(gotNames, "write") {
		t.Fatalf("tool_trace 应记录 read+write，实际 %v", gotNames)
	}
	t.Logf("事件序列 = %v", names)
}

// TestVerifyMemoryDegradation：契约 §9.7 —— 检索失败不阻断对话。
// 嵌入端点指向不可达端口，对话必须照常跑完（done），且不许出现 error 事件。
func TestVerifyMemoryDegradation(t *testing.T) {
	vNeedIT(t)
	r, cookie, _ := vSetup(t)
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", t.TempDir())
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1") // 检索必然失败
	llm.SetMockScript([]llm.MockTurn{{Text: "没有记忆我也能答"}})

	raw := vPost(t, r, cookie, `{"model":"mock-echo","content":"检索失败时你还答得出来吗","tools":true}`)
	frames := vSSE(t, raw)
	names := vNames(frames)
	for _, f := range frames {
		if f.Name == "error" {
			t.Fatalf("检索失败不该把对话打断：%v（%v）", names, f.Data)
		}
	}
	if names[len(names)-1] != "done" {
		t.Fatalf("应以 done 收尾：%v", names)
	}
	var acc strings.Builder
	for _, f := range frames {
		if f.Name == "delta" {
			if s, _ := f.Data["content"].(string); s != "" {
				acc.WriteString(s)
			}
		}
	}
	if !strings.Contains(acc.String(), "没有记忆我也能答") {
		t.Fatalf("回复内容不对：%q", acc.String())
	}
	t.Logf("降级后事件序列 = %v，回复 = %q", names, acc.String())

	// 再抓一次模型请求：system prompt 里**不许**出现记忆块（真降级，而不是注入了空块）
	var mu sync.Mutex
	var captured llm.ChatRequest
	agent.SetStreamer(func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error) {
		mu.Lock()
		captured = req
		mu.Unlock()
		ch := make(chan llm.Event, 4)
		ch <- llm.Event{Type: llm.EventDelta, Content: "再答一次"}
		ch <- llm.Event{Type: llm.EventDone}
		close(ch)
		return ch, nil
	})
	_, _ = vPostPath(t, r, cookie, "/api/chat", `{"model":"mock-echo","content":"再来一次","tools":true}`)
	mu.Lock()
	sys := ""
	for _, m := range captured.Messages {
		if m.Role == llm.RoleSystem {
			sys = m.Content
		}
	}
	mu.Unlock()
	if strings.Contains(sys, "<memory>") {
		t.Fatalf("检索不可达时不该注入记忆块，实际 system prompt 含 <memory>：%q", sys)
	}
	t.Logf("降级：system prompt 无 <memory>（长度 %d）✓", len(sys))
}

// TestVerifyStreamerInstalledBySetup 独立复验 cua-ui 真机抓到的缺口（契约 v1.1 A 节）：
// 「模型调用缝」必须由**生产入口自己**装好，不能靠调用方记得装。本用例不需要 MySQL，属默认门禁。
func TestVerifyStreamerInstalledBySetup(t *testing.T) {
	agent.SetStreamer(nil) // 先清干净：只能由 web.Setup 装回来
	t.Cleanup(func() { agent.SetStreamer(nil) })

	gin.SetMode(gin.TestMode)
	_ = godotenv.Load("../../.env")
	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	r := gin.New()
	web.Setup(r, reg)

	if agent.Streamer() == nil {
		t.Fatal("web.Setup 之后 agent.Streamer() 仍为 nil —— 生产里 tools=true 会直接返回 agent: 未安装模型调用实现")
	}
	t.Log("web.Setup 之后 agent.Streamer() != nil ✓")
}

// vReq 发任意方法的请求。
func vReq(t *testing.T, r *gin.Engine, cookie, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", "session="+cookie)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// vCleanupVerifyMemoryDocs 删掉本项目验收用例写进知识库的记忆文档（标题带前缀）。
// 真 MySQL 是共享的，用例必须自己收拾干净；顺带也能自愈清掉历史遗留。
func vCleanupVerifyMemoryDocs(t *testing.T, r *gin.Engine, cookie string) int {
	t.Helper()
	code, body := vReq(t, r, cookie, http.MethodGet, "/api/kb/documents", "")
	if code != http.StatusOK {
		t.Logf("列知识库失败（code=%d），跳过清理", code)
		return 0
	}
	var listed struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Logf("知识库列表解析失败：%v", err)
		return 0
	}
	removed := 0
	for _, doc := range listed.Data {
		title, _ := doc["title"].(string)
		if !strings.HasPrefix(title, "验收记忆 VERIFY-") {
			continue
		}
		idf, _ := doc["id"].(float64)
		if idf == 0 {
			continue
		}
		if c, _ := vReq(t, r, cookie, http.MethodDelete, fmt.Sprintf("/api/kb/documents/%d", int(idf)), ""); c == http.StatusOK || c == http.StatusNoContent {
			removed++
		}
	}
	return removed
}

// vPostPath 发任意路径的 POST，返回状态码与响应体。
func vPostPath(t *testing.T, r *gin.Engine, cookie, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session="+cookie)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestVerifyMemoryInjectionEndToEnd 正面路径：真 Ollama 嵌入 + 真 MySQL 入库后，
// 自动注入的 <memory> 块必须真的进了 system prompt（用**录音 streamer** 抓模型请求）。
//
//	$env:VERIFY_MEMORY_E2E="1"; go test ./internal/agent/ -run TestVerifyMemoryInjection -count=1 -v
//
// 负面路径与它配对：同一个请求把嵌入端点指向不可达端口 → system prompt 里不许有 <memory>（且对话照常完成）。
func TestVerifyMemoryInjectionEndToEnd(t *testing.T) {
	if os.Getenv("VERIFY_MEMORY_E2E") != "1" {
		t.Skip("需要真 Ollama + 真 MySQL：设 VERIFY_MEMORY_E2E=1 才跑")
	}
	r, cookie, uid := vSetup(t)
	_ = uid
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_FS_ROOT", t.TempDir())
	t.Logf("嵌入端点 = %s", strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")))

	t.Cleanup(func() { vCleanupVerifyMemoryDocs(t, r, cookie) })
	if n := vCleanupVerifyMemoryDocs(t, r, cookie); n > 0 {
		t.Logf("清理历史遗留的验收记忆文档 %d 条", n)
	}

	nonce := fmt.Sprintf("VERIFY-%d", time.Now().UnixNano())
	memText := "暗号 " + nonce + "：用户的猫叫团子"
	kbBody, _ := json.Marshal(map[string]any{"title": "验收记忆 " + nonce, "text": memText})
	code, resp := vReq(t, r, cookie, http.MethodPost, "/api/kb/text", string(kbBody))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Skipf("知识库入库不可用（多半是 Ollama 没拉嵌入模型）：code=%d body=%s", code, resp)
	}
	t.Logf("入库成功：code=%d", code)

	// 录音 streamer：只假模型，不假记忆
	var mu sync.Mutex
	var captured llm.ChatRequest
	calls := 0
	agent.SetStreamer(func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error) {
		mu.Lock()
		captured = req
		calls++
		mu.Unlock()
		ch := make(chan llm.Event, 4)
		ch <- llm.Event{Type: llm.EventDelta, Content: "收到"}
		ch <- llm.Event{Type: llm.EventDone}
		close(ch)
		return ch, nil
	})
	code2, sse := vReq(t, r, cookie, http.MethodPost, "/api/chat", `{"model":"mock-echo","content":"我的猫叫什么？","tools":true}`)
	if code2 != http.StatusOK {
		t.Fatalf("chat 状态 %d：%s", code2, sse)
	}
	mu.Lock()
	sys := ""
	for _, m := range captured.Messages {
		if m.Role == llm.RoleSystem {
			sys = m.Content
		}
	}
	got := calls
	mu.Unlock()
	if got == 0 {
		t.Fatal("录音 streamer 没被调用，抓不到模型请求")
	}
	if !strings.Contains(sys, "<memory>") || !strings.Contains(sys, nonce) {
		t.Fatalf("system prompt 里应有本轮检索到的 <memory> 块（含 %s），实际长度 %d", nonce, len(sys))
	}
	if !strings.Contains(sse, "event: done") {
		t.Fatalf("SSE 应以 done 收尾：%q", sse)
	}
	t.Logf("正面：system prompt 含 <memory> 且含本轮记忆 %s（system 长度 %d）", nonce, len(sys))
}
