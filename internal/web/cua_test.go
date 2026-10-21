package web

// CUA 网关与模型代理的单元测试。全部离线：不启 CUA 子进程、不打真实模型 API。
// 真实链路的黑盒验证见 docs/verify（由 verifier 独立复核）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"blog-my/internal/llm"

	einomodel "github.com/cloudwego/eino/components/model"
	einoschema "github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

// 1x1 PNG 的 base64（内容不重要，只验证 MIME 与字节被原样解出来）。
const cuaTestPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func cuaTestSetup() {
	gin.SetMode(gin.TestMode)
	reg := llm.NewRegistry()
	eps := llm.DefaultEndpoints()
	eps = append(eps, llm.Endpoint{ID: "test-empty", Label: "空回复桩", Backend: "empty", Models: []string{"empty-model"}})
	reg.SetEndpoints(eps)
	reg.Register("empty", cuaEmptyBackend)
	registry = reg
	cuaMgr = &cuaManager{byToken: map[string]*cuaRun{}}
}

func cuaTestRouter() *gin.Engine {
	r := gin.New()
	// 测试里没有真实会话中间件：用 X-Test-User 头顶替登录态，
	// 让「只返回当前用户自己的 run」这类归属校验能被真实覆盖。
	r.Use(func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(ctxUserIDKey, u)
		}
		c.Next()
	})
	r.GET("/api/cua/status", cuaStatus)
	r.GET("/api/cua/runs", cuaListRuns)
	r.POST("/api/cua/runs", cuaCreateRun)
	r.GET("/api/cua/runs/:id/events", cuaRunEvents)
	r.POST("/api/cua/runs/:id/stop", cuaStopRun)
	r.POST("/internal/cua/llm/v1/chat/completions", cuaLLMProxy)
	return r
}

func cuaDo(r *gin.Engine, method, path, body, auth string) *httptest.ResponseRecorder {
	return cuaDoAs(r, method, path, body, auth, "")
}

// cuaDoAs 以指定用户身份发请求；user 为空表示未登录（currentUserID 返回 ""）。
func cuaDoAs(r *gin.Engine, method, path, body, auth, user string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- 契约 3：data URI → llm.Image ---

func TestCuaDecodeDataURI(t *testing.T) {
	mime, data, err := cuaDecodeDataURI("data:image/png;base64," + cuaTestPNGB64)
	if err != nil {
		t.Fatalf("合法 data URI 解析失败：%v", err)
	}
	if mime != "image/png" {
		t.Errorf("MIME = %q，想要 image/png", mime)
	}
	want, _ := base64.StdEncoding.DecodeString(cuaTestPNGB64)
	if len(data) != len(want) {
		t.Fatalf("字节长度 = %d，想要 %d", len(data), len(want))
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("第 %d 字节 = %d，想要 %d", i, data[i], want[i])
		}
	}

	// base64 里插了换行也要能解（有些客户端会折行）。
	withBreaks := cuaTestPNGB64[:10] + "\n" + cuaTestPNGB64[10:20] + "\r\n" + cuaTestPNGB64[20:]
	if _, d2, err := cuaDecodeDataURI("data:image/jpeg;base64," + withBreaks); err != nil || len(d2) != len(want) {
		t.Errorf("含换行的 base64 解析失败：err=%v len=%d", err, len(d2))
	}

	for _, bad := range []string{"https://example.com/a.png", "data:image/png,abc", "data:image/png;base64,!!!not-base64!!!"} {
		if _, _, err := cuaDecodeDataURI(bad); err == nil {
			t.Errorf("非法输入 %q 应该报错", bad)
		}
	}
}

