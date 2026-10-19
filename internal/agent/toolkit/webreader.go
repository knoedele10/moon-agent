package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"blog-my/internal/agent"
)

// allowPrivateHosts 默认拒绝本机/内网地址：模型可能被网页内容诱导去读内网服务（SSRF）。
// 测试里把它打开，好让 httptest 的 127.0.0.1 能通过。
var allowPrivateHosts = false

// jinaReaderBase 是备用阅读器入口（Jina Reader，免费无需 Key，返回 Markdown）。
// 声明成变量是为了单测能把它指向 httptest。
//
// 抄自 Agent Reach 的思路：**一个入口不够**。直连拿不到（403/超时/反爬）时换一条路，
// 比直接告诉用户“抓取失败”有用得多。
var jinaReaderBase = "https://r.jina.ai/"

// webReaderClient 单独的超时：抓网页比搜索更容易挂住。
var webReaderClient = &http.Client{Timeout: 15 * time.Second}

// WebReaderTool 抓取网页正文。
type WebReaderTool struct{}

func (WebReaderTool) Name() string { return "web_reader" }

func (WebReaderTool) Description() string {
	return "抓取一个网页并返回正文文本（会去掉脚本、样式和标签）。" +
		"web_search 只给摘要，需要看全文时用它；只支持 http/https 公网地址，不抓本机与内网。" +
		"直连被反爬拦截时会自动改用备用阅读器。"
}

func (WebReaderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":       map[string]any{"type": "string", "description": "要抓取的完整网址（http/https）"},
			"max_chars": map[string]any{"type": "integer", "description": "最多返回多少字符（默认 4000，最多 20000）"},
		},
		"required": []string{"url"},
	}
}

func (WebReaderTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	target := strings.TrimSpace(a.URL)
	if target == "" {
		return agent.Result{}, fmt.Errorf("url 不能为空")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return agent.Result{}, fmt.Errorf("只支持完整的 http/https 网址，收到 %q", target)
	}
	if !allowPrivateHosts && isPrivateHost(u.Hostname()) {
		return agent.Result{}, fmt.Errorf("拒绝抓取本机或内网地址（%s）", u.Hostname())
	}

	maxChars := a.MaxChars
	if maxChars <= 0 {
		maxChars = 4000
	}
	if maxChars > 20000 {
		maxChars = 20000
	}

	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// 第一路：直连抓取。
	text, directErr := directFetch(reqCtx, u)

	// 第二路：直连失败，或抓回来的是反爬验证页 → 换备用阅读器。
	usedFallback := false
	if directErr != nil || looksLikeChallenge(text) {
		jina, jerr := fetchViaJina(reqCtx, u.String())
		switch {
		case jerr == nil && strings.TrimSpace(jina) != "" && !looksLikeChallenge(jina):
			text = strings.TrimSpace(jina)
			usedFallback = true
		case directErr != nil:
			return agent.Result{}, fmt.Errorf("抓取失败：%v；备用阅读器也没成功：%v", directErr, jerr)
		default:
			// 直连拿到的是验证页，备用阅读器也没救回来。
			return agent.Result{}, fmt.Errorf("目标站点返回了反爬验证页，未取到正文（建议改用该站点的专用工具，或用电脑操控打开浏览器）")
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return agent.Result{}, fmt.Errorf("页面里没有抓到可读文本")
	}

	truncated := false
	if len([]rune(text)) > maxChars {
		text = string([]rune(text)[:maxChars])
		truncated = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "来源：%s\n\n%s", u.String(), text)
	if truncated {
		fmt.Fprintf(&b, "\n\n...（已截断，只显示前 %d 个字符）", maxChars)
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{"url": u.String(), "chars": len([]rune(text)), "truncated": truncated, "fallback": usedFallback},
	}, nil
}

// directFetch 直连目标站点并把 HTML 转成文本。
func directFetch(ctx context.Context, u *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := webReaderClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("网络不可用或站点无响应: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("目标返回 HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") || strings.HasPrefix(strings.TrimSpace(text), "<") {
		text = htmlToText(text)
	}
	return strings.TrimSpace(text), nil
}

// fetchViaJina 走 Jina Reader 取 Markdown 正文。
// 它对我们来说是**纯 HTTP 调用**，不需要装任何东西，也不会把供应链带进来。
func fetchViaJina(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaReaderBase+target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; blog-my-agent/1.0)")
	req.Header.Set("Accept", "text/plain")

	resp, err := webReaderClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("备用阅读器返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// challengeMarkers 是反爬/验证页的特征串（抄自 Agent Reach 的 web 渠道）。
//
// 为什么必须识别：把 Cloudflare 的「Just a moment...」当成网页正文喂给模型，
// 模型会一本正经地总结这个验证页——比直接报错糟糕得多。
var challengeMarkers = []string{
	"just a moment...",
	"performing security verification",
	"attention required! | cloudflare",
	"enable javascript and cookies to continue",
	"cdn-cgi/challenge-platform",
	"checking your browser before accessing",
	"verifying you are human",
	"正在验证您是否是真人",
	"请开启 javascript 并刷新此页面",
}

// looksLikeChallenge 判断抓回来的内容是不是反爬验证页。
// 只在开头一小段里找特征：正文里偶尔提到这些词（比如讲爬虫的文章）不该被误判。
func looksLikeChallenge(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	sample := text
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	lower := strings.ToLower(sample)
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	// 双条件：出现 ray id 且带 challenge 路径，才算 Cloudflare 拦截页。
	if strings.Contains(lower, "ray id") && strings.Contains(lower, "/cdn-cgi/challenge-platform/") {
		return true
	}
	return false
}

// isPrivateHost 判断主机名是否指向本机 / 内网 / 链路本地地址。
func isPrivateHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false // 普通域名：交给 DNS 与网络层
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	blockTagRe    = regexp.MustCompile(`(?is)</?(p|div|br|li|tr|h[1-6]|section|article|header|footer)[^>]*>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRunRe    = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankRunRe    = regexp.MustCompile(`\n{3,}`)
)

// htmlToText 把 HTML 粗略转成可读文本：块级标签转换行，其余标签删掉，实体反转义。
func htmlToText(page string) string {
	s := scriptStyleRe.ReplaceAllString(page, " ")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spaceRunRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return blankRunRe.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
}
