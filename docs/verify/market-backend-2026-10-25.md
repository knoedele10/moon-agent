# 验证记录：对标项后端收口（2026-10-25，第二轮）

对象：`docs/agent/14-market-comparison.md` 的 P0/P1 建议项。上一轮提交 `126c608`
已经落下 SQLite、规则、用量、任务对账与策略沙箱；本轮补的是当时仍缺的四块：

1. **浏览器工具改成真正的 CDP**（P1-7）；
2. **MCP Streamable HTTP 传输**（§2.1）；
3. **对话页的用量显示 + 改动审阅面板**（P0-4 / P0-3 的验收项）；
4. **评测用例从 5 条扩到 19 条**（P1-8）。

## 环境

- Windows，仓库 `E:\blog-my`，Go 版本以 `go.mod` 为准
- 构建缓存：仓库内 `.gocache` / `.gomodcache-hermes` / `.gotmp-hermes`
- **未接真实模型、未启动真实 Chrome、未启动 Electron 窗口、未连 MySQL**

## 命令与结果

```powershell
go build ./...                        # exit 0
go vet ./...                          # exit 0
go test ./... -count=1 -timeout 900s   # 全包 ok，exit 0
go test ./evals -count=1 -v            # 15 条脚本任务 + 4 条循环任务全过
npm run test:ui                        # 6 个 jsdom 脚本全 PASS
gofmt -l <本轮改动文件>                 # 无输出
go mod tidy                            # gorilla/websocket 转为直接依赖
```

全包列表：`evals`、`agent`、`agent/evolve`、`agent/fs`、`agent/golden`、`agent/hooks`、
`agent/mcps`、`agent/rules`、`agent/runstore`、`agent/sandbox`、`agent/skill`、
`agent/toolkit`、`agent/toolkit/browser`、`agent/toolsys`、`agent/trigger`、`app`、
`content`、`llm`、`model`、`obs`、`rag`、`web`。

## 1. 浏览器（CDP）

实现：`internal/agent/toolkit/browser/{cdp.go,tools.go}`；`toolkit` 里保留类型别名
（`BrowserOpenTool` 等），web 层注册表不用改。

覆盖（`browser_test.go`，假 Chrome = httptest + WebSocket）：

| 场景 | 断言 |
| --- | --- |
| 导航 | `Page.navigate` 后轮询 `readyState`，拿到 `complete` |
| 快照 | 正文与元素清单；`type=password` 只显示「内容已隐藏」 |
| 点击 | 按 `data-moon-idx` 找到元素并点击，返回 `OK button …` |
| 填写 | 写入并派发 input/change；`submit` 走 `form.requestSubmit()` |
| 截图 | PNG 落在工作区内、文件存在、`Result.Image` 带回图片 |
| 越界 | `path=../../evil.png` 被拒 |
| 白名单 | 未配置时本机地址被拒且提示 `AGENT_BROWSER_ALLOW`；后缀伪装 `example.com.evil.com` 被拒 |
| 网络 | `Network.requestWillBeSent` + `responseReceived` 进入环形缓冲，`browser_network` 能列出 |

删除了此前的占位实现（抓静态 HTML + 内存改表单）：它处理不了登录、JS 渲染与真实提交，
与 14 号文 P1-7 的要求不符，且会和 CDP 版出现两套同名工具。

**未验证**：真实 Chrome 的登录态、像素截图、弹窗/下载、站点兼容性。需要本机
`chrome --remote-debugging-port=9222` 手工跑一轮。

## 2. MCP Streamable HTTP

实现：`internal/agent/mcps/http.go`；`Start` 按配置选择 stdio / HTTP，Manager 默认走 `Start`。

覆盖（`http_test.go`）：

- `initialize` 走 JSON 响应并记住 `Mcp-Session-Id`，后续请求自动回传；
- `tools/list` 走 SSE：先推一条 `notifications/tools/list_changed`，再推匹配 id 的响应；
- `tools/call` 走 JSON；`notifications/initialized` 返回 202；
- 非 2xx → 错误里带 `HTTP 500` 与响应摘要；
- URL 校验：空、`ftp://`、缺 host 全部拒绝；传输选择：只有 url 时走 HTTP。

**未验证**：真实远程 MCP 服务器（OAuth、重定向、长连接保活）。

## 3. 前端：用量与改动审阅

- `templates/chat/index.html`：输入框工具条新增「改动」按钮与用量胶囊；计划面板下方新增审阅面板。
- `templates/static/js/chat.js`：处理 SSE `usage` 事件；解析 `diff --git` 列出文件；逐文件调用
  `POST /api/agent/restore`；新对话时同时清空计划、用量与审阅面板。
- `templates/static/css/style.css`：新增 `.review-*` / `.usage-chip`（复用现有变量与圆角）。

jsdom 回归 `scripts/verify-review-ui.cjs`：拉 diff → 列出 2 个文件 → 点「撤销此文件」→
断言 POST 的 `path` 与 `session_id` → 关闭面板 → 新对话清空。已并入 `npm run test:ui`。

**未验证**：真实浏览器像素（jsdom 不渲染），以及 Electron 打包版的窄屏布局。

## 4. 评测

`evals/` 从 5 条扩到 19 条：

- `TestScriptedTasks`（15 条，纯函数级验收）：规则脚手架、规则叠加、verify 命令解析、
  沙箱拒绝 shell、沙箱写边界、成本估算、多轮用量累加、强杀对账（未知副作用 / 幂等两种）、
  技能 meta、计划整份替换（含非法计划不落库）、OTel JSONL、文件变化快照、PreToolUse 拒绝、
  检查点恢复。
- `TestLoopScenarios`（4 条，真实 fs 工具 + 脚本模型跑完整循环）：`grep→read→edit→read`、
  未读就改被拒后自纠、生成周报、整理并移动文件。每条带 **模型轮数上限** 与 **墙钟预算**。

这 4 条循环任务断言的是磁盘终态与工具结果，不产生真实 token 账单；成本维度用模型轮数近似。
真模型 smoke 仍需供应商密钥，未在 CI 跑。

## 5. 顺带修掉的问题

- `internal/agent/hooks/hooks_test.go` 写死了 `/bin/false`、`/bin/true`，在 Windows 上必红；
  改成按 `runtime.GOOS` 选 `cmd.exe /c exit 1|0`。
- `toolsys/meta_test.go` 的并行/确认/幂等集合漏了 git 工具（上一轮新增），补齐。
- `internal/web/agent.go` 的 git 工具说明原本是乱码（一长串 `?`），改回中文。

## 6. 仍未做 / 边界

- **OS 级沙箱**：`internal/agent/sandbox` 是策略门（拒绝交互式 shell、限制写根与出网域名），
  不是 MCP/MXC / AppContainer。14 号文 P1-6 的「OS 级隔离」只完成了策略层。
- **真实模型验收**：缓存命中率、工具选择质量、压缩摘要，都需要真供应商跑一轮。
- **SQLite 干净机器验收**：单测覆盖 `DB_DRIVER` 选择与建库，但「装完到第一个任务 < 10 分钟」
  没有在干净 Windows 上实测。
- **macOS / 多端**：未开始。