func TestCuaConvertMessages(t *testing.T) {
	payload := `[
	  {"role":"system","content":"你是操作员"},
	  {"role":"user","content":"看这张图"},
	  {"role":"user","content":[
	     {"type":"text","text":"第一段"},
	     {"type":"image_url","image_url":{"url":"data:image/png;base64,` + cuaTestPNGB64 + `"}},
	     {"type":"text","text":"第二段"}
	  ]}
	]`
	var raw []cuaOpenAIMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		t.Fatalf("测试负载不合法：%v", err)
	}
	msgs, err := cuaConvertMessages(raw)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("消息条数 = %d，想要 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "你是操作员" {
		t.Errorf("字符串 content 没被正确映射：%+v", msgs[0])
	}
	if msgs[2].Role != llm.RoleUser {
		t.Errorf("role = %q", msgs[2].Role)
	}
	// 契约 3：文字按顺序拼接，图片解出 MIME + 原始字节落到 llm.Message.Images。
	if msgs[2].Content != "第一段"+"\n"+"第二段" {
		t.Errorf("多 part 文字拼接 = %q", msgs[2].Content)
	}
	if len(msgs[2].Images) != 1 {
		t.Fatalf("Images 数量 = %d，想要 1", len(msgs[2].Images))
	}
	want, _ := base64.StdEncoding.DecodeString(cuaTestPNGB64)
	if msgs[2].Images[0].MIME != "image/png" {
		t.Errorf("Image.MIME = %q", msgs[2].Images[0].MIME)
	}
	if len(msgs[2].Images[0].Data) != len(want) {
		t.Errorf("Image.Data 长度 = %d，想要 %d", len(msgs[2].Images[0].Data), len(want))
	}

	// 坏图片必须报错，而不是悄悄丢图。
	bad := []cuaOpenAIMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]`)}}
	if _, err := cuaConvertMessages(bad); err == nil {
		t.Error("非 data URI 图片应该报错")
	}
}

// --- 契约 3：上游真实原因必须透出 ---

func TestCuaUpstreamErrorInfo(t *testing.T) {
	apiErr := &goopenai.APIError{
		Message:        "This model's maximum context length is 8192 tokens",
		HTTPStatusCode: 400,
		HTTPStatus:     "400 Bad Request",
	}
	status, msg := cuaUpstreamErrorInfo(apiErr)
	if status != 400 {
		t.Errorf("status = %d，想要 400", status)
	}
	if !strings.Contains(msg, "maximum context length is 8192") {
		t.Errorf("真实原因丢了：%q", msg)
	}

	reqErr := &goopenai.RequestError{
		HTTPStatusCode: 429,
		Body:           []byte(`{"error":{"message":"Rate limit reached for requests"}}`),
	}
	if status, msg := cuaUpstreamErrorInfo(reqErr); status != 429 || !strings.Contains(msg, "Rate limit reached") {
		t.Errorf("RequestError 映射错：status=%d msg=%q", status, msg)
	}

	if status, _ := cuaUpstreamErrorInfo(llm.ErrMissingKey); status != http.StatusBadRequest {
		t.Errorf("缺 key 应该是 400，得到 %d", status)
	}
	if status, msg := cuaUpstreamErrorInfo(context.DeadlineExceeded); status != http.StatusBadGateway || msg == "" {
		t.Errorf("未知错误应回落 502 且带原文：status=%d msg=%q", status, msg)
	}
}

// --- 契约 2.1：status.models 只列视觉模型 ---

func TestCuaVisionModels(t *testing.T) {
	cuaTestSetup()
	items := cuaVisionModels()
	found := map[string]bool{}
	for _, it := range items {
		found[it.Name] = true
		if !it.Vision {
			t.Errorf("%s 出现在视觉列表里但 vision=false", it.Name)
		}
	}
	if !found["deepseek-flash"] {
		t.Errorf("status.models 必须包含 deepseek-flash，实际 %v", found)
	}
	if found["deepseek-v4-pro"] {
		t.Errorf("deepseek-v4-pro 不支持视觉，不得出现，实际 %v", found)
	}
	if found["deepseek-chat"] {
		t.Errorf("纯文本模型 deepseek-chat 不得出现，实际 %v", found)
	}
	if !found["qwen2.5vl:7b"] {
		t.Errorf("名字启发式应认出 qwen2.5vl:7b，实际 %v", found)
	}
}

// --- 契约 2.2：400 / 409 边界（不发真实 run） ---

func TestCuaCreateRunBoundaries(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()

	long := strings.Repeat("a", cuaTaskMaxLen+1)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"task 为空", `{"task":"  ","model":"deepseek-flash"}`, 400},
		{"task 超长", `{"task":"` + long + `","model":"deepseek-flash"}`, 400},
		{"model 为空", `{"task":"x","model":""}`, 400},
		{"model 非视觉", `{"task":"x","model":"deepseek-v4-pro"}`, 400},
		{"model 不存在", `{"task":"x","model":"no-such-model"}`, 400},
		{"max_steps 0", `{"task":"x","model":"deepseek-flash","max_steps":0}`, 400},
		{"max_steps 201", `{"task":"x","model":"deepseek-flash","max_steps":201}`, 400},
		{"max_seconds 9", `{"task":"x","model":"deepseek-flash","max_seconds":9}`, 400},
		{"max_seconds 3601", `{"task":"x","model":"deepseek-flash","max_seconds":3601}`, 400},
		{"请求体非法", `{oops`, 400},
	}
	for _, tc := range cases {
		w := cuaDo(r, http.MethodPost, "/api/cua/runs", tc.body, "")
		if w.Code != tc.want {
			t.Errorf("%s：状态码 = %d，想要 %d，body=%s", tc.name, w.Code, tc.want, w.Body.String())
		}
		var got map[string]any
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || got["error"] == nil {
			t.Errorf("%s：400 应返回带 error 字段的 JSON，实际 %s", tc.name, w.Body.String())
		}
	}

	// 409：先占住唯一运行位。
	// 该测试路由没有会话，占用位用匿名用户（UserID ""）建，与 HTTP 建出来的 run 同口径。
	busy := newCUARun(cuaNewID(), "", "占位", "deepseek-flash", cuaNewID(), true)
	if !cuaMgr.reserve(busy) {
		t.Fatal("占位 run 抢占失败")
	}
	w := cuaDo(r, http.MethodPost, "/api/cua/runs", `{"task":"x","model":"deepseek-flash"}`, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("并发 run 应该 409，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "已有任务在运行") {
		t.Errorf("409 文案不符契约：%s", w.Body.String())
	}

	// 契约 2.5：列表能看到占位 run。
	lw := cuaDo(r, http.MethodGet, "/api/cua/runs", "", "")
	if lw.Code != 200 {
		t.Fatalf("列表状态码 = %d", lw.Code)
	}
	var list struct {
		Runs []cuaRunItem `json:"runs"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil || len(list.Runs) != 1 {
		t.Fatalf("列表内容不对：%s", lw.Body.String())
	}
	if list.Runs[0].Status != "running" || list.Runs[0].RunID != busy.ID {
		t.Errorf("列表项不对：%+v", list.Runs[0])
	}
}

