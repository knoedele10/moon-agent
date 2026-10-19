# blog-my 接口契约（api-contract）

> 本文件定义前端与后端之间的接口契约。Day 15 起 `/api/health` 已公网可访问；其余接口在 Day 16–20 由真实业务实现，前端按本契约调用。
> 公网 mock 版同时实现了所有接口（返回假数据），用来在真后端就绪前演示页面。
> Day 23 增补第 9 节 `/api/kb/*`（本地多模态知识库）。该节按 `web/kb.go` 的实现逐字段整理，
> 状态码与响应结构均经 `scripts/verify-rag.mjs` 实测复核（证据见 `docs/day23/verify-log.txt` 与 `docs/day23/notes.md`）。

## 通用约定
- 协议：HTTP / 同源（前端与接口在同一域名下，无跨域）。
- 编码：UTF-8；响应体为 `application/json`。
- 鉴权：需要登录的接口，浏览器带 Cookie；未登录返回 `401`，前端跳 `/login`。
- 错误格式：非 2xx 响应体为 `{ "error": "可读的错误说明" }`。

## 接口列表

### 1. `GET /api/health` —— 健康检查（Day 15 已上线）
用途：公网探活，确认服务在跑。
- 请求：无参数。
- 响应 `200`：
```json
{ "status": "ok", "service": "blog-my", "version": "mock-1", "time": "2026-10-02T14:00:00.000Z", "uptime": 123 }
```
- 说明：永远立即返回，不受业务 mock 开关影响。

### 2. `GET /api/conversations` —— 会话列表
- 鉴权：需登录。
- 响应 `200`：`[ { "id": 1, "title": "帮我写一段自我介绍" }, ... ]`
- `401`：未登录。
- `500`：服务错误 `{ "error": "..." }`。

### 3. `GET /api/conversations/{id}/messages` —— 某会话的历史消息
- 鉴权：需登录。
- 路径参数：`id` 会话 ID。
- 响应 `200`：`[ { "role": "user"|"assistant", "content": "..." } ]`
- `401` / `404`（会话不存在）/ `500`。

### 4. `GET /api/models` —— 可用模型列表
- 鉴权：需登录。
- 响应 `200`：`[ { "id": "deepseek-chat", "label": "DeepSeek · deepseek-chat", "endpoint": "deepseek", "has_key": true }, ... ]`
- `endpoint` 为厂商端点 ID；`has_key` 为 `false` 表示该模型尚未配置 API Key，前端在"模型"视图里标"未配置 Key"。
- 端点清单（含新增）：deepseek、openai、moonshot、qwen、zhipu、anthropic、gemini、ernie、ark、ollama、mock。
  - LLM 调用层已迁移到 Eino（CloudWeGo）框架：每个端点由 `Backend` 字段映射到一个 Eino ChatModel 组件（openai / claude / gemini / ollama / mock）。
  - `ernie`（文心一言）改走千帆 v2 OpenAI 兼容端点，不再做 `API_KEY:SECRET_KEY` 两段式 token 交换，用户填的 key 直接作为 bearer；v2 的 key 在千帆控制台 v2 页面生成，与旧版不同。
  - `ark`（火山方舟）为占位端点，后端工厂未实现，选中后返回 `llm: 没有匹配的模型后端`。
  - `ollama`（本地模型）无需 API Key，默认 `http://localhost:11434`。
  - `mock`（本地回声）无需 API Key，用于无凭证时验证流式链路。

### 5. `GET /api/keys` —— 当前用户的 API Key 列表
- 鉴权：需登录。
- 响应 `200`：`[ { "id": 1, "endpoint": "deepseek", "masked": "sk-****1234" } ]`
- `endpoint` 为厂商端点 ID（与 `/api/models` 的 `endpoint` 一致）；`masked` 为掩码后的密钥，完整密钥不返回。

### 6. `POST /api/favorites` —— 收藏写入（Day 18）
- 请求体：`{ "title": "收藏内容标题", "url": "可选链接" }`
  - `title` 为必填字段。
- 成功 `201`：`{ "ok": true, "id": 5, "title": "...", "url": null, "created_at": "..." }`（数据库新增一行）
- 重复提交（同 title 已存在）`409`：`{ "ok": false, "error": "该内容已在收藏中，请勿重复收藏" }`
- 缺必填字段 `400`：`{ "ok": false, "error": "缺少必填字段 title（收藏内容标题不能为空）" }`
- 非法 JSON `400`：`{ "ok": false, "error": "请求体不是合法的 JSON" }`

### 7. `PATCH /api/favorites/{id}` —— 收藏修改（Day 22）
- 路径参数：`id` 收藏 ID。
- 请求体：`{ "title": "可选", "url": "可选" }`，两个字段至少给一个；**只改传进来的字段**，没传的保持原值。
- 成功 `200`：`{ "ok": true, "before": { "title": "原标题", "url": null }, "data": { "id": 7, "title": "新标题", "url": "https://...", "updated_at": "..." } }`
  - 响应里带回 `before`（改动前的原值），前端可直接做「改之前 / 改之后」对比。
- `404`：`{ "ok": false, "error": "收藏不存在或已删除（id=7）" }`
- `400`：`{ "ok": false, "error": "没有可更新的字段（title / url 至少给一个）" }`；`title` 改成空 → `title 不能改成空`；请求体非法 JSON → `请求体不是合法的 JSON`。
- `409`：改成与另一条收藏相同的 title → `{ "ok": false, "error": "该内容已在收藏中，请勿重复收藏" }`。

