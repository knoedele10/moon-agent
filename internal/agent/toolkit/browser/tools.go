package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blog-my/internal/agent"
)

// maxSnapshotText 限制快照正文长度：模型需要的是「页面在说什么」，
// 不是整页 DOM；更长内容让它用 browser_network 或页面内的链接继续深入。
const maxSnapshotText = 4000

// maxSnapshotNodes 限制返回的可交互元素数量。
const maxSnapshotNodes = 60

// snapshotScript 生成页面摘要：标题、地址、可见正文，以及带稳定索引的可交互元素。
// 索引写在 data-moon-idx 上，后续 click/type 直接按索引找，避免让模型猜选择器。
var snapshotScript = fmt.Sprintf(`(() => {
  const out = { title: document.title || '', url: location.href, text: '', elements: [] };
  const body = document.body ? document.body.innerText : '';
  out.text = (body || '').replace(/\n{3,}/g, '\n\n').trim().slice(0, %d);
  const sel = 'a,button,input,textarea,select,[role=button],[role=link],[contenteditable=true]';
  let i = 0;
  for (const el of document.querySelectorAll(sel)) {
    if (i >= %d) break;
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) continue;
    el.setAttribute('data-moon-idx', String(i));
    const type = (el.getAttribute('type') || '').toLowerCase();
    const item = {
      index: i,
      tag: el.tagName.toLowerCase(),
      type: type,
      text: (el.innerText || el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 80),
      placeholder: el.getAttribute('placeholder') || '',
      name: el.getAttribute('name') || ''
    };
    if (type === 'password') item.redacted = true;
    out.elements.push(item);
    i++;
  }
  return JSON.stringify(out);
})()`, maxSnapshotText, maxSnapshotNodes)

type OpenTool struct{}