// 归属隔离（P1 回归）：列表只回当前用户自己的 run；
// 别人的 run 在事件流与停止接口上一律 404，且不能泄漏任务文本。
func TestCuaRunIsolation(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()

	alice := newCUARun(cuaNewID(), "alice", "看我的屏幕", "deepseek-flash", cuaNewID(), true)
	if !cuaMgr.reserve(alice) {
		t.Fatal("alice 的 run 抢占失败")
	}
	alice.finish("done", "", 0)
	cuaMgr.clearCurrent(alice)

	bob := newCUARun(cuaNewID(), "bob", "别人的任务", "deepseek-flash", cuaNewID(), true)
	if !cuaMgr.reserve(bob) {
		t.Fatal("bob 的 run 抢占失败")
	}
	bob.finish("done", "", 0)
	cuaMgr.clearCurrent(bob)

	// 列表：alice 只看到自己那条，且响应体里没有 bob 的任务文本。
	lw := cuaDoAs(r, http.MethodGet, "/api/cua/runs", "", "", "alice")
	if lw.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", lw.Code)
	}
	var list struct {
		Runs []cuaRunItem `json:"runs"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表解析失败：%v", err)
	}
	if len(list.Runs) != 1 || list.Runs[0].RunID != alice.ID {
		t.Fatalf("alice 只该看到自己的 run，实际 %s", lw.Body.String())
	}
	if strings.Contains(lw.Body.String(), "别人的任务") {
		t.Fatal("列表泄漏了他人任务文本")
	}

	// 越权：事件流与停止都必须 404，不能给出"存在但无权"的差异信号。
	if code := cuaDoAs(r, http.MethodGet, "/api/cua/runs/"+alice.ID+"/events", "", "", "bob").Code; code != http.StatusNotFound {
		t.Errorf("bob 读 alice 事件流应 404，实际 %d", code)
	}
	if code := cuaDoAs(r, http.MethodPost, "/api/cua/runs/"+alice.ID+"/stop", "", "", "bob").Code; code != http.StatusNotFound {
		t.Errorf("bob 停 alice 任务应 404，实际 %d", code)
	}
	// 本人：已结束的 run 走历史补发，仍应 200。
	if code := cuaDoAs(r, http.MethodGet, "/api/cua/runs/"+alice.ID+"/events", "", "", "alice").Code; code != http.StatusOK {
		t.Errorf("alice 读自己的事件流应 200，实际 %d", code)
	}
}

// --- 契约 2.1：status 永远 200 ---

func TestCuaStatusAlwaysOK(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()
	w := cuaDo(r, http.MethodGet, "/api/cua/status", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status 必须永远 200，实际 %d", w.Code)
	}
	var got struct {
		Available bool           `json:"available"`
		Running   bool           `json:"running"`
		RunID     string         `json:"run_id"`
		Python    map[string]any `json:"python"`
		CUA       map[string]any `json:"cua"`
		Models    []cuaModelInfo `json:"models"`
		Notes     []string       `json:"notes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status 不是合法 JSON：%v", err)
	}
	if got.Running || got.RunID != "" {
		t.Errorf("没有 run 时 running 应为 false / run_id 为空：%+v", got)
	}
	if got.Models == nil {
		t.Error("models 必须是数组，不能是 null")
	}
	if got.Notes == nil {
		t.Error("notes 必须是数组，不能是 null")
	}
}