### 8. `DELETE /api/favorites/{id}` —— 收藏删除（Day 22，软删除）
- 路径参数：`id` 收藏 ID。
- 成功 `200`：`{ "ok": true, "id": 7, "deleted": true, "soft": true, "deleted_at": "..." }`
- 语义：**软删除** —— 只在记录上写 `is_deleted: true`，不真删数据；`GET /api/favorites` 会跳过它，删错了还能从 `deploy/data/favorites.json` 里找回。
- 删除后 `GET /api/favorites` 不再返回该条；重复 DELETE 同一个 id → `404`。

### 9. 知识库（Day 23 新增，多模态 RAG）

全部 `/api/kb/*` 接口都在需要登录的分组里；未登录返回 `401` `{ "error": "未登录" }`（页面请求会 302 跳 `/login`，接口请求返回 401）。

**公共约定**
- 上传上限 **20 MiB**（`20 << 20 = 20971520` 字节），超出会被拒。
- 文档类型按**文件名扩展名**判定，不看请求的 `Content-Type`（浏览器上传图片时常给 `application/octet-stream`）：
  - `kind=text`：`.txt` `.md` `.markdown` `.log`
  - `kind=image`：`.png` `.jpg` `.jpeg` `.webp` `.gif`
- **去重按内容 sha256**（`kb_document.sha256` 唯一索引）：同一份字节/同一段文本再次入库不新建记录，返回 `200` + `duplicated: true` + 已有文档的 `id`。首次入库为 `201` + `duplicated: false`。
- 时间字段 `created_at` 为 RFC3339 带本地时区偏移，例如 `\"2026-10-11T13:13:09+08:00\"`。
- 列表与检索的范围是「本用户的文档 + `user_id` 为空串的公共文档」（`user_id = ? OR user_id = ''`）。当前 `user_id` 只是归属标记，**不做权限硬隔离**。
- 向量为 bge-m3 的 1024 维 float32；`chunks[].dim` 是写入时该块的实际维度。

#### 9.1 `GET /api/kb/documents` —— 文档列表
- 请求：无参数。返回最新 200 条，按 `id` 倒序；**不返回** `content` / `embedding`。
- 响应 `200`：
```json
{ "ok": true, "data": [
  { "id": 2, "kind": "text", "title": "Day 23 验收：本地多模态知识库文本 20261011131308",
    "mime": "text/plain; charset=utf-8", "bytes": 1505, "chunk_count": 3,
    "created_at": "2026-10-11T13:13:09+08:00" }
] }
```
- `500`：`{ "error": "读取知识库失败：..." }`。

#### 9.2 `POST /api/kb/text` —— 直接入库一段文本
- 请求体：`{ "title": "可选标题", "text": "正文（必填）" }`
- `title` 为空时取正文的第一个非空行，最多 40 个字符。
- 正文先去首尾空白；空白正文 → `400 { "error": "文本内容为空" }`。
- 成功 `201`：`{ "ok": true, "id": 2, "kind": "text", "title": "...", "chunk_count": 3, "duplicated": false }`
- 内容重复 `200`：同上结构但 `\"duplicated\": true`，`id`/`title`/`chunk_count` 取自已有文档。
- 其它 `400`：请求体非法 JSON → `请求格式错误`；切块为空 → `切块结果为空`；嵌入失败 → 原样回传底层错误（例如 `连接本地嵌入模型失败（http://127.0.0.1:11434）：...`）。

#### 9.3 `POST /api/kb/documents` —— 上传文件（multipart/form-data，字段名 `file`）
- 请求：`multipart/form-data`，文件名决定类型（见上方公共约定）。
- 成功 `201`：`{ "ok": true, "id": 4, "kind": "image", "title": "verify-day23-20261011131308.png", "chunk_count": 1, "duplicated": false }`
- 同上内容重复 `200`（`duplicated: true`）。
- `title` 取文件名（含扩展名）；图片文档的 `source` 也记文件名。
- `400`：
  - 缺 `file` 字段 / 请求体超过 20 MiB → `{ "error": "缺少文件字段 file（multipart/form-data）" }`
  - 不支持的扩展名 → `{ "error": "不支持的文件类型 \".pdf\"（先支持 .txt/.md 与 .png/.jpg/.webp/.gif）" }`
  - 图片理解失败 → `{ "error": "图片理解失败（视觉模型不可用或超时）：..." }`

#### 9.4 `GET /api/kb/documents/{id}` —— 文档详情（含各块文本）
- 路径参数：`id` 文档 ID。
- 响应 `200`：
```json
{ "ok": true, "data": {
  "id": 2, "kind": "text", "title": "...", "mime": "text/plain; charset=utf-8",
  "bytes": 1505, "chunk_count": 3, "created_at": "2026-10-11T13:13:09+08:00",
  "chunks": [ { "seq": 0, "kind": "text", "text": "本文件是 Day 23 端到端验收用的临时文本...", "dim": 1024 } ]
} }
```
- `chunks` 按 `seq` 升序，字段只有 `seq` / `kind` / `text` / `dim`（**不含** `id`、`document_id`、`embedding`、`image_path`，也不返回图片原始字节）。
- `400 { "error": "id 非法" }` / `404 { "error": "文档不存在" }` / `500 { "error": "读取文档失败：..." }`。

