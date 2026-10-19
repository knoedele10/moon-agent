package toolkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
)

func execTool(t *testing.T, tool agent.Tool, args map[string]any) agent.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), raw, &agent.Exec{SessionID: "t"})
	if err != nil {
		t.Fatalf("%s 执行失败: %v", tool.Name(), err)
	}
	return res
}

// ---------- calculator ----------

func TestCalculatorEvaluates(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"(1+2)*3", "9"},
		{"2^10", "1024"},
		{"10%3", "1"},
		{"-4+6", "2"},
		{"1.5*4", "6"},
		{"sqrt(16)", "4"},
		{"min(3, 1, 2)", "1"},
		{"2*pi", "6.283185307179586"},
	}
	for _, c := range cases {
		v, err := evalExpression(c.expr)
		if err != nil {
			t.Fatalf("%s 应当可算: %v", c.expr, err)
		}
		if got := formatNumber(v); got != c.want {
			t.Fatalf("%s = %s，期望 %s", c.expr, got, c.want)
		}
	}
}

func TestCalculatorRejectsBadInput(t *testing.T) {
	for _, expr := range []string{"1/0", "2+*3", "sqrt(-1)", "(1+2", "foo(3)", ""} {
		if _, err := evalExpression(expr); err == nil {
			t.Fatalf("%q 应当报错", expr)
		}
	}
}

func TestCalculatorToolContent(t *testing.T) {
	res := execTool(t, CalculatorTool{}, map[string]any{"expression": "(1+2)*3"})
	if !strings.Contains(res.Content, "= 9") {
		t.Fatalf("结果文本应当包含 = 9: %q", res.Content)
	}
}

// ---------- datetime ----------

func TestDateTimeNowWithTimezone(t *testing.T) {
	res := execTool(t, DateTimeTool{}, map[string]any{"action": "now", "timezone": "Asia/Shanghai"})
	if !strings.Contains(res.Content, "当前时间") || !strings.Contains(res.Content, "Asia/Shanghai") {
		t.Fatalf("now 应当返回当前时间与时区: %q", res.Content)
	}
	if !strings.Contains(res.Content, "星期") {
		t.Fatalf("now 应当带星期: %q", res.Content)
	}
}

func TestDateTimeAddDays(t *testing.T) {
	res := execTool(t, DateTimeTool{}, map[string]any{
		"action": "add", "datetime": "2026-01-01", "amount": 3, "unit": "days", "timezone": "UTC",
	})
	if !strings.Contains(res.Content, "2026-01-04") {
		t.Fatalf("3 天后应当是 2026-01-04: %q", res.Content)
	}
}

func TestDateTimeDiff(t *testing.T) {
	res := execTool(t, DateTimeTool{}, map[string]any{
		"action": "diff", "datetime": "2026-01-01", "to": "2026-01-03", "timezone": "UTC",
	})
	if !strings.Contains(res.Content, "2 天") {
		t.Fatalf("相差应当为 2 天: %q", res.Content)
	}
}

func TestDateTimeRejectsUnknownTimezone(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"action": "now", "timezone": "Not/AZone"})
	if _, err := (DateTimeTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
		t.Fatal("未知时区应当报错")
	}
}

// ---------- web_search ----------

const searchFixture = `<html><body>
<div class="result">
  <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&amp;rut=1">Example <b>A</b></a>
  <a class="result__snippet">First &amp; snippet</a>
</div>
<div class="result">
  <a class="result__a" rel="nofollow" href="https://example.org/b">Example B</a>
  <a class="result__snippet">Second snippet</a>
</div>
</body></html>`

func TestParseSearchResults(t *testing.T) {
	got := parseDDGHTML(searchFixture, 5)
	if len(got) != 2 {
		t.Fatalf("应当解析出 2 条，实际 %d", len(got))
	}
	if got[0].URL != "https://example.com/a" {
		t.Fatalf("跳转链接应当还原成真实地址: %q", got[0].URL)
	}
	if got[0].Title != "Example A" {
		t.Fatalf("标题应当去掉标签并反转义: %q", got[0].Title)
	}
	if got[0].Snippet != "First & snippet" {
		t.Fatalf("摘要应当反转义: %q", got[0].Snippet)
	}
	if got[1].URL != "https://example.org/b" {
		t.Fatalf("直链应当原样保留: %q", got[1].URL)
	}
}

