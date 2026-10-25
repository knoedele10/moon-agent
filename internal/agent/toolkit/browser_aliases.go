package toolkit

import (
	"blog-my/internal/agent"
	"blog-my/internal/agent/toolkit/browser"
)

// 浏览器工具的真实实现在 toolkit/browser：通过 Chrome DevTools Protocol 驱动
// 用户已经登录的 Chrome（导航、快照、点击、填写、截图、网络请求）。
//
// 早先这里有一版「抓静态 HTML + 在内存里改表单」的占位实现，它无法处理登录、
// JS 渲染和真实提交，和 docs/agent/14-market-comparison.md P1-7 的要求对不上，
// 已由 CDP 实现取代。这里保留旧类型名作为别名，避免 web 层注册表和文档跟着改名。
type (
	BrowserOpenTool       = browser.OpenTool
	BrowserSnapshotTool   = browser.SnapshotTool
	BrowserScreenshotTool = browser.ScreenshotTool
	BrowserNetworkTool    = browser.NetworkTool
	BrowserClickTool      = browser.ClickTool
	BrowserTypeTool       = browser.TypeTool
)

// BrowserTools 返回浏览器工具集（登记顺序与 model 看到的顺序一致）。
func BrowserTools() []agent.Tool { return browser.Tools() }
