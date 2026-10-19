# CUA × blog-my 集成契约（v1，冻结）

> 本文由 Lead 拥有并冻结。任何实现方**不得单方面修改**；发现矛盾请报 Lead 裁定。
> 冻结时间：2026-10-11。目标：把 Electron 壳 / Go 后端 / Python CUA **统一成一个应用**。

## 0. 总原则

1. **CUA 的 `cua/1` 动作协议一个字不改**（见 03-action-protocol.md）。集成只在它外面包一层。
2. **不新造线协议**：CUA 已有的 JSONL 日志 schema 就是 Go↔Python 的事件协议。
3. **Go 不重写 CUA**：Go 只做「进程管家 + 事件转发 + 模型代理」。
4. 所有 `/api/cua/*` 路由挂在既有 `api := r.Group("/api", authRequired)` 下。

## 1. 接口 I1：CUA 的事件流（stdout JSONL）

新增 CLI：`--events stdout`（默认关闭，保持既有行为不变）。

- 开启后，**每一行 stdout 是一个 JSON 对象**，逐行 flush（`PYTHONUNBUFFERED` 不依赖）。
- 对象 schema **复用现有 JSONL 日志记录**，`kind` 取值：
  `run_start` / `frame` / `observation` / `agent` / `step` / `action_result` /
  `replan` / `expect_timeout` / `executor_retry` / `guardrail` / `run_end`
- 开启 `--events stdout` 时，**人类可读的打印必须全部关闭**，stdout 只允许 JSONL。
- 诊断信息走 stderr。

新增 CLI：`--events-images`（可选，需与 `--events stdout` 同用）。
- 每产生一帧，在 `frame` 之后追加一条 `{"kind":"frame_image","step":N,"media_type":"image/jpeg","width":W,"height":H,"data_uri":"data:image/jpeg;base64,..."}`。
- 图片**缩到长边 640、JPEG q60**（实测约 30–60 KB），避免 SSE 洪水。
- `frame_image` 是**可选**事件：前端收不到也必须正常工作（只少一张图）。

## 2. 接口 I2：Go 侧 `/api/cua/*`（authRequired）

### 2.1 `GET /api/cua/status`
返回环境与可用性，**永远 200**（不可用不是错误，是状态）：
```json
{
  "available": true,
  "running": false,
  "run_id": "",
  "python": {"found": true, "version": "3.x.y"},
  "cua": {"found": true, "version": "0.1.0"},
  "env": {"screen_w": 2560, "screen_h": 1600, "dpi_scale": 1.5},
  "models": [{"name": "qwen2.5vl-16k", "backend": "ollama", "vision": true}],
  "notes": ["..."]
}
```
`models` 只列**支持视觉**的模型（CUA 必须有视觉）。探测不到就返回空数组，不报错。

### 2.2 `POST /api/cua/runs`
请求体：
```json
{"task": "...", "model": "qwen2.5vl-16k", "max_steps": 20, "max_seconds": 600,
 "dry_run": false, "allow_dangerous_hotkeys": false}
```
- `task` 必填，非空，长度 ≤ 2000；否则 400。
- `model` 必填（**不允许空**，避免静默用错模型）；不在 `status.models` 里则 400。
- `max_steps` 默认 20，范围 1–200；`max_seconds` 默认 600，范围 10–3600。越界 400。
- 同一时刻**只允许一个 run**；已有 run 在跑则 409 `{"error":"已有任务在运行"}`。
成功：201 `{"run_id":"<32位十六进制>","model":"...","task":"..."}`

### 2.3 `GET /api/cua/runs/:id/events`（SSE）
- 与 kbAsk 同款写法：`Content-Type: text/event-stream` + `http.Flusher`。
- 事件名直接取 CUA 的 `kind`（`start` 除外，见下），`data:` 是**原样透传的 CUA JSON 行**。
- 首个事件固定为 `event: start`，data 为 `{"run_id":"...","model":"...","task":"..."}`。
- 结束前最后一个事件固定为 `event: exit`，data 为 `{"run_id":"...","exit_code":N,"code":"E_xxx"}`。
- run 不存在 → 404。run 已结束 → 立即补发全部历史事件再发 `exit`，然后关闭（**支持刷新页面重连**）。
- 客户端断开不得杀死 CUA 进程。

**事件保留上限（v1 补充，2026-10-11，Lead 裁定）**——起因：cua-ui 实测内存占用。
- 实测：单帧 `frame_image` 9.4KB，一个 200 步 run 的事件约 1.9MB；按 50 条 run 上限，
  最坏常驻约 **95MB**，对桌面应用过大。
- 规则：**每个 run 最多保留最近 10 条 `frame_image`**（丢弃更早的图片事件）；
  **所有非图片事件全部保留**（体积小，是轨迹回放的主体）。
  按此上限，50 条 run 的图片内存约 4.7MB。