func TestParseSearchResultsRespectsMax(t *testing.T) {
	if got := parseDDGHTML(searchFixture, 1); len(got) != 1 {
		t.Fatalf("max=1 时应当只返回 1 条，实际 %d", len(got))
	}
}

// 用 httptest 顶替搜索入口：验证 Execute 的取数、解析、拼文本整条链路（不碰真实网络）。
func TestWebSearchToolAgainstStubEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			t.Errorf("请求里应当带 q 参数")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(searchFixture))
	}))
	defer srv.Close()

	// 把多引擎列表临时换成一个指向 httptest 的入口，验证取数→解析→拼文本整条链路。
	old := searchEngines
	searchEngines = []searchEngine{{
		name:  "stub",
		url:   func(q string) string { return srv.URL + "/?q=" + url.QueryEscape(q) },
		parse: parseDDGHTML,
	}}
	defer func() { searchEngines = old }()

	res := execTool(t, WebSearchTool{}, map[string]any{"query": "golang", "max_results": 2})
	if !strings.Contains(res.Content, "https://example.com/a") {
		t.Fatalf("结果里应当含解析出的链接: %q", res.Content)
	}
	if !strings.Contains(res.Content, "First & snippet") {
		t.Fatalf("结果里应当含摘要: %q", res.Content)
	}
}

// DuckDuckGo Lite 是表格布局，类名与 HTML 版完全不同：这里钉住它也能解析。
func TestParseDDGLite(t *testing.T) {
	page := `<html><body><table>` +
		`<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Flite.example%2Fa">Lite A</a></td></tr>` +
		`<tr><td class="result-snippet">Lite snippet</td></tr>` +
		`</table></body></html>`
	got := parseDDGLite(page, 5)
	if len(got) != 1 {
		t.Fatalf("应当解析出 1 条，实际 %d", len(got))
	}
	if got[0].URL != "https://lite.example/a" || got[0].Title != "Lite A" || got[0].Snippet != "Lite snippet" {
		t.Fatalf("解析结果不对: %+v", got[0])
	}
}

// Bing 是第三个回退入口：结构又是另一套（b_algo + h2 + p）。
func TestParseBing(t *testing.T) {
	page := `<html><body><ol>` +
		`<li class="b_algo"><h2><a href="https://bing.example/a">Bing <strong>A</strong></a></h2><p>Bing snippet</p></li>` +
		`</ol></body></html>`
	got := parseBing(page, 5)
	if len(got) != 1 {
		t.Fatalf("应当解析出 1 条，实际 %d", len(got))
	}
	if got[0].URL != "https://bing.example/a" || got[0].Title != "Bing A" || got[0].Snippet != "Bing snippet" {
		t.Fatalf("解析结果不对: %+v", got[0])
	}
}

// 三个入口全失败时，错误信息要带上每个入口的原因，而不是一句笼统的“搜索失败”。
func TestWebSearchAllEnginesFailReportsReasons(t *testing.T) {
	old := searchEngines
	searchEngines = []searchEngine{
		{name: "eng-a", url: func(string) string { return "http://127.0.0.1:1/" }, parse: parseDDGHTML},
		{name: "eng-b", url: func(string) string { return "http://127.0.0.1:1/" }, parse: parseDDGHTML},
	}
	defer func() { searchEngines = old }()

	raw, _ := json.Marshal(map[string]any{"query": "x"})
	_, err := (WebSearchTool{}).Execute(context.Background(), raw, &agent.Exec{})
	if err == nil {
		t.Fatal("全部失败时应当报错")
	}
	for _, want := range []string{"eng-a", "eng-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应当包含入口名 %q: %v", want, err)
		}
	}
}

func TestWebSearchRejectsEmptyQuery(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"query": "  "})
	if _, err := (WebSearchTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
		t.Fatal("空 query 应当报错")
	}
}

// ---------- web_reader ----------

const readerFixture = `<html><head><title>T</title><style>body{color:red}</style>` +
	`<script>var secret = 1;</script></head><body><h1>标题</h1><p>第一段 &amp; 更多</p><p>第二段</p></body></html>`

