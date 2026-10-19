package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
)

// 搜索入口用多引擎回退：单个入口改版 / 限流 / 403 都会让“搜索没法用”，
// 而这在真实网络里是常态。按顺序试，谁先给出可解析的结果就用谁。
//
// 都只用免密入口（不需要 API Key），保持开箱可用。
var searchEngines = []searchEngine{
	{
		name:  "duckduckgo",
		url:   func(q string) string { return "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q) },
		parse: parseDDGHTML,
	},
	{
		name:  "duckduckgo-lite",
		url:   func(q string) string { return "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(q) },
		parse: parseDDGLite,
	},
	{
		name:  "bing",
		url:   func(q string) string { return "https://www.bing.com/search?q=" + url.QueryEscape(q) },
		parse: parseBing,
	},
}

// searchClient 单独一个 client：搜索卡住不该拖垮整轮对话。
var searchClient = &http.Client{Timeout: 12 * time.Second}

// engineFailures 记录每个入口最后一次失败的时间（抄 Agent Reach 的“体检”思路）。
// 作用不是“永久禁用”，而是**降权**：刚失败过的入口排到最后，
// 避免每次都先等一个已知不可用的入口超时；其他入口都失败时它仍会被重试。
var (
	engineFailMu sync.Mutex
	engineFailAt = map[string]time.Time{}
)

func noteEngineFailure(name string) {
	engineFailMu.Lock()
	engineFailAt[name] = time.Now()
	engineFailMu.Unlock()
}

func clearEngineFailure(name string) {
	engineFailMu.Lock()
	delete(engineFailAt, name)
	engineFailMu.Unlock()
}

// orderEngines 把入口按“最近失败过”排序：没失败过的优先，失败过的按失败时间早的在前。
func orderEngines(engines []searchEngine) []searchEngine {
	engineFailMu.Lock()
	snapshot := make(map[string]time.Time, len(engineFailAt))
	for k, v := range engineFailAt {
		snapshot[k] = v
	}
	engineFailMu.Unlock()

	out := make([]searchEngine, len(engines))
	copy(out, engines)
	sort.SliceStable(out, func(i, j int) bool {
		ti, iFailed := snapshot[out[i].name]
		tj, jFailed := snapshot[out[j].name]
		if iFailed != jFailed {
			return !iFailed // 没失败过的排前面
		}
		if iFailed && jFailed {
			return ti.Before(tj)
		}
		return false
	})
	return out
}

type searchEngine struct {
	name  string
	url   func(query string) string
	parse func(page string, max int) []searchResult
}

// WebSearchTool 联网搜索，返回标题 / 链接 / 摘要。
type WebSearchTool struct{}

func (WebSearchTool) Name() string { return "web_search" }

func (WebSearchTool) Description() string {
	return "联网搜索，返回若干条结果的标题、链接和摘要（不是全文）。" +
		"需要最新的、或你不确定的事实（版本号、新闻、价格、文档地址）时用它，不要凭训练数据猜。" +
		"只有摘要不够、要看某个页面全文时用 web_reader。"
}

func (WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "搜索关键词"},
			"max_results": map[string]any{"type": "integer", "description": "返回多少条（默认 5，最多 10）"},
		},
		"required": []string{"query"},
	}
}