// --- 契约 3：代理鉴权 + 空 content 当错误 ---

// cuaEmptyBackend 是个永远返回空内容的假后端，用来验证「空 content 必须当错误」。
type cuaEmptyBackendModel struct{}

var _ einomodel.BaseChatModel = (*cuaEmptyBackendModel)(nil)

func (m *cuaEmptyBackendModel) Generate(_ context.Context, _ []*einoschema.Message, _ ...einomodel.Option) (*einoschema.Message, error) {
	return &einoschema.Message{Role: einoschema.Assistant, Content: ""}, nil
}

func (m *cuaEmptyBackendModel) Stream(ctx context.Context, _ []*einoschema.Message, _ ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	reader, writer := einoschema.Pipe[*einoschema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(&einoschema.Message{Role: einoschema.Assistant, Content: ""}, nil)
	}()
	return reader, nil
}

func cuaEmptyBackend(_ context.Context, _ llm.Endpoint, _ string, _ llm.ChatRequest) (einomodel.BaseChatModel, error) {
	return &cuaEmptyBackendModel{}, nil
}

func TestCuaLLMProxyAuth(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()

	body := `{"model":"mock-echo","messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct{ name, auth string }{
		{"无 token", ""},
		{"非 Bearer", "Token abc"},
		{"token 无效", "Bearer deadbeef"},
	} {
		w := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions", body, tc.auth)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s：状态码 = %d，想要 401", tc.name, w.Code)
		}
		var got struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error.Message == "" {
			t.Errorf("%s：401 body 形状不对：%s", tc.name, w.Body.String())
		}
	}
}

func TestCuaLLMProxyHappyPathWithMockBackend(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()

	run := newCUARun(cuaNewID(), "u1", "t", "mock-echo", "tok-happy", true)
	if !cuaMgr.reserve(run) {
		t.Fatal("reserve 失败")
	}
	// mock 端点不需要 key；这里替换掉 resolveKey 的 DB 依赖。
	old := cuaResolveKey
	cuaResolveKey = func(string, llm.Endpoint) string { return "test-key" }
	defer func() { cuaResolveKey = old }()

	// 纯文本请求：mock 后端回显最后一条 user 消息的 Content。
	payload := `{"model":"mock-echo","messages":[{"role":"user","content":"你好"}]}`
	w := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions", payload, "Bearer tok-happy")
	if w.Code != http.StatusOK {
		t.Fatalf("代理状态码 = %d，body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Role != "assistant" {
		t.Fatalf("响应形状不符契约 3：%s", w.Body.String())
	}
	// mock 后端回显最后一条 user 消息的 Content，说明消息确实到了 registry。
	if !strings.Contains(got.Choices[0].Message.Content, "你好") {
		t.Errorf("回显里没有文字部分：%q", got.Choices[0].Message.Content)
	}

	// 带图片的请求也必须走通：图片走 multi-content（此时 Content 留空），
	// mock 后端读不到 Content，所以这里只断言形状合法；
	// data URI → llm.Image{MIME,Data} 的映射由 cuaConvertMessages 与 8300 黑盒桩测试覆盖。
	imgPayload := `{"model":"mock-echo","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + cuaTestPNGB64 + `"}}]}]}`
	iw := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions", imgPayload, "Bearer tok-happy")
	if iw.Code != http.StatusOK {
		t.Fatalf("带图片的请求应该 200，实际 %d %s", iw.Code, iw.Body.String())
	}

	// 未知模型 → 400，且 body 形状是 {"error":{"message":...}}。
	badModel := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions",
		`{"model":"no-such","messages":[{"role":"user","content":"hi"}]}`, "Bearer tok-happy")
	if badModel.Code != http.StatusBadRequest {
		t.Errorf("未知模型状态码 = %d，想要 400", badModel.Code)
	}
}

func TestCuaLLMProxyEmptyContentIsError(t *testing.T) {
	cuaTestSetup()
	r := cuaTestRouter()

	run := newCUARun(cuaNewID(), "u1", "t", "empty-model", "tok-empty", true)
	if !cuaMgr.reserve(run) {
		t.Fatal("reserve 失败")
	}
	old := cuaResolveKey
	cuaResolveKey = func(string, llm.Endpoint) string { return "test-key" }
	defer func() { cuaResolveKey = old }()

	w := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions",
		`{"model":"empty-model","messages":[{"role":"user","content":"hi"}]}`, "Bearer tok-empty")
	if w.Code == http.StatusOK {
		t.Fatalf("空 content 绝不能当合法回复返回：%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "E_MODEL_EMPTY") {
		t.Errorf("空回复错误应带 E_MODEL_EMPTY 标记：%s", w.Body.String())
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error.Message == "" {
		t.Errorf("错误 body 形状不对：%s", w.Body.String())
	}
}

// --- 契约 3：上游报错必须带出真实原因 ---

// TestCuaLLMProxySurfacesUpstreamReason 用返回 400 + OpenAI 错误体的假上游，
// 断言代理把上游真实原因原样带出来（不得把上下文超限说成 key/模型名问题）。
func TestCuaLLMProxySurfacesUpstreamReason(t *testing.T) {
	cuaTestSetup()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	reg := llm.NewRegistry()
	reg.SetEndpoints([]llm.Endpoint{{
		ID: "stub", Label: "假上游", Backend: "openai",
		BaseURL: upstream.URL + "/v1", Models: []string{"stub-vision"},
	}})
	reg.Register("openai", llm.NewOpenAIBackend)
	registry = reg

	run := newCUARun(cuaNewID(), "u1", "t", "stub-vision", "tok-err", true)
	if !cuaMgr.reserve(run) {
		t.Fatal("reserve 失败")
	}
	old := cuaResolveKey
	cuaResolveKey = func(string, llm.Endpoint) string { return "test-key" }
	defer func() { cuaResolveKey = old }()

	w := cuaDo(cuaTestRouter(), http.MethodPost, `/internal/cua/llm/v1/chat/completions`,
		`{"model":"stub-vision","messages":[{"role":"user","content":"hi"}]}`, "Bearer tok-err")
	body := w.Body.String()
	if !strings.Contains(body, "maximum context length") {
		t.Fatalf("上游真实原因没带出来：%d %s", w.Code, body)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("上游 400 应原样透出 400，实际 %d（body=%s）", w.Code, body)
	}
}

// --- 契约 2.3：历史截图保留上限 + 空列表形状 ---

func TestCuaFrameImageHistoryCap(t *testing.T) {
	run := newCUARun(cuaNewID(), "u1", "t", "m", "tok", true)
	// 15 帧截图 + 15 条 step：历史里只该留 10 帧，step 一条都不能少。
	for i := 0; i < 15; i++ {
		run.emit("step", []byte("s"))
		run.emit(cuaFrameImageEvent, []byte{byte(48 + i/10), byte(48 + i%10)})
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	imgs, steps := 0, 0
	for _, ev := range run.events {
		switch ev.Name {
		case cuaFrameImageEvent:
			imgs++
		case "step":
			steps++
		}
	}
	if imgs != cuaMaxFrameImages {
		t.Errorf("历史截图 = %d，想要 %d", imgs, cuaMaxFrameImages)
	}
	if steps != 15 {
		t.Errorf("非图片事件被误删：step = %d，想要 15", steps)
	}
	if len(run.imageSlots) != cuaMaxFrameImages {
		t.Errorf("imageSlots = %d，想要 %d", len(run.imageSlots), cuaMaxFrameImages)
	}
	for _, idx := range run.imageSlots {
		if idx < 0 || idx >= len(run.events) || run.events[idx].Name != cuaFrameImageEvent {
			t.Fatalf("imageSlots 下标错位：%d", idx)
		}
	}
	last := run.imageSlots[len(run.imageSlots)-1]
	if string(run.events[last].Data) != "14" {
		t.Errorf("留下的应该是最新一帧 14，实际 %q", run.events[last].Data)
	}
}

// TestCuaListRunsEmptyIsArray 空历史必须是 {"runs":[]} 而不是 null（前端空态）。
func TestCuaListRunsEmptyIsArray(t *testing.T) {
	cuaTestSetup()
	w := cuaDo(cuaTestRouter(), http.MethodGet, "/api/cua/runs", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"runs":[]}` {
		t.Errorf("空列表形状 = %s，想要空数组", got)
	}
}