- 合法性：`frame_image` 在契约第 1 节就是**可选事件**，前端必须在收不到时正常工作，
  因此丢弃历史图片**不违约**。前端不得假设「每个 frame 都必有对应 frame_image」。
- 历史**仅存内存**：服务重启后 `GET /api/cua/runs` 返回空列表。前端必须优雅处理空态，
  不得把空列表当错误。

### 2.4 `POST /api/cua/runs/:id/stop`
- 停止该 run（杀进程树）。成功 `{"stopped":true}`；run 不存在 404；已结束 `{"stopped":false}`。

### 2.5 `GET /api/cua/runs`
返回历史（内存保留最近 50 条）：
```json
{"runs":[{"run_id":"...","task":"...","model":"...","status":"running|done|failed|stopped",
  "code":"E_xxx","steps":N,"started_at":"RFC3339","ended_at":"RFC3339"}]}
```

## 3. 接口 I3：模型代理（CUA 的大脑）

`POST /internal/cua/llm/v1/chat/completions`

- **不走 authRequired**（CUA 进程没有 session cookie），改用**每 run 一次性 Bearer token**：
  Go 生成随机 token，通过环境变量 `CUA_LLM_TOKEN` 传给子进程，并设 `CUA_MODEL_BASE_URL=http://127.0.0.1:<port>/internal/cua/llm/v1`、`CUA_MODEL_API_KEY=<token>`。
  校验失败 → 401。
- 请求体是**标准 OpenAI chat completions**（非流式），`messages[].content` 可能是字符串，也可能是数组，数组元素形如：
  `{"type":"text","text":"..."}` 或 `{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}`
- 实现必须把 `image_url` 的 data URI **解出 MIME 与原始字节**，映射到 `llm.Message.Images`（见 llm/provider.go:33-46），交给既有 registry。
- `model` 字段是 blog-my 的模型名。
- 响应：非流式 OpenAI 格式
  `{"choices":[{"message":{"role":"assistant","content":"..."}}]}`
- 上游报错 → 4xx/5xx 且 body 形如 `{"error":{"message":"..."}}`，**必须把上游真实原因带出来**（不能再出现「模型服务返回 HTTP 400」这种把上下文超限说成 key 问题的误导）。

### 3.1 max_tokens 与空回复（Lead 实测，2026-10-11）

- 实测 `deepseek-flash` 在 `max_tokens` 过小时**返回空 content**（模型内部有 reasoning 消耗 token）：
  同一张图同一个问题，`max_tokens=80` → 空字符串；`max_tokens=4000` → 正常，
  且 `completion_tokens` 实测 163 / 532 / 2189（差异极大）。
- 因此：模型代理**不得**设置过小的 `max_tokens`；CUA 侧对云端模型也必须放大 `model_max_tokens`
  （其默认值 1024 会踩这个坑）。
- **下限从 4096 上调为 16384（2026-10-11 二次裁定，Lead 实测）**：真实端到端跑 deepseek-flash 时，
  run 在 **step 12 死于 `E_MODEL_EMPTY`**（轨迹显示它当时正推进正常：记事本已打开、文字已输入）。
  高保真复测（CUA 真实系统提示词 + 真实 1472×920 截图，prompt=5299 tok）：
  **同一 prompt 三次调用 completion_tokens = 414 / 195 / 1339，波动极大** → 4096 在长推理步会被吃光。
  故 `cuaMinMaxTokens` 由 4096 上调为 **16384**，给推理留足余量。
  实测 16384 下同一请求正常返回（completion=1339，content 非空）。
- **空 content 必须当作错误**（`E_MODEL_EMPTY`），**不得**当成合法回复送进动作解析器——
  否则现场表现是「模型返回结构异常」，把真正的成因掩盖掉。

### 3.2 视觉模型可用性（Lead 实测，2026-10-11）

- `deepseek-flash`：**支持视觉，且 GUI 坐标定位准确**。合成截图（1472×920）实测：
  Save 按钮偏差 2.2px、文件名输入框 5.0px、Cancel 按钮 24.2px，**三个全部命中目标**。
- `deepseek-v4-pro`：**不支持视觉**，对图片答「我无法查看你上传的图片」。
- 结论：CUA 的默认视觉模型指向 `deepseek-flash`；`status.models` 必须含它、不得含 v4-pro。
- 注册表事实修正（Lead 批准，2026-10-11）：`llm/catalog.go` 原 deepseek 端点只列
  `deepseek-chat`/`deepseek-reasoner`，与线上实际不符；实测该端点真实返回
  `deepseek-flash`/`deepseek-v4-pro`。故注册表补入这两个 id（事实纠正，非绕过），
  `deepseek-v4-pro` 留在视觉白名单之外。副作用：`/api/models`（聊天页）也会多出这两个模型，属预期。