func (WebSearchTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return agent.Result{}, fmt.Errorf("query 不能为空")
	}
	max := a.MaxResults
	if max <= 0 {
		max = 5
	}
	if max > 10 {
		max = 10
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// 逐个入口尝试，把每次的失败原因记下来，最后一次性告知——
	// 模型与用户都需要知道“到底是网络不通，还是被搜索引擎拒了”。
	var failures []string
	for _, eng := range orderEngines(searchEngines) {
		results, err := runSearch(ctx, eng, query, max)
		if err != nil {
			noteEngineFailure(eng.name)
			failures = append(failures, eng.name+": "+err.Error())
			continue
		}
		if len(results) == 0 {
			noteEngineFailure(eng.name)
			failures = append(failures, eng.name+": 没解析出结果（可能被反爬拦截，或入口页面结构变了）")
			continue
		}
		clearEngineFailure(eng.name)

		var b strings.Builder
		fmt.Fprintf(&b, "搜索「%s」的前 %d 条结果（来源：%s）：\n", query, len(results), eng.name)
		for i, r := range results {
			fmt.Fprintf(&b, "\n%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
		}
		return agent.Result{
			Content: b.String(),
			Display: map[string]any{"query": query, "count": len(results), "engine": eng.name},
		}, nil
	}

	return agent.Result{}, fmt.Errorf("所有搜索入口都没成功：%s", strings.Join(failures, "；"))
}

// runSearch 跑单个入口：带超时、带 UA，返回解析后的结果。
func runSearch(ctx context.Context, eng searchEngine, query string, max int) ([]searchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, eng.url(query), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	// 不带 UA 会被部分入口直接拒绝。
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := searchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络不通？）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return eng.parse(string(body), max), nil
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	// DuckDuckGo HTML 版
	ddgAnchorRe  = regexp.MustCompile(`(?is)<a\b[^>]*class="[^"]*result__a[^"]*"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<a\b[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)

	// DuckDuckGo Lite 版：结果在带 class="result-link" 的 <a> 里，摘要在相邻的 td 里。
	liteAnchorRe  = regexp.MustCompile(`(?is)<a\b[^>]*class="[^"]*result-link[^"]*"[^>]*>(.*?)</a>`)
	liteSnippetRe = regexp.MustCompile(`(?is)<td\b[^>]*class="[^"]*result-snippet[^"]*"[^>]*>(.*?)</td>`)

	// Bing：<li class="b_algo"> 里的 <h2><a href>，摘要在 <p> 里。
	bingBlockRe   = regexp.MustCompile(`(?is)<li\b[^>]*class="[^"]*b_algo[^"]*"[^>]*>(.*?)</li>`)
	bingAnchorRe  = regexp.MustCompile(`(?is)<h2[^>]*>\s*<a\b[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	bingSnippetRe = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)

	hrefRe  = regexp.MustCompile(`(?is)href="([^"]+)"`)
	tagRe   = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// parseDDGHTML 解析 html.duckduckgo.com 的结果页。
func parseDDGHTML(page string, max int) []searchResult {
	anchors := ddgAnchorRe.FindAllStringSubmatch(page, -1)
	snippets := ddgSnippetRe.FindAllStringSubmatch(page, -1)

	out := make([]searchResult, 0, len(anchors))
	for i, m := range anchors {
		if len(out) >= max {
			break
		}
		title := cleanText(m[1])
		href := ""
		if h := hrefRe.FindStringSubmatch(m[0]); len(h) == 2 {
			href = normalizeResultURL(h[1])
		}
		if title == "" || href == "" {
			continue
		}
		snippet := ""
		if i < len(snippets) {
			snippet = cleanText(snippets[i][1])
		}
		out = append(out, searchResult{Title: title, URL: href, Snippet: snippet})
	}
	return out
}

// parseDDGLite 解析 lite.duckduckgo.com 的结果页（表格布局，没有 result__a 类名）。
func parseDDGLite(page string, max int) []searchResult {
	anchors := liteAnchorRe.FindAllStringSubmatch(page, -1)
	snippets := liteSnippetRe.FindAllStringSubmatch(page, -1)

	out := make([]searchResult, 0, len(anchors))
	for i, m := range anchors {
		if len(out) >= max {
			break
		}
		title := cleanText(m[1])
		href := ""
		if h := hrefRe.FindStringSubmatch(m[0]); len(h) == 2 {
			href = normalizeResultURL(h[1])
		}
		if title == "" || href == "" {
			continue
		}
		snippet := ""
		if i < len(snippets) {
			snippet = cleanText(snippets[i][1])
		}
		out = append(out, searchResult{Title: title, URL: href, Snippet: snippet})
	}
	return out
}

// parseBing 解析 bing.com 的结果页。
func parseBing(page string, max int) []searchResult {
	blocks := bingBlockRe.FindAllStringSubmatch(page, -1)
	out := make([]searchResult, 0, len(blocks))
	for _, block := range blocks {
		if len(out) >= max {
			break
		}
		body := block[1]
		a := bingAnchorRe.FindStringSubmatch(body)
		if len(a) != 3 {
			continue
		}
		title := cleanText(a[2])
		href := normalizeResultURL(a[1])
		if title == "" || href == "" {
			continue
		}
		snippet := ""
		if s := bingSnippetRe.FindStringSubmatch(body); len(s) == 2 {
			snippet = cleanText(s[1])
		}
		out = append(out, searchResult{Title: title, URL: href, Snippet: snippet})
	}
	return out
}

// normalizeResultURL 把跳转链接还原成真实地址。
func normalizeResultURL(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if strings.Contains(href, "uddg=") {
		if u, err := url.Parse(href); err == nil {
			if real := u.Query().Get("uddg"); real != "" {
				return real
			}
		}
	}
	return href
}

func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}