// --- 契约 5：sidecar 优先 / python 回落 ---

// TestCuaSidecarResolution 覆盖打包产物与开发态的取舍逻辑：
// 有 bin/cua-sidecar.exe 就用它，没有就回落 python -m cua.main。
func TestCuaSidecarResolution(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-sidecar.exe")
	if err := os.WriteFile(fake, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1) CUA_SIDECAR 指到真实存在的文件 → 用它。
	t.Setenv("CUA_SIDECAR", fake)
	if got := cuaSidecarPath(); got != fake {
		t.Errorf("CUA_SIDECAR 生效时 = %q，想要 %q", got, fake)
	}
	cmd := cuaCommand([]string{"--task", "x"})
	if cmd.Path != fake {
		t.Errorf("命令路径 = %q，想要 %q", cmd.Path, fake)
	}
	// 2) CUA_SIDECAR 不存在 + 工作目录没有 bin/ → 回落 python -m cua.main。
	t.Setenv("CUA_SIDECAR", filepath.Join(dir, "nope.exe"))
	t.Setenv("CUA_PYTHON", "py-test")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	if got := cuaSidecarPath(); got != "" {
		t.Errorf("没有 sidecar 时应返回空串，实际 %q", got)
	}
	cmd = cuaCommand([]string{"--task", "x"})
	if filepath.Base(cmd.Path) != "py-test" {
		t.Errorf("回落命令 = %q，想要 py-test", cmd.Path)
	}
	if len(cmd.Args) < 3 || cmd.Args[1] != "-m" || cmd.Args[2] != "cua.main" {
		t.Errorf("回落参数不对：%v", cmd.Args)
	}
}