func (OpenTool) Name() string { return "browser_open" }
func (OpenTool) Description() string {
	return "在用户已登录的 Chrome 里打开一个网页（CDP）。需要登录、点击、填表时用它；" +
		"只抓静态正文用 web_reader。默认拒绝本机/内网地址，需要时配置 AGENT_BROWSER_ALLOW。"
}
func (OpenTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "完整 http/https 网址"},
		},
		"required": []string{"url"},
	}
}
func (OpenTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail("browser_open 参数不是合法 JSON: " + err.Error()), nil
	}
	if _, err := checkURL(a.URL); err != nil {
		return fail(err.Error()), nil
	}
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if _, err := c.call(ctx, "Page.navigate", map[string]any{"url": strings.TrimSpace(a.URL)}); err != nil {
		return fail("打开页面失败：" + err.Error()), nil
	}
	// 等 load 完成：最多 10 秒，超时也把当前状态返回给模型，让它自己判断要不要继续等。
	deadline := time.Now().Add(10 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		if state, err = c.evaluate(ctx, "document.readyState"); err != nil {
			break
		}
		if state == "complete" {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	info, _ := c.evaluate(ctx, "JSON.stringify({title: document.title, url: location.href})")
	return agent.Result{
		Content: "已打开。页面状态：" + state + "\n" + info,
		Display: map[string]any{"url": a.URL, "ready_state": state},
	}, nil
}

type SnapshotTool struct{}

func (SnapshotTool) Name() string { return "browser_snapshot" }
func (SnapshotTool) Description() string {
	return "读取当前页面的标题、正文摘要和可交互元素清单（带 index）。" +
		"点击/填写前先快照，用返回的 index 定位元素比猜 CSS 选择器可靠。密码框内容不会返回。"
}
func (SnapshotTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (SnapshotTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	body, err := c.evaluate(ctx, snapshotScript)
	if err != nil {
		return fail("读取页面失败：" + err.Error()), nil
	}
	var snap struct {
		Title    string `json:"title"`
		URL      string `json:"url"`
		Text     string `json:"text"`
		Elements []struct {
			Index       int    `json:"index"`
			Tag         string `json:"tag"`
			Type        string `json:"type"`
			Text        string `json:"text"`
			Placeholder string `json:"placeholder"`
			Name        string `json:"name"`
			Redacted    bool   `json:"redacted"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		return fail("页面快照无法解析：" + err.Error()), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "标题：%s\n地址：%s\n\n正文摘要：\n%s\n\n可交互元素（点击/填写时用 index）：\n", snap.Title, snap.URL, clipText(snap.Text, maxSnapshotText))
	if len(snap.Elements) == 0 {
		b.WriteString("（没有找到可交互元素）\n")
	}
	for _, el := range snap.Elements {
		if el.Redacted {
			fmt.Fprintf(&b, "[%d] %s type=password（内容已隐藏）\n", el.Index, el.Tag)
			continue
		}
		label := el.Text
		if label == "" {
			label = el.Placeholder
		}
		fmt.Fprintf(&b, "[%d] %s type=%s %q\n", el.Index, el.Tag, el.Type, clipText(label, 80))
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{"url": snap.URL, "title": snap.Title, "elements": len(snap.Elements)},
	}, nil
}

type ClickTool struct{}

func (ClickTool) Name() string { return "browser_click" }
func (ClickTool) Description() string {
	return "点击页面元素：优先用 browser_snapshot 返回的 index，也可以传 CSS selector。需要用户确认。"
}
func (ClickTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"index":    map[string]any{"type": "integer", "description": "快照里的元素 index"},
			"selector": map[string]any{"type": "string", "description": "CSS 选择器（没有 index 时用）"},
		},
	}
}
func (ClickTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Index    *int   `json:"index"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail("browser_click 参数不是合法 JSON: " + err.Error()), nil
	}
	lookup, err := elementLookup(a.Index, a.Selector)
	if err != nil {
		return fail(err.Error()), nil
	}
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	script := `(() => {
  const el = ` + lookup + `;
  if (!el) return 'MISS';
  el.scrollIntoView({ block: 'center', inline: 'center' });
  el.click();
  return 'OK ' + el.tagName.toLowerCase() + ' ' + (el.innerText || '').trim().slice(0, 60);
})()`
	out, err := c.evaluate(ctx, script)
	if err != nil {
		return fail("点击失败：" + err.Error()), nil
	}
	if out == "MISS" {
		return fail("没有找到该元素；先 browser_snapshot 确认 index 或 selector 是否还有效"), nil
	}
	return agent.Result{Content: "已点击：" + out, Display: map[string]any{"result": out}}, nil
}

type TypeTool struct{}

func (TypeTool) Name() string { return "browser_type" }
func (TypeTool) Description() string {
	return "往输入框填写文本（可顺便提交表单）。优先用 index。拒绝填写密码、支付卡和验证码字段；需要用户确认。"
}
func (TypeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"index":    map[string]any{"type": "integer", "description": "快照里的元素 index"},
			"selector": map[string]any{"type": "string", "description": "CSS 选择器（没有 index 时用）"},
			"text":     map[string]any{"type": "string", "description": "要填的文本"},
			"submit":   map[string]any{"type": "boolean", "description": "填完是否提交所在表单，默认 false"},
		},
		"required": []string{"text"},
	}
}
func (TypeTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Index    *int   `json:"index"`
		Selector string `json:"selector"`
		Text     string `json:"text"`
		Submit   bool   `json:"submit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail("browser_type 参数不是合法 JSON: " + err.Error()), nil
	}
	lookup, err := elementLookup(a.Index, a.Selector)
	if err != nil {
		return fail(err.Error()), nil
	}
	encoded, err := json.Marshal(a.Text)
	if err != nil {
		return fail("文本无法编码"), nil
	}
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	script := `(() => {
  const el = ` + lookup + `;
  if (!el) return 'MISS';
  const type = (el.getAttribute('type') || '').toLowerCase();
  const ac = (el.getAttribute('autocomplete') || '').toLowerCase();
  if (type === 'password' || ac.includes('password') || ac.includes('one-time-code') || ac.includes('cc-number')) {
    return 'SENSITIVE';
  }
  el.scrollIntoView({ block: 'center' });
  el.focus();
  const proto = el.constructor && el.constructor.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, 'value');
  if (setter && setter.set) { setter.set.call(el, ` + string(encoded) + `); } else { el.value = ` + string(encoded) + `; }
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return 'OK';
})()`
	out, err := c.evaluate(ctx, script)
	if err != nil {
		return fail("填写失败：" + err.Error()), nil
	}
	switch out {
	case "MISS":
		return fail("没有找到该输入框；先 browser_snapshot 确认 index 或 selector"), nil
	case "SENSITIVE":
		return fail("拒绝填写密码/支付/验证码字段：这类凭据只能由用户自己输入"), nil
	}
	if a.Submit {
		submitScript := `(() => {
  const el = ` + lookup + `;
  const form = el && el.form;
  if (!form) return 'NO_FORM';
  form.requestSubmit();
  return 'SUBMITTED';
})()`
		if res, err := c.evaluate(ctx, submitScript); err == nil && res == "SUBMITTED" {
			return agent.Result{Content: "已填写并提交表单。", Display: map[string]any{"submitted": true}}, nil
		}
		return agent.Result{Content: "已填写；没找到可提交的表单，请用 browser_click 点提交按钮。", Display: map[string]any{"submitted": false}}, nil
	}
	return agent.Result{Content: "已填写。", Display: map[string]any{"submitted": false}}, nil
}

type ScreenshotTool struct{}

func (ScreenshotTool) Name() string { return "browser_screenshot" }
func (ScreenshotTool) Description() string {
	return "截取当前页面（PNG）。不传 path 时存到工作区 .moon/screenshots/ 下；返回图片和文件路径。"
}
func (ScreenshotTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "工作区内的保存路径（可选）"},
		},
	}
}
func (ScreenshotTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail("browser_screenshot 参数不是合法 JSON: " + err.Error()), nil
	}
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	raw2, err := c.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return fail("截图失败：" + err.Error()), nil
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw2, &shot); err != nil || shot.Data == "" {
		return fail("截图返回为空"), nil
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return fail("截图数据无法解码：" + err.Error()), nil
	}
	path, err := screenshotPath(ex, a.Path)
	if err != nil {
		return fail(err.Error()), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail("创建截图目录失败：" + err.Error()), nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fail("写入截图失败：" + err.Error()), nil
	}
	res := agent.Result{
		Content: fmt.Sprintf("截图已保存：%s（%d KB）", path, len(data)/1024),
		Display: map[string]any{"path": path, "bytes": len(data)},
	}
	if len(data) <= 3<<20 {
		res.Image = &agent.ImagePart{MIME: "image/png", Data: data}
	}
	return res, nil
}

type NetworkTool struct{}

func (NetworkTool) Name() string { return "browser_network" }
func (NetworkTool) Description() string {
	return "列出最近 40 条页面网络请求（方法、URL、状态码、类型）。排查接口、确认请求是否发出时用它。"
}
func (NetworkTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (NetworkTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	c, err := shared(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.eventMu.Lock()
	events := append([]netEvent(nil), c.events...)
	c.eventMu.Unlock()
	if len(events) == 0 {
		return agent.Result{Content: "还没有捕获到网络请求。先执行 browser_open 或点击，再回来看。"}, nil
	}
	var b strings.Builder
	for i, ev := range events {
		fmt.Fprintf(&b, "%d. %s %s", i+1, ev.Method, ev.URL)
		if ev.Status != 0 {
			fmt.Fprintf(&b, " → %d", ev.Status)
		}
		if ev.Type != "" {
			fmt.Fprintf(&b, " (%s)", ev.Type)
		}
		b.WriteByte('\n')
	}
	return agent.Result{Content: b.String(), Display: map[string]any{"count": len(events)}}, nil
}

// Tools 返回浏览器工具集（登记顺序即模型看到的顺序）。
func Tools() []agent.Tool {
	return []agent.Tool{OpenTool{}, SnapshotTool{}, ClickTool{}, TypeTool{}, ScreenshotTool{}, NetworkTool{}}
}

func fail(msg string) agent.Result {
	return agent.Result{IsError: true, Content: msg}
}

// elementLookup 生成「按 index 或 selector 找元素」的 JS 表达式。
// selector 通过 JSON 编码注入，避免把模型给的字符串直接拼进脚本。
func elementLookup(index *int, selector string) (string, error) {
	if index != nil {
		if *index < 0 {
			return "", fmt.Errorf("index 不能为负数")
		}
		return fmt.Sprintf(`document.querySelector('[data-moon-idx="%d"]')`, *index), nil
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("需要 index 或 selector 之一；先 browser_snapshot 拿到元素清单")
	}
	encoded, err := json.Marshal(selector)
	if err != nil {
		return "", fmt.Errorf("selector 无法编码")
	}
	return "document.querySelector(" + string(encoded) + ")", nil
}

// screenshotPath 把截图限制在工作区内；没有工作区时退到系统临时目录。
func screenshotPath(ex *agent.Exec, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	base := ""
	if ex != nil {
		base = strings.TrimSpace(ex.Root)
	}
	if base == "" {
		base = os.TempDir()
	} else {
		root, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("解析工作区失败：%w", err)
		}
		base = root
	}
	if name == "" {
		name = filepath.Join(".moon", "screenshots", fmt.Sprintf("browser-%d.png", time.Now().UnixNano()))
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("path 必须是工作区内的相对路径")
	}
	full := filepath.Join(base, filepath.FromSlash(name))
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path 不能跳出工作区：%s", name)
	}
	if !strings.EqualFold(filepath.Ext(full), ".png") {
		full += ".png"
	}
	return full, nil
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（已截断）"
}