#### 9.5 `DELETE /api/kb/documents/{id}` —— 删除文档及其全部块
- 响应 `200`：`{ "ok": true, "id": 2, "deleted": true }`
- 语义：在一个事务里先删 `kb_chunk` 再删 `kb_document`；删除后列表与详情都不再返回该文档，重复 DELETE 同一 id → `404`。
- `400 { "error": "id 非法" }` / `404 { "error": "文档不存在" }` / `500 { "error": "删除失败：..." }`。

#### 9.6 `GET /api/kb/stats` —— 总量概览
- 响应 `200`：`{ "ok": true, "documents": 1, "chunks": 3 }`
- `500 { "error": "统计失败：..." }`。

#### 9.7 `POST /api/kb/search` —— 只检索
- 请求体：`{ "query": "检索内容（必填）", "top_k": 5 }`
- `top_k` 会被收敛：`<= 0` 视为 `5`，`> 20` 按 `20` 处理。
- 响应 `200`：
```json
{ "ok": true, "hits": [
  { "chunk_id": 7, "document_id": 3, "seq": 0, "kind": "text",
    "title": "Day 23 验收：本地多模态知识库文本 20261011131308",
    "text": "本文件是 Day 23 端到端验收用的临时文本...", "score": 0.46086741101203554 }
] }
```
- `kind` 为 `text`（文本块）或 `image_caption`（图片描述块）；`score` 是余弦相似度，按降序排列；无命中时 `hits` 为空数组 `[]`。
- `400`：请求体非法 JSON → `请求格式错误`；`query` 为空白 → `查询内容为空`；嵌入失败 → 原样回传底层错误。

#### 9.8 `POST /api/kb/ask` —— 检索 + 生成（SSE 流式）
- 请求体：`{ "query": "问题（必填）", "model": "qwen2.5:7b", "top_k": 5 }`
- `model` 必须是 `GET /api/models` 返回过的模型 `id`（裸 id，不带端点前缀），否则在 SSE 里回一个 `error` 事件：`llm: 端点不存在`。
- 请求体非法 JSON → `400 { "error": "请求格式错误" }`；`query` 为空白 → `400 { "error": "query 不能为空" }`（这两个校验发生在写 SSE 响应头之前）。
- 一旦开始流式响应，HTTP 状态固定为 `200`，错误改用 `error` 事件表达：
  - 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`。
  - 事件序列：首个事件恒为 `sources`，随后若干 `delta`，以 `done` 结束（出错时以 `error` 收尾且不再有 `done`）。
```
event: sources
data: {"hits":[{ "chunk_id": 11, "document_id": 5, "seq": 0, "kind": "text", "title": "...", "text": "...", "score": 0.4472799496156533 }]}

event: delta
data: {"content":"紫罗兰-20261011131337"}