func TestHtmlToTextStripsScriptAndStyle(t *testing.T) {
	got := htmlToText(readerFixture)
	if strings.Contains(got, "secret") || strings.Contains(got, "color:red") {
		t.Fatalf("脚本/样式不该出现在正文里: %q", got)
	}
	if !strings.Contains(got, "第一段 & 更多") || !strings.Contains(got, "第二段") {
		t.Fatalf("正文应当保留并反转义: %q", got)
	}
}

func TestWebReaderFetchesAndExtracts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(readerFixture))
	}))
	defer srv.Close()

	old := allowPrivateHosts
	allowPrivateHosts = true // httptest 跑在 127.0.0.1 上
	defer func() { allowPrivateHosts = old }()

	res := execTool(t, WebReaderTool{}, map[string]any{"url": srv.URL})
	if !strings.Contains(res.Content, "第一段 & 更多") {
		t.Fatalf("应当返回网页正文: %q", res.Content)
	}
}

func TestWebReaderRejectsBadURLAndPrivateHost(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "not-a-url", ""} {
		raw, _ := json.Marshal(map[string]any{"url": u})
		if _, err := (WebReaderTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
			t.Fatalf("%q 应当被拒", u)
		}
	}

	// 默认不允许抓本机/内网（防 SSRF）。
	if allowPrivateHosts {
		t.Fatal("测试前提：默认应当禁止内网地址")
	}
	raw, _ := json.Marshal(map[string]any{"url": "http://127.0.0.1:1/"})
	if _, err := (WebReaderTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
		t.Fatal("内网地址应当被拒")
	}
}

// 反爬验证页必须被识别出来：把 Cloudflare 的“Just a moment...”当正文喂给模型，
// 模型会一本正经地总结这个验证页——比直接报错糟糕得多。
func TestLooksLikeChallenge(t *testing.T) {
	challenge := `<html><head><title>Just a moment...</title></head>` +
		`<body>performing security verification</body></html>`
	if !looksLikeChallenge(challenge) {
		t.Fatal("应当识别为反爬验证页")
	}
	if looksLikeChallenge("") {
		t.Fatal("空文本不该被判为验证页")
	}
	// 正常文章提到“Cloudflare”“验证码”不该被误判（特征串是英文整句）。
	article := "这篇文章讲了如何处理 Cloudflare 与验证码，以及常见的反爬手段。" + strings.Repeat("正文内容。", 50)
	if looksLikeChallenge(article) {
		t.Fatal("正常文章不该被误判为验证页")
	}
}

// 直连失败时自动改用备用阅读器（抄 Agent Reach 的多入口思路）。
func TestWebReaderFallsBackToJina(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // 模拟被站点拒绝
	}))
	defer direct.Close()

	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("备用阅读器取回的正文"))
	}))
	defer jina.Close()

	oldAllow, oldBase := allowPrivateHosts, jinaReaderBase
	allowPrivateHosts = true
	jinaReaderBase = jina.URL + "/"
	defer func() { allowPrivateHosts = oldAllow; jinaReaderBase = oldBase }()

	res := execTool(t, WebReaderTool{}, map[string]any{"url": direct.URL})
	if !strings.Contains(res.Content, "备用阅读器取回的正文") {
		t.Fatalf("应当回退到备用阅读器: %q", res.Content)
	}
	if res.Display["fallback"] != true {
		t.Fatalf("应当标记 fallback=true: %v", res.Display)
	}
}

// 失败过的入口要被降权（排到最后），但不会被永久禁用。
func TestOrderEnginesDeprioritizesFailed(t *testing.T) {
	old := engineFailAt
	engineFailAt = map[string]time.Time{}
	defer func() { engineFailAt = old }()

	engines := []searchEngine{{name: "a"}, {name: "b"}}
	noteEngineFailure("a")

	got := orderEngines(engines)
	if len(got) != 2 || got[0].name != "b" || got[1].name != "a" {
		t.Fatalf("失败过的入口应当排到最后，实际 %v", []string{got[0].name, got[1].name})
	}

	clearEngineFailure("a")
	got = orderEngines(engines)
	if got[0].name != "a" {
		t.Fatalf("清掉失败记录后应当恢复原顺序，实际 %v", got[0].name)
	}
}