### 3.3 测试缝（Lead 批准，2026-10-11）

为了让模型代理可被黑盒验证，新增两个**仅测试用**的环境变量（生产不设）：

| 变量 | 语义 | 默认 |
|---|---|---|
| `CUA_LLM_TOKEN` | 代理的每 run token。**已设则所有 run 都用这个值**（不再随机），并原样传给子进程 | 未设 → 每 run 随机 32 位 hex |
| `CUA_LLM_UPSTREAM` | 覆盖代理的**上游 base_url**，原样使用（OpenAI 兼容服务自带 `/v1`） | 未设 → 注册表里该端点的 BaseURL |
| `CUA_LLM_ENDPOINT_OVERRIDE` | 指定覆盖哪个端点 | `deepseek` |

生效时启动日志打印：`cua: 模型代理上游已覆盖 → endpoint=... base_url=...`。

### 3.4 多模态修复（Lead 批准，2026-10-11）——这是既有缺陷，不是新功能

cua-go 在打通 §3 时发现 `llm/provider.go` 的 OpenAI 多模态路径**原样调用必炸**：
go-openai 的 `ChatCompletionMessage.MarshalJSON` 不允许 `Content` 与 `MultiContent` 并存，
而 eino openai 后端会把两者一起带上 → 上游 502 `can't use both Content and MultiContent properties simultaneously`。

两处修复（均有单测 + 黑盒证据）：
1. `toSchemaMessages`：有图片时 `Content` 留空，文字改由 multi-content 的第一个 text part 承载。
   （ollama 后端本就从 parts 取文字，行为不变。）
2. 新增 `Registry.Generate`（非流式入口）：代理按 §3 是「请求非流式 + 响应非流式」；
   走 `Stream` 时 eino 会强制 `stream:true`，上游普通 JSON 会被读成空 content。

**注意**：这条修复也影响 RAG 的图片问答路径（此前只跑过 ollama 后端，故未暴露）。
属**修好既有隐患**，不改变 `cua/1` 协议。

## 4. 接口 I4：前端「电脑操控」tab

- 路由 `#/cua`，侧栏入口与既有 tab 同款样式。
- 必须能：看环境状态 / 填任务 / 选模型（只列视觉模型）/ 启动 / 停止 / **实时看轨迹与截图** / 看最终报告。
- 轨迹每条至少显示：步号、`thought`、`action`、`ok`/`error`、耗时。
- SSE 用 `EventSource`；页面切走或关闭必须 `close()`。
- 不得引入任何新前端依赖（保持无构建步骤）。

## 5. 接口 I5：打包（阶段 C）

- `packaging/cua.spec`：PyInstaller 单文件，产物 `bin/cua-sidecar.exe`。
- Go 侧优先用 `bin/cua-sidecar.exe`，找不到则回落到 `python -m cua.main`（开发态）。
- Electron 侧用 `electron-builder` 出安装包；`package.json` 加 `dist` 脚本。

## 6. 写域划分（互不重叠）

| 归属 | 写域 |
|---|---|
| cua-go | `web/cua.go`、`web/cua_llm.go`、`web/web.go`（仅加路由行）、`llm/catalog.go`（视觉能力标记）、`llm/provider.go`（多模态修复，见 §3.3）、`packaging/**`、`electron/**`（打包态启动修复，见 §5.1） |
| cua-py | `cua/**`（不含 `cua/tests/`） |
| cua-ui | `templates/chat/index.html`、`templates/static/js/cua.js`、`templates/static/css/style.css`、`templates/static/js/chat.js`（**仅** hash 路由分发两处最小改动：`VIEW_TITLES` 加 cua、`showView()` 加 cua 分支） |
| cua-doc | `docs/cua/04-integration.md`、`docs/cua/README.md`、`docs/api-contract.md`（第 10 节） |
| verifier | `cua/tests/**`、`docs/verify/**` |
| Lead | 本文、最终集成与验收 |

## 7. 验收口径

- `go build ./...` 与 `go test ./...` 通过。
- `python -m cua.main --selfcheck` 仍 81 通过 / 0 失败（**不得回归**）。
- `python cua/tests/run_tests.py` 仍 91 用例 / 0 失败。
- 新增：`--events stdout` 的 JSONL 可被逐行解析；`--events stdout` 时 stdout 无人类可读文本。
- 新增：`/api/cua/*` 五个接口的黑盒测试（含 400/404/409 边界）。
- 新增：模型代理能把 data URI 图片正确映射到 `llm.Message.Images`（用 mock 后端验证，不打真实 API）。
- 新增：前端 `#/cua` 能被真实浏览器加载、能起一次 dry-run 并渲染轨迹。
- 未实测项必须标 ⏳，不得写成已完成。