// --- 契约 3.1：max_tokens 下限（2026-10-11 二次裁定：4096 -> 16384）---

// cuaRecordBackend 记录代理最终发给后端的 max_tokens，供下限行为测试断言。
var cuaRecordedMaxTokens int

type cuaRecordBackendModel struct{}

var _ einomodel.BaseChatModel = (*cuaRecordBackendModel)(nil)

func (m *cuaRecordBackendModel) Generate(_ context.Context, _ []*einoschema.Message, _ ...einomodel.Option) (*einoschema.Message, error) {
	return &einoschema.Message{Role: einoschema.Assistant, Content: "ok"}, nil
}

func (m *cuaRecordBackendModel) Stream(_ context.Context, _ []*einoschema.Message, _ ...einomodel.Option) (*einoschema.StreamReader[*einoschema.Message], error) {
	reader, writer := einoschema.Pipe[*einoschema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(&einoschema.Message{Role: einoschema.Assistant, Content: "ok"}, nil)
	}()
	return reader, nil
}

func cuaRecordBackend(_ context.Context, _ llm.Endpoint, _ string, req llm.ChatRequest) (einomodel.BaseChatModel, error) {
	cuaRecordedMaxTokens = req.MaxTokens
	return &cuaRecordBackendModel{}, nil
}