// ---------- ask_user ----------

// ask_user 必须把问题交给上层（AwaitUser），而不是自己编一个答案继续跑。
func TestAskUserReturnsAwaitUser(t *testing.T) {
	res := execTool(t, AskUserTool{}, map[string]any{
		"question": "要删哪个文件？", "options": []string{"a.txt", "b.txt"},
	})
	if res.AwaitUser == "" {
		t.Fatal("AwaitUser 必须非空，否则循环会继续跑下去")
	}
	if !strings.Contains(res.AwaitUser, "要删哪个文件？") || !strings.Contains(res.AwaitUser, "a.txt") {
		t.Fatalf("问题与选项都应当带上: %q", res.AwaitUser)
	}
	if res.IsError {
		t.Fatal("提问不是错误")
	}
}

func TestAskUserRejectsEmptyQuestion(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"question": "  "})
	if _, err := (AskUserTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
		t.Fatal("空问题应当报错")
	}
}

// ---------- json_format ----------

func TestJSONToolPrettyAndMinify(t *testing.T) {
	pretty := execTool(t, JSONTool{}, map[string]any{"json": `{"a":1,"b":[2,3]}`, "mode": "pretty"})
	if !strings.Contains(pretty.Content, "\n  \"a\": 1") {
		t.Fatalf("美化结果不对: %q", pretty.Content)
	}

	min := execTool(t, JSONTool{}, map[string]any{"json": "{\n  \"a\": 1\n}", "mode": "minify"})
	if min.Content != `{"a":1}` {
		t.Fatalf("压缩结果不对: %q", min.Content)
	}

	valid := execTool(t, JSONTool{}, map[string]any{"json": "[1,2]", "mode": "validate"})
	if !strings.Contains(valid.Content, "合法") {
		t.Fatalf("校验结果不对: %q", valid.Content)
	}
}

func TestJSONToolRejectsInvalid(t *testing.T) {
	for _, bad := range []string{`{"a":1,}`, `{a:1}`, `{"a":1} extra`, ""} {
		raw, _ := json.Marshal(map[string]any{"json": bad})
		if _, err := (JSONTool{}).Execute(context.Background(), raw, &agent.Exec{}); err == nil {
			t.Fatalf("%q 应当被判为非法", bad)
		}
	}
}

// ---------- sql_query ----------

// 只读白名单是硬约束：任何写操作 / DDL / 多语句 / 注释隐藏关键字都必须被拒。
func TestSQLGuardRejectsWrites(t *testing.T) {
	bad := []string{
		"DROP TABLE user",
		"DELETE FROM message",
		"INSERT INTO user VALUES (1)",
		"UPDATE user SET username='x'",
		"SELECT 1; DROP TABLE user",
		"/* hidden */ DROP TABLE user",
		"SELECT * INTO OUTFILE '/tmp/x' FROM user",
		"TRUNCATE TABLE user",
	}
	for _, q := range bad {
		if err := sqlGuard(q); err == nil {
			t.Fatalf("%q 应当被拒绝", q)
		}
	}
}

func TestSQLGuardAcceptsReads(t *testing.T) {
	good := []string{
		"SELECT 1",
		"select id, username from user limit 5;",
		"SHOW TABLES",
		"DESCRIBE user",
		"EXPLAIN SELECT 1",
		"WITH t AS (SELECT 1 AS a) SELECT * FROM t",
		"SELECT 1 -- 注释",
	}
	for _, q := range good {
		if err := sqlGuard(q); err != nil {
			t.Fatalf("%q 应当被允许: %v", q, err)
		}
	}
}

func TestSQLToolWithoutDBReportsClearly(t *testing.T) {
	old := sqlDB
	sqlDB = nil
	defer func() { sqlDB = old }()

	raw, _ := json.Marshal(map[string]any{"sql": "SELECT 1"})
	_, err := (SQLTool{}).Execute(context.Background(), raw, &agent.Exec{})
	if err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("未注入数据库时应当明确报“未配置”: %v", err)
	}
}