event: done
data: {}
```
  - `error` 事件的 `data` 形如 `{"error":"..."}`，取值包括检索失败原因、`llm: 端点不存在`、上游模型错误、`连接意外中断`。
- 生成提示词要求「只依据检索到的资料回答，资料不足就说知识库中没有相关信息，并在末尾用 `[编号]` 标注出处」；命中 `image_caption` 块且 `model` 名包含 `vl` / `vision` / `gemma3` 时，最多把 3 张原图作为多模态内容一并发给模型（实测用的 `qwen2.5:7b` 是纯文本模型，不会附带图片）。

---

### 10. 电脑操控 CUA（Day 24 新增）

全部 `/api/cua/*` 接口都挂在需要登录的分组里；未登录返回 `401`（页面请求 302 跳 `/login`）。
另有 1 条**内部**路由 `POST /internal/cua/llm/v1/chat/completions`（模型代理），它**不走**登录校验，见 10.6。

**公共约定**
- 这些接口**不使用** `ok/data` 包裹，直接返回业务对象：`status` 平铺、历史用 `runs` 数组、创建返回三个字段。
- 同一时刻**只允许一个 run**；已有 run 在跑时创建返回 `409`。
- 事件流是 SSE：**事件名就是 CUA 的 `kind`**，`data:` 是 CUA 子进程原样输出的 JSON 行（Go 不做二次封装）。
- 运行历史与事件**仅存内存**：服务重启后 `GET /api/cua/runs` 返回空数组。前端须把空列表当正常空态。
- 每个 run 只保留最近 **10 条** `frame_image` 事件（丢弃更早的截图），**所有非图片事件全部保留**。
- 时间字段为 RFC3339（如 `"2026-10-11T16:24:38+08:00"`）。

#### 10.1 `GET /api/cua/status` —— 环境与可用性（**永远 200**）
- 请求：无参数。不可用不是错误，用字段表达；探测结果缓存 30 秒。
- 响应 `200`：
```json
{
  "available": true,
  "running": false,
  "run_id": "",
  "python": { "found": true, "version": "3.13.5" },
  "cua": { "found": true, "version": "0.1.0" },
  "env": { "screen_w": 2560, "screen_h": 1600, "dpi_scale": 1.5 },
  "models": [ { "name": "deepseek-flash", "backend": "openai", "vision": true } ],
  "notes": ["..."]
}
```
- `models` **只列支持视觉的模型**（CUA 必须有视觉）；探测不到返回 `[]`，不报错。
- `env` 用 ctypes 取主屏物理分辨率与缩放，**不依赖 pyautogui**。

#### 10.2 `POST /api/cua/runs` —— 启动一次运行
- 请求体：
```json
{ "task": "在记事本里输入 hello", "model": "deepseek-flash",
  "max_steps": 20, "max_seconds": 600, "dry_run": false, "allow_dangerous_hotkeys": false }
```
| 字段 | 类型 | 必填 | 约束 |
|---|---|---|---|
| `task` | string | 是 | 去空白后非空；长度 **≤ 2000** 个字符（按码点计） |
| `model` | string | 是 | **不允许空**；必须出现在 `status.models` 里 |
| `max_steps` | int | 否 | 1–200，默认 **20** |
| `max_seconds` | int | 否 | 10–3600，默认 **600** |
| `dry_run` | bool | 否 | 默认 `false`；为真时不产生真实键鼠输入 |
| `allow_dangerous_hotkeys` | bool | 否 | 默认 `false` |

- 成功 `201`：`{ "run_id": "<32 位十六进制>", "model": "deepseek-flash", "task": "..." }`
- 错误：
  - `400` `{ "error": "请求格式错误" }` —— 请求体不是合法 JSON
  - `400` `{ "error": "task 不能为空" }` / `{ "error": "task 长度不能超过 2000 字符" }`
  - `400` `{ "error": "model 不能为空（不允许静默用错模型）" }` / `{ "error": "模型 \"x\" 不在可用视觉模型列表里" }`
  - `400` `{ "error": "max_steps 超出范围 1-200" }` / `{ "error": "max_seconds 超出范围 10-3600" }`
  - `409` `{ "error": "已有任务在运行" }` —— 单 run 闸门
  - `500` `{ "error": "启动 CUA 子进程失败：..." }` —— 子进程起不来（该 run 同时被标记为 `failed`）

#### 10.3 `GET /api/cua/runs/{id}/events` —— 实时轨迹（SSE）
- 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`。
- 序列：首个事件恒为 `start` → 历史事件 → 实时事件 → 最后一个事件恒为 `exit`。
```
event: start
data: {"run_id":"7f3a...","model":"deepseek-flash","task":"在记事本里输入 hello"}

event: run_start
data: {"ts":1760247878.123,"kind":"run_start","task":"...","route":{...}}

event: frame_image
data: {"kind":"frame_image","step":1,"media_type":"image/jpeg","width":640,"height":400,"data_uri":"data:image/jpeg;base64,..."}

event: exit
data: {"run_id":"7f3a...","exit_code":0,"code":""}
```
- `run_id` 不存在 → `404` `{ "error": "run 不存在" }`。
- run **已结束** → 立即补发全部历史（含 `exit`）后关闭，**支持刷新页面重连**。
- **客户端断开不会杀死 CUA 进程**（断线只退订）。
- `exit` 里的 `code` 是 CUA 的终止码（`E_xxx`）；**空串表示正常结束**。
- 事件名取值：`start` / `exit` 由 Go 生成，其余为 CUA 的 `kind`：`run_start`、`frame`、`frame_image`、`observation`、`agent`、`step`、`action_result`、`replan`、`expect_timeout`、`executor_retry`、`guardrail`、`run_end`。
- `frame_image` 是**可选**事件（长边 640 / JPEG q60）：前端不得假设每个 `frame` 都有配对的 `frame_image`。

#### 10.4 `POST /api/cua/runs/{id}/stop` —— 停止运行
- 请求：无请求体。
- `200` `{ "stopped": true }` —— 已发出停止并杀掉整棵进程树（Windows 用 `taskkill /T /F`）。
- `200` `{ "stopped": false }` —— 该 run **已经结束**（不是错误）。
- `404` `{ "error": "run 不存在" }`。

#### 10.5 `GET /api/cua/runs` —— 运行历史（内存，最近 50 条）
- 请求：无参数。新的在前。
- 响应 `200`：
```json
{ "runs": [ { "run_id": "7f3a...", "task": "在记事本里输入 hello", "model": "deepseek-flash",
  "status": "done", "code": "", "steps": 3,
  "started_at": "2026-10-11T16:24:38+08:00", "ended_at": "2026-10-11T16:24:41+08:00" } ] }
```
- `status` 取值：`running` / `done` / `failed` / `stopped`；`ended_at` 运行中为空串。
- **服务重启后该接口返回 `{ "runs": [] }`**（仅内存保留）。

#### 10.6 `POST /internal/cua/llm/v1/chat/completions` —— 模型代理（**内部路由，不在 `/api` 下**）
- 用途：CUA 子进程没有 session cookie，不能直连上游，也拿不到用户的 API Key，于是把模型调用代理回 Go。
- 鉴权：`Authorization: Bearer <每 run 一次性 token>`。token 由 Go 生成并经 `CUA_MODEL_API_KEY` 传给子进程；缺失或无效 → `401` `{ "error": { "message": "..." } }`。
- 请求：标准 OpenAI chat completions，**非流式**。`messages[].content` 可为字符串，也可为数组：
  ```json
  { "model": "deepseek-flash",
    "messages": [ { "role": "user", "content": [
      { "type": "text", "text": "..." },
      { "type": "image_url", "image_url": { "url": "data:image/jpeg;base64,..." } } ] } ],
    "max_tokens": 16384 }
  ```
  - 图片**只支持 `data:` URI**（base64）；外链图片不下载。未知 part 类型（audio 等）被忽略。
- 成功 `200`：`{ "choices": [ { "message": { "role": "assistant", "content": "..." } } ] }`
- 错误（body 形如 `{ "error": { "message": "..." } }`）：
  - `400` 请求本身的问题：`model 不能为空`、`未知模型：x`、`messages[i].content 既不是字符串也不是数组`、`只支持 data: URI 图片`
  - `401` 缺少 Bearer token / token 无效
  - `502` `模型返回空 content（E_MODEL_EMPTY）：多为 max_tokens 过小或上游截断，代理已按不低于 16384 兜底`
  - `4xx/5xx` 上游错误**按上游真实状态码与原因透传**（不再把上下文超限说成 key/模型名问题）
- `max_tokens` **下限 16384**（2026-10-11 二次裁定）：请求里更小的值会被抬到 16384（更大的原样使用）。
  实测 `deepseek-flash` 在 `max_tokens=80` 时返回空 content、`4000` 时正常；更关键的是**真实桌面任务**在下限 4096 时于 **step 12 死于 `E_MODEL_EMPTY`**——同一 prompt 三次调用 `completion_tokens = 414 / 195 / 1339`，长推理步会把 4096 吃光；上调到 16384 后同一请求正常返回（`completion=1339`）。

---

### 11. `POST /api/chat` 与 AI 自主工具调用（Day 25 新增）

> 依据：`docs/agent/00-contract.md`（**冻结**）。本节只追加，**不改第 10 节（CUA 冻结）**。
> **实现状态（2026-10-12 14:50:41 实测）**：前端（3 页签 / 右上角密钥 / 工具卡片）、`agent` + `agent/fs` + `llm` + `rag`，以及 **`web/agent.go` 接线均已落地**：
> `web/chat.go:125` 收 `tools` 字段，`:207-210` 在 `tools=true` **且** `AGENT_TOOLS` 开启时调 `runAgentChat`。
> 下面的字段与顺序按**实测代码**（`web/agent.go:239-280`）书写；**尚未用真实模型端到端跑过** ⏳。

`POST /api/chat` 此前未收录进本文档，本节一并将**现状**补齐（现状来自 `web/chat.go` 实测）。

#### 11.1 现状（未加 `tools` 时的行为，仍然完全不变）

- 路由：`api.POST("/chat", postChat)`，挂在 `api := r.Group("/api", authRequired)` 下，**需要登录会话**。
- 请求体（`web/chat.go:119-123`）：

```json
{ "conversation_id": 12, "model": "deepseek-chat", "content": "你好" }
```

- 响应：**SSE**（`text/event-stream`），帧格式实测为 `event: <名字>\ndata: <JSON>\n\n`（`web/chat.go:188`）。
- 现有三个事件（**保持不变**，前端向后兼容）：

| 事件 | data | 说明 |
|---|---|---|
| `delta` | `{"content":"…"}` | 增量文本 |
| `done` | `{}` | 正常结束 |
| `error` | `{"error":"…"}` | 出错（含"连接意外中断"） |

- 参数校验（现状）：`请求格式错误`（400）、`内容不能为空`（400）、`会话不存在`（404）、`创建会话失败`（500）。

#### 11.2 新增字段 `tools` 🔶

| 值 | 行为 |
|---|---|
| 省略 / `false` | **与 11.1 完全一致**（契约 §0：未开启时行为不变） |
| `true` | 走 agent 循环：模型可使用六个工具（见 `docs/agent/02-tools.md`） |

```json
{ "conversation_id": 12, "model": "deepseek-chat", "content": "把 notes/a.txt 里的 world 改成 CUA", "tools": true }
```

> 服务端另有**环境变量总开关** `AGENT_TOOLS`（默认关闭）；未配 `AGENT_FS_ROOT` 时**四个文件工具不注册**（`web/agent.go:102-110`）。
> **两道闸门已实测确认**：服务端未开启时收到 `tools:true` **静默走老路径**（`web/agent.go:67`：`toolsRequested && agentToolsEnabled()`）。

#### 11.3 新增事件：`tool_call` / `tool_result` 🔶

```
event: tool_call
data: {"id":"call_abc","name":"read","arguments":{"file_path":"a.txt"}}

event: tool_result
data: {"id":"call_abc","name":"read","ok":true,"summary":"读取 a.txt（12 行）","detail":"1\thello\n2\t...","display":{}}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 调用 ID；`tool_result` 与对应的 `tool_call` **同 id** |
| `name` | string | 工具名（`read` / `write` / `edit` / `read_image` / `memory_search` / `memory_write`） |
| `arguments` | object | `tool_call`：模型给出的参数（**原始 JSON**，未校验） |
| `ok` | bool | `tool_result`：该次调用是否成功 |
| `summary` | string | `tool_result`：给前端的一行摘要 |
| `detail` | string | `tool_result`：完整结果（前端折叠展示）；工具失败时这里是**给模型看的错误说明** |
| `display` | object | `tool_result`：结构化摘要（可选，用于卡片渲染） |

#### 11.4 事件顺序与不变量 🔶

1. 每次工具调用：先 `tool_call`，执行完后**必有**一条同 id 的 `tool_result`（**成对**）。
2. **工具失败走 `tool_result`（`ok:false`），不走 `error` 事件**——工具报错是给模型的信息，循环继续（契约 §3 第 5 条）。
3. `delta` 仍然实时发；一轮里可以出现 `delta` 与 `tool_call` 交替（模型边说边调）。
4. 正常结束发 `done`；触顶（12 轮 / 24 次）与取消/出错发 `error`，**触顶文案必须说明是哪种触顶**。
5. `read_image` 产出的图片**不出现在 SSE 里**：它在服务端作为一条 user 消息回灌给模型。

#### 11.5 记忆自动注入 🔶

`tools=true` 时，服务端用**用户这条消息**检索记忆库 top-k，把结果以 `<memory>…</memory>` 拼进 system prompt：
- **用户看不到**这一步的卡片（它是 system prompt 的一部分）；
- **检索失败必须降级为"无记忆"并继续对话**，不得阻断（契约 §5/§6）；
- 与 `memory_search` 工具**共存**：注入解决"顺带想起"，工具解决"主动翻找"。

#### 11.6 向后兼容保证

| 场景 | 期望 | 依据 |
|---|---|---|
| 不传 `tools` | SSE 事件序列与改造前**逐帧一致** | 契约 §9 第 3 条 |
| `llm.Registry.Stream`/`Generate` 现有调用（含 CUA 模型代理） | 行为不变（`req.Tools` 为空即原路径） | 契约 §0/§2 |
| CUA 全部接口 | **零改动** | 契约 §0 红线 |

#### 11.7 实测门禁（2026-10-12 14:5x，本人复跑）

| 命令 | 结果 |
|---|---|
| `go build ./...` | 退出码 **0** ✅ |
| `go vet ./...` | 退出码 **0** ✅ |
| `go test ./... -count=1` | 退出码 **0** ✅（`agent` / `agent/fs` / `llm` / `rag` / `web` 全 `ok`） |
| 通过用例数（`-v` 计 `--- PASS`） | `agent` **14**、`agent/fs` **27**、`llm` **15**、`rag` **26**，共 **82** |
| **CUA 红线** `go test ./web/ -run TestCua -count=1` | **ok blog-my/web 0.562s，退出码 0** ✅ 未碰坏 |

> 未覆盖面：**没有真实模型端到端**（六个工具从未被真实请求触发）；沙箱红线与观察策略只有**实现方单测**、**无独立复验**；`runAgentChat` 的 SSE 序列无专门断言 ⏳。
> 另：14:47–14:49 之间曾观察到 `TestSandboxRejectsSymlinkEscape` / `TestConcurrentEditsOneWins` / `llm TestT36Merge*` 处于 FAIL，随后转绿——**先红后绿**说明这些负向对照确实在断言行为。

---

## mock 演示开关（仅公网 mock 版有）
页面 URL 带 query 可演示各状态（标记写进 cookie，对 API 请求也生效）：
- `?mock=normal` —— 正常返回。
- `?mock=slow` —— 接口延迟 10 秒（看加载态）。
- `?mock=hang` —— 接口挂起 120 秒（看加载态，便于截图）。
- `?mock=error` —— 接口返回 500（看错误态 + 重试）。
- `/api/health` 不受这些开关影响。

## 待实现（Day 16–20）
## 12. `/api/agent/settings` —— 工作区设置（2026-10-12 新增）

工作区 = AI 文件工具的沙箱根目录。按用户保存，优先级高于环境变量 `AGENT_FS_ROOT`。

### `GET /api/agent/settings`

- 鉴权：需登录。
- 响应 `200`：
```json
{"workspace":"D:\\my-workspace","configured":true,"source":"user","tools_enabled":true}
```
- `source`：`user`（用户设过）/ `env`（回落到 `AGENT_FS_ROOT`）/ `none`（都没有）。

### `PUT /api/agent/settings`

- 鉴权：需登录。
- 请求体：`{"workspace":"D:\\my-workspace"}`
- 校验：必须是**绝对路径**；不能是盘符根（`E:\\`）；目录不存在会自动创建；最终必须是目录。
- 成功 `200`：`{"ok":true,"workspace":"D:\\my-workspace"}`
- 失败 `400`：`{"error":"请填绝对路径，例如 D:\\my-workspace"}`

前端入口在对话页的「记忆」面板内（工作区卡片）。

---

### 13. `/api/skills` —— 技能管理（2026-10-12 新增）

全部 `/api/skills*` 接口都挂在 `api := r.Group("/api", authRequired)` 下，**需要登录**；未登录返回 `401`（页面请求 302 跳 `/login`）。
技能**按用户隔离**（`agent_skill.user_id`）：所有接口只读写当前登录用户的技能，看不到别人的。

**公共约定**

- 技能名在单个用户内唯一（`(user_id, name)` 唯一索引）：新建与导入遇到同名一律**覆盖更新**，不产生重复行。
- 列表接口不返回正文；正文只在 `GET /api/skills/:id` 与两个给模型用的工具里出现。
- `body_runes` 是正文字符数（按 Unicode 码点计，`len([]rune(body))`），不是字节数。
- 错误格式仍是 `{ "error": "可读说明" }`（与本文开头「通用约定」一致）。
- `AgentSkill` 有 `created_at` / `updated_at` 列，但接口**不返回**时间字段。

#### 13.1 `GET /api/skills` —— 技能列表

- 鉴权：需登录。
- 查询参数：`q`（可选）。去首尾空白后非空才生效，过滤范围是 `name LIKE %q% OR description LIKE %q%`，**不含正文**（要在正文里找词，用模型侧的 `skill_search` 工具）。
- 排序：`enabled desc, name asc`（已启用的在前，同状态按名字升序）。
- 响应 `200`：
```json
{ "skills": [
  { "id": 12, "name": "caveman", "description": "超压缩沟通模式…", "enabled": true,
    "source": "C:\\Users\\you\\.agents\\skills\\caveman\\SKILL.md", "body_runes": 1840 }
], "total": 1 }
```
- `total` 等于**本次返回的条数**（过滤后），不是库里总数。
- `source` 是导入时的来源文件路径；手工新建的技能为空串。
- `500`：`{ "error": "读取技能失败" }`。

#### 13.2 `GET /api/skills/:id` —— 技能详情（含正文）

- 鉴权：需登录。
- 路径参数：`id`。
- 响应 `200`：`{ "id": 12, "name": "caveman", "description": "…", "body": "……正文……", "enabled": true }`
- 注意字段集与列表**不同**：详情没有 `source` / `body_runes`，多了 `body`。
- `404`：`{ "error": "技能不存在" }`（id 不存在，或该技能属于别的用户）。

#### 13.3 `POST /api/skills` —— 新建或覆盖

- 鉴权：需登录。
- 请求体：
```json
{ "name": "my-skill", "description": "一句话简介", "body": "# 正文……", "enabled": true,
  "markdown": "---\nname: my-skill\ndescription: 简介\n---\n正文", "source": "可选来源" }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 条件必填 | 技能名；给了 `markdown` 时可由 frontmatter 解析出来 |
| `description` | string | 否 | 简介；本字段为空且 `markdown` 解析出 description 时用它 |
| `body` | string | 条件必填 | 正文；`markdown` 非空时**正文一律取 markdown 的解析结果** |
| `markdown` | string | 否 | 直接贴一段 SKILL.md，服务端用 `skill.ParseMarkdown` 解析 frontmatter（支持 `description: >` 块标量） |
| `enabled` | bool | 否 | 默认 **false**；不传时新建与覆盖都按 false |
| `source` | string | 否 | 溯源用；覆盖同名技能时，只有非空才更新原 `source` |

- 校验：`name` 去空白后为空 → `400` `技能名不能为空`；正文去空白后为空 → `400` `技能正文不能为空`；请求体非法 JSON → `400` `请求格式错误`。
- 同名覆盖：按 `(user_id, name)` 查到已有技能 → 更新 `description` / `body` / `enabled`，返回 **`200`** `{ "ok": true, "id": 12, "updated": true }`。
- 新建：返回 **`201`** `{ "ok": true, "id": 12 }`（**没有** `updated` 字段）。
- 注意默认值方向：本接口 `enabled` 省略时是 **false**（新建出来是关的）；导入接口相反，默认 true（见 13.6）。

#### 13.4 `PATCH /api/skills/:id` —— 改开关 / 简介 / 正文

- 鉴权：需登录。
- 请求体：`{ "enabled": false, "description": "新简介", "body": "新正文" }`，三个字段都可选，**只改传进来的字段**（服务端用指针区分「没传」与「传了零值」）。
- `description` 会去首尾空白；`body` 原样保存（不 trim）。
- 成功 `200`：`{ "ok": true, "enabled": false }`（只回 `enabled`，不回简介 / 正文）。
- `404` `{ "error": "技能不存在" }`；`400` `{ "error": "请求格式错误" }`；`500` `{ "error": "保存技能失败" }`。
- **不支持改名**：请求体里给 `name` 会被忽略（前端也把技能名输入框设为只读）。

#### 13.5 `DELETE /api/skills/:id` —— 删除

- 鉴权：需登录。
- 成功 **`204`**，无响应体。
- `404` `{ "error": "技能不存在" }`（不存在或不属于当前用户）。
- `500` `{ "error": "删除技能失败" }`。
- **硬删除**，不可恢复。

#### 13.6 `POST /api/skills/import` —— 从目录批量导入

- 鉴权：需登录。
- 请求体：`{ "dir": "C:\\Users\\you\\.agents\\skills", "enabled": true, "include_hidden": false }`
- `dir` 校验（`validateSkillImportDir`，与工作区同一套标准）：
  - 空 → `400` `请填写技能库目录`
  - 非绝对路径 → `400` `请填绝对路径，例如 C:\Users\you\.agents\skills`
  - 盘符根（如 `E:\`）→ `400` `不要用盘符根目录，请指向技能库文件夹`
  - 不存在 → `400` `目录不存在：<clean 后的路径>`
  - 存在但不是目录 → `400` `<clean 后的路径> 不是目录`
- `enabled` 是**三态**：不传 = 新建的启用、**已有的保留各自当前开关**；`true` / `false` = 一律设置。
  （刻意如此：重跑导入不该把用户手工关掉的技能又打开。）
- `include_hidden` 默认 **false**。
- 响应 `200`：
```json
{ "ok": true, "root": "C:\\Users\\you\\.agents\\skills", "files": 111, "imported": 110,
  "created": 100, "updated": 10, "enabled": true, "preserve_existing": false,
  "duplicates": ["caveman <- C:\\...\\.gbrain\\...\\SKILL.md"], "errors": [] }
```

| 字段 | 说明 |
|---|---|
| `root` | clean 后的绝对路径 |
| `files` | 扫描阶段遇到的 `SKILL.md` 数 |
| `imported` | 成功写库的技能数 |
| `created` / `updated` | 其中新建 / 覆盖的条数 |
| `enabled` | 新建技能会是什么状态（`enabled` 不传时为 `true`） |
| `preserve_existing` | `true` = 本次没有动过已有技能的开关（请求里没传 `enabled`） |
| `duplicates` | 被同名去重丢掉的条目，格式 `技能名 <- 文件路径` |
| `errors` | 单条失败原因，格式 `技能名: 错误` |

- `duplicates` / `errors` 为空时可能是 `null` 而不是 `[]`（Go 的 nil slice），前端要按「数组或 null」处理。
- 幂等语义：重复导入同一目录不会产生重复行；同一条技能再次导入走 `updated`，正文 / 简介被刷新，`enabled` 只在请求显式传了值时才改（不传则保留原有开关）。
- 扫描与去重规则（`skill.ScanDir`，详见 [agent/04-skills.md](agent/04-skills.md) 第 5 节）：递归找 `SKILL.md`；跳过 `node_modules` 与（默认）以 `.` 开头的目录；同名只保留路径层级最浅的一份。
- 以本机技能库 `C:\Users\woshiss\.agents\skills` 实测为例：盘上共 579 个 `SKILL.md`（约 28 MB，含隐藏目录里的同步副本）；默认 `include_hidden=false` 时 `files=111`、`imported=110`；传 `include_hidden=true` 才会走到 579 个文件，但去重后仍是 110 条。
- 单条写库失败不中断整批：记进 `errors` 继续下一条；只有扫描阶段就失败才返回 `500` `导入失败：...`。

#### 13.7 `POST /api/skills/recall` —— 检索预演（召回 + 重排）

- 用途：看一句话会命中哪些技能；不改业务数据（除惰性补算向量）。
- 鉴权：需登录。
- 请求体：`{ "query": "帮我做一份 PPT", "top_k": 20, "top_n": 5, "model": "deepseek-chat" }`

| 字段 | 必填 | 默认 | 收敛规则 |
|---|---|---|---|
| `query` | 是 | — | 去空白后为空 → `400` `query 不能为空` |
| `top_k` | 否 | **20** | `<= 0` 按 20；`> 50` 按 50 |
| `top_n` | 否 | **5** | `<= 0` 按 5；`> 10` 按 10 |
| `model` | 否 | 空 | 给了、且能在模型注册表里找到、且当前用户有对应 key，才做重排 |

- 响应 `200`：
```json
{ "ok": true, "query": "帮我做一份 PPT",
  "recall_query": "帮我做一份 PPT PPT 演示文稿 幻灯片 制作 生成 presentation slides",
  "recall": [ { "id": 3, "name": "officecli-pptx", "description": "…", "score": 0.42,
    "lexical": 0.5, "semantic": 0.0, "entity": 0.0, "name_hit": false } ],
  "picked": [ { "id": 3, "name": "officecli-pptx", "description": "…", "score": 0.42,
    "lexical": 0.5, "semantic": 0.0, "entity": 0.0, "name_hit": false } ],
  "source": "hybrid",
  "error": "" }
```
- `recall_query` 是**真正拿去召回**的检索串：`model` 可用时它是「原句 + 模型给出的中英混合关键词」（第一步"理解任务"，见 [agent/04-skills.md](agent/04-skills.md) 第 3 节），否则等于 `query`。看召回为什么不准时先看它。
- `recall` 是混合召回的 top_k 条；`picked` 是最终选中的 top_n 条（重排成功时顺序会变）。
- `source`：`llm`（过了大模型重排）/ `hybrid`（纯检索排序）。
- `error`：**不是失败**，是降级原因（例如 `本地嵌入模型不可用（Ollama 没启动？），本轮只用词面+实体召回：...`）；没有降级时为空串。
- 打分字段：`lexical`（词面覆盖率）、`semantic`（bge-m3 余弦，未启用 Ollama 时恒为 0）、`entity`（实体命中率）、`name_hit`（查询直接点名了技能名，命中时融合分 +0.5）。
- `model` 给了但注册表里找不到、或没有可用 key、或候选数 `<= top_n` 时，**静默跳过重排**：`source` 仍是 `hybrid`、`error` 仍为空。
- `500` `{ "error": "召回失败：..." }`（只在读库失败时）。
- 注意：本接口**不看** `AGENT_SKILL_AUTO` / `AGENT_SKILL_RERANK` / `AGENT_SKILL_UNDERSTAND` 三个开关（它本来就是用户主动调用）；只有对话里的自动检索看这三个开关，见 [agent/04-skills.md](agent/04-skills.md) 第 4 节。
- 给了 `model` 时本接口会**两次**调用模型：一次"理解任务"改写查询、一次重排（各约 1 秒）。

---

## 待实现（Day 16–20）
- `POST /api/conversations` 新建会话。
- `POST /api/conversations/{id}/messages` 发送消息（SSE 流式回复）。
- `DELETE /api/conversations/{id}` 删除会话。
- `POST /api/keys` / `DELETE /api/keys/{id}` 密钥增删。
- `POST /login` / `POST /register` / `POST /logout`。