func TestCuaMaxTokensFloor(t *testing.T) {
	if cuaMinMaxTokens < 16384 {
		t.Fatalf("契约 3.1（二次裁定）要求 max_tokens 不低于 16384，实际 %d", cuaMinMaxTokens)
	}

	gin.SetMode(gin.TestMode)
	reg := llm.NewRegistry()
	eps := append(llm.DefaultEndpoints(),
		llm.Endpoint{ID: "test-record", Label: "记录 max_tokens", Backend: "record", Models: []string{"record-model"}})
	reg.SetEndpoints(eps)
	reg.Register("record", cuaRecordBackend)
	registry = reg
	cuaMgr = &cuaManager{byToken: map[string]*cuaRun{}}
	r := cuaTestRouter()

	run := newCUARun(cuaNewID(), "u1", "t", "record-model", "tok-max", true)
	if !cuaMgr.reserve(run) {
		t.Fatal("reserve 失败")
	}
	old := cuaResolveKey
	cuaResolveKey = func(string, llm.Endpoint) string { return "test-key" }
	defer func() { cuaResolveKey = old }()

	for _, tc := range []struct {
		name string
		body int
		want int
	}{
		{"未给 max_tokens", 0, cuaMinMaxTokens},
		{"body 过小（1024）", 1024, cuaMinMaxTokens},
		{"body 正好等于下限", cuaMinMaxTokens, cuaMinMaxTokens},
		{"body 更大（32768）", 32768, 32768},
	} {
		cuaRecordedMaxTokens = -1
		payload := `{"model":"record-model","messages":[{"role":"user","content":"hi"}],"max_tokens":` +
			strconv.Itoa(tc.body) + `}`
		w := cuaDo(r, http.MethodPost, "/internal/cua/llm/v1/chat/completions", payload, "Bearer tok-max")
		if w.Code != http.StatusOK {
			t.Fatalf("%s：状态码 = %d，body=%s", tc.name, w.Code, w.Body.String())
		}
		if cuaRecordedMaxTokens != tc.want {
			t.Errorf("%s：实际 max_tokens = %d，想要 %d", tc.name, cuaRecordedMaxTokens, tc.want)
		}
	}
}
