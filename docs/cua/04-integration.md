# CUA × blog-my 集成说明（v1）

本文说明**已经落地**的集成实现：Electron 渲染进程 ↔ Go 后端 ↔ Python CUA 运行时，
以及它们之间的五条接口（I1 事件流 / I2 HTTP / I3 模型代理 / I4 前端 / I5 打包启动）。

- 契约（冻结，Lead 拥有）：[06-integration-contract.md](06-integration-contract.md)。
  本文**不修改契约**，只描述实现细节与实测边界；两者冲突时以契约与实现代码为准，并应报 Lead 裁定。
- 动作协议：仍是 [03-action-protocol.md](03-action-protocol.md) 的 `cua/1`，集成只是包在外面的一层，**协议一个字未改**。
- HTTP 侧的逐字段契约另见 [../api-contract.md](../api-contract.md) 第 10 节（与本文同源，一个偏向"接口长什么样"，一个偏向"怎么接起来的"）。

> **可信度标记**：✅ 已实测（有命令输出 / 测试结果 / 日志） ｜ 🔶 设计如此（契约或代码已这样定，端到端效果未必验过） ｜ ⏳ 待验证 ｜ 📐 估算。
> 本文所有数字都能在仓库里找到出处；找不到出处的就不写。

---

## 1. 架构总览

    ┌──────────────────────────── Electron 渲染进程 ────────────────────────────┐
    │ templates/chat/index.html + templates/static/js/cua.js                     │
    │ 「电脑操控」tab（路由 #/cua）                                               │
    │   ① GET  /api/cua/status                环境与可用性（永远 200）             │
    │   ② POST /api/cua/runs                  起一次运行                          │
    │   ③ GET  /api/cua/runs/:id/events       实时轨迹（EventSource / SSE）        │
    │   ④ POST /api/cua/runs/:id/stop         停手（杀进程树）                     │
    │   ⑤ GET  /api/cua/runs                  历史（内存，最近 50 条）🔶 前端未消费  │
    └────────────────────────────────┬──────────────────────────────────────────┘
                                     │ HTTP，走既有 session cookie（authRequired）
    ┌────────────────────────────────▼──────────────────────────────────────────┐
    │ Go 后端 web/                                                              │
    │   web/cua.go       进程管家：单 run 闸门、JSONL→SSE 广播、历史、杀进程树      │
    │   web/cua_llm.go   模型代理：Bearer token 校验、OpenAI 消息→llm.Message     │
    │   llm/ 既有网关    注册表 + 多后端（本次修了多模态路径，见 §4.4）             │
    └───────┬─────────────────────────────────────────────────▲─────────────────┘
            │ ① spawn 子进程                                   │ ② HTTP POST
            │   bin/cua-sidecar.exe（打包态）                   │   /internal/cua/llm/v1
            │   或 python -m cua.main（开发态）                 │   Authorization: Bearer <每 run token>
            │ 环境：CUA_MODEL_BASE_URL/API_KEY、                 │
            │      CUA_ALLOW_DANGEROUS_HOTKEYS、PYTHONUTF8=1…   │
    ┌───────▼─────────────────────────────────────────────────┴─────────────────┐
    │ Python CUA 运行时 cua/                                                     │
    │   python -m cua.main --task … --events stdout --events-images             │
    │   stdout：逐行 JSONL 事件（唯一允许的内容）                                  │
    │   stderr：人类可读输出与诊断                                                │
    │   模型调用：OpenAI 兼容客户端 → 指回 Go 的模型代理（不直连上游）               │
    └───────────────────────────────────────────────────────────────────────────┘

三条设计原则（契约 §0，实现严格遵守）：

| 原则 | 实现体现 |
|---|---|
| CUA 的 `cua/1` 协议一个字不改 | Go 只解析 `kind`/`step`/`steps`/`code` 四个字段用于统计，其余**原样透传**（`web/cua.go` 的 `cuaWireEvent`） |
| 不新造线协议 | Go↔Python 的事件协议就是 CUA 既有的 JSONL 日志 schema |
| Go 不重写 CUA | Go 只做进程管家 + 事件转发 + 模型代理，不做坐标换算、不做动作解析、不碰键鼠 |

---

## 2. 接口 I1：`--events stdout` 事件流

✅ 已实测（`cua/loop.py` 的 `EVENT_STREAM_KINDS`、`cua/main.py` 的启动路径、`web/cua_test.go`）。

### 2.1 命令与语义

    python -m cua.main --task "…" --events stdout                 # 开启事件流
    python -m cua.main --task "…" --events stdout --events-images # 额外附带截图事件
    python -m cua.main --events-images                            # 报错退出（必须与 --events stdout 同用）

- 开启后 **stdout 只允许 JSONL**：一行一个 JSON 对象，逐行 flush。
- **人类可读输出与诊断全部改走 stderr**（`human = sys.stderr if cfg.events_stdout else real_stdout`）；
  另有一道兜底：进入 loop 前把 `sys.stdout` 重定向到 stderr，任何漏网的 `print` 都落不到事件流里。
- 默认关闭（`--events` 默认 `None`），因此**既有行为零回归**：不开这个开关时 stdout 仍是原样的人类可读输出。

### 2.2 事件 Kind（契约冻结 11 个 + 可选截图 1 个）

`cua/loop.py:141-154` 的 `EVENT_STREAM_KINDS` 是硬白名单，**不在集合里的 kind 不写 stdout**（审计日志文件不受限，保留全部既有事件）。

| kind | 含义 |
|---|---|
| `run_start` | 运行开始：任务、能力路由决策 |
| `frame` | 截取了一帧：几何、哈希、变化比例 |
| `observation` | 组装给模型的观测：图片尺寸、剩余步数与秒数、标志位 |
| `agent` | 模型交互（含 `model_reask` 等子事件） |
| `step` | 模型给出的一步：`thought` + `action` |
| `action_result` | 执行回执：`ok` / 错误码 / 耗时 |
| `replan` | 触发重规划（`E_STALL` / `E_LOOP`） |
| `expect_timeout` | 等待 `expect` 超时 |
| `executor_retry` | 执行器重试 |
| `guardrail` | 护栏拦截（破坏性词表、高风险动作） |
| `run_end` | 运行结束：`code`、`steps`、摘要 |
| `frame_image` | **可选**：截图本体（仅 `--events-images` 时） |

### 2.3 `frame_image`（可选事件）

    {"kind":"frame_image","step":N,"media_type":"image/jpeg","width":W,"height":H,"data_uri":"data:image/jpeg;base64,…"}

- 每产生一帧，紧跟在该帧的 `frame` 之后追加一条。
- 图片缩到**长边 640、JPEG q60**（`cua/config.py` 的 `event_image_max_edge = 640` / `event_image_quality = 60`）。
- ✅ 实测单帧约 **9.4 KB**（契约 §2.3 记录的内存测算依据）。
- **前端不得假设"每个 `frame` 都有对应的 `frame_image`"**：它是可选事件，收不到也必须正常工作（只少一张图）。

### 2.4 编码：GBK 落字节问题与两侧修复 ✅

**现象**：Windows 管道下 Python 的 stdout 默认按**控制台 locale**（本机实测 **GBK**）落字节，
而 `json.dumps(..., ensure_ascii=False)` 写的是中文；Go 按 UTF-8 读就被静默替换成 U+FFFD。
**emoji 更严重**：修复前子进程直接 `UnicodeEncodeError` 崩溃、退出码 1。

**归因（Lead 更正，2026-10-11）**：成因**只是 stdout 的字节编码**，
**与 argv 无关**——Windows 的 argv 走宽字符 API（`GetCommandLineW`），与 locale 无关，
cua-py 用码点转储证明过 argv 里的中文 + emoji 逐码点正确。

**修复（两侧，缺一不可）**：

1. **Go 侧**（`web/cua.go` spawn）：给子进程环境加 `PYTHONUTF8=1`、`PYTHONIOENCODING=utf-8`（外加 `PYTHONUNBUFFERED=1`）。
2. **Python 侧**（`cua/main.py` 的 `_force_utf8_stdio()`）：`--events stdout` 时，在写第一个事件**之前**对 stdout / stderr 做
   `reconfigure(encoding="utf-8", errors="strict")`（就地生效，已持有的流引用同样被修正）。

**修复后的独立复验（verifier，2026-10-11，记录见 [../verify/c14-encoding-record.md](../verify/c14-encoding-record.md) §4 与 §5）**：

- **stdout 侧**：E8a stdout 原始字节严格 UTF-8 **PASS**（2551 字节）；E8b U+FFFD 计数 **0**（2411 个字符）；
  E8c `run_start.task` 与提交值**逐码点相同**（12 个码点）；E8e 每行解码后再编码逐字节相同 **PASS**。
- **Go / SSE 侧**：C14a SSE 原始字节严格 UTF-8 **PASS**（35234 字节）；C14b task 保真（22 个码点全等、U+FFFD 计数 0）；
  C14c 事件流内无替换字符（18 条事件）。
- **负向对照**（判据本身必须能抓到违规）：事件流 11/11 变异全被抓、集成链路 16/16 全被抓。
  其中「**完全合法的 JSON，但 task 已变成 U+FFFD**」这条最有说明力——事件种类、首尾事件、字段等形状类判据**全部照过**，
  **只有字节级判据抓到它**；这正是必须新增 C14 判据、而不能只靠形状断言的理由。
- 该复验是在 `PYTHONUTF8` / `PYTHONIOENCODING` **均未设**的 shell 里跑的，因此绿不是"环境变了"造成的。
- ⚠️ verifier 明说：它**没能亲手复现"修复前"的坏字节**（它第一次跑判据时修复已落地），
  修复前的采样数字来自 Lead，它自己用「按 GBK 落字节的假生产者」复现同类损坏（坏字节 0xd0 @ 52，乱码串与 Lead 记录完全相同）。

> ⚠️ **已知限制（如实声明）**：非 `--events` 模式（人类可读输出）**仍按控制台 locale 编码**——这是**既有行为，本次未改** 🔶。
> 产品路径（Go 侧始终传 `--events stdout`）已被上述修复覆盖；单独手跑 `python -m cua.main` 看人类输出时，中文仍可能受控制台代码页影响。

### 2.5 输入层：`type` 改走剪贴板（task-25，2026-10-11）

真实 run 里发现**逐字符 `write` 会丢字符**（含空格的长文本尤其明显），task-25 把输入层改成**剪贴板主路径**：

- **所有非空文本**都先 `set_clipboard(text)` 再 `Ctrl+V`——**ASCII 与中文一样走剪贴板**（粘贴是原子操作，不丢字符且更快）。
- **退化路径**：剪贴板不可用（写后读不回）或 `type_use_clipboard_for_unicode=False` 时，退回旧的逐字符 `write`。
- `clear_first` 语义与顺序不变：先 `Ctrl+A`，再粘贴。
- 空文本记 `type_path=empty`；输入会临时**占用系统剪贴板**（该路径的已知副作用）。
- **事件流观测点**：`action_result` 的 `meta.type_path` ∈ {`clipboard`, `write`}，做轨迹回放时可以据此判断实际走了哪条路。
- 自检同步新增 3 项（ASCII 也走剪贴板 / 剪贴板不可用时退化 / `clear_first` 仍先 `Ctrl+A`），自检项数 **84 → 87**。
- 口径与 [../../cua/README.md](../../cua/README.md) 的安全不变量第 10 条一致（该文件由 cua-py 维护）。

---

## 3. 接口 I2：`/api/cua/*` 逐字段

全部挂在既有的 `api := r.Group("/api", authRequired)` 下（`web/web.go:53-57`），**需要登录会话**。
逐字段的请求 / 响应样例与错误码见 [../api-contract.md](../api-contract.md) 第 10 节；下面是实现视角的要点。

### 3.1 `GET /api/cua/status` —— 环境与可用性（永远 200）

不可用不是错误，是状态：CUA 没装、Python 找不到，也返回 200，用字段表达。

| 字段 | 类型 | 说明 |
|---|---|---|
| `available` | bool | CUA 是否可用 |
| `running` | bool | 当前是否有 run 在跑 |
| `run_id` | string | 当前 run 的 id，没有则为空串 |
| `python` | object | `{found, version}` |
| `cua` | object | `{found, version}` |
| `env` | object | `{screen_w, screen_h, dpi_scale}`，用 ctypes 取主屏物理分辨率与缩放，**不依赖 pyautogui** |
| `models` | array | **只列支持视觉的模型**：`[{name, backend, vision}]`；探测不到返回空数组，不报错 |
| `notes` | array | 诊断提示 |

- 探测结果**带 30 秒缓存**（`cuaProbeTTL`），避免每次刷新都起子进程。
- 视觉模型来源是注册表 + 实测白名单（`llm.ModelSupportsVision`）：✅ 实测 `deepseek-flash` 在列，
  `deepseek-v4-pro` **不在列**（实测不支持视觉，见 §4.3）。

### 3.2 `POST /api/cua/runs` —— 起一次运行

请求体字段：

| 字段 | 类型 | 必填 | 约束 | 越界/缺失 |
|---|---|---|---|---|
| `task` | string | 是 | 去空白后非空，且 **≤ 2000 字符**（按 Unicode 码点计数） | 400 |
| `model` | string | 是 | **不允许空**；必须出现在 `status.models` 里 | 400 |
| `max_steps` | int | 否 | 1–200，默认 **20** | 400 |
| `max_seconds` | int | 否 | 10–3600，默认 **600** | 400 |
| `dry_run` | bool | 否 | 默认 false；为真时给子进程加 `--dry-run` | — |
| `allow_dangerous_hotkeys` | bool | 否 | 默认 false；转成 `CUA_ALLOW_DANGEROUS_HOTKEYS=0/1` | — |

- **同一时刻只允许一个 run**：已有 run 在跑 → **409** `{"error":"已有任务在运行"}`。
- 成功 → **201** `{"run_id":"<32 位十六进制>","model":"…","task":"…"}`。
- 子进程启动失败 → **500**，并把 run 标记为 failed。
- 注意：Go 侧默认值（20 步 / 600 秒）与 CUA 自己的预算分档（cloud 40 步 / 300 秒、local 40 步 / 1800 秒）**不是一回事**——
  Go **总是显式传** `--max-steps` / `--max-seconds`，覆盖 CUA 的分档默认值。

### 3.3 `GET /api/cua/runs/:id/events` —— SSE 实时轨迹

- 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`。
- 事件名 = CUA 的 `kind`（原样），`data:` = **原样透传的 CUA JSON 行**。
- 序列：首个事件固定 `event: start`（`{"run_id","model","task"}`）→ 历史事件 → 实时事件 → 最后固定 `event: exit`（`{"run_id","exit_code","code"}`）。
- run 不存在 → **404**；run **已结束** → 立即补发全部历史（含 `exit`）后关闭，**支持刷新页面重连**。
- **客户端断开只退订，不杀进程**：进程与 SSE 是两条独立生命周期。
- 每个订阅者有 512 条事件缓冲；缓冲满了丢给该订阅者的事件（不阻塞 CUA 进程）。

### 3.4 `POST /api/cua/runs/:id/stop` —— 停手

- 成功 → 200 `{"stopped":true}`；run 不存在 → 404；**已结束 → 200 `{"stopped":false}`**（不是错误）。
- 实现：Windows 下 `taskkill /PID <pid> /T /F` 杀**整棵进程树**。

### 3.5 `GET /api/cua/runs` —— 历史（内存）

    {"runs":[{"run_id":"…","task":"…","model":"…","status":"running|done|failed|stopped",
              "code":"E_xxx","steps":N,"started_at":"RFC3339","ended_at":"RFC3339"}]}

- 内存保留**最近 50 条**，新的在前；`ended_at` 在运行中为空串。
- ⚠️ **历史仅存内存**：**服务重启后 `/api/cua/runs` 返回空列表**（契约 §2.3）。
  前端必须把空列表当**正常空态**，不得当错误。
- ⏳ **前端消费状态（截至 2026-10-11 16:20）**：`cua.js` 只调用了 `status`、`POST runs`、`stop`（实测 grep 全文件仅 3 处 `apiJSON` 调用）。
  因此"历史列表"目前只有 HTTP 面、没有 UI 面；cua-ui 的 **task-22**「前端补 `/api/cua/runs` 历史列表」**正在进行中**（in_progress），落地后本节与 [../api-contract.md](../api-contract.md) 第 10 节需同步。

---

## 4. 接口 I3：模型代理（CUA 的"大脑"）

### 4.1 为什么要代理

CUA 子进程**没有 session cookie**，不能走 `authRequired`；也不能让子进程直接持有用户的上游 API Key。
所以新增一条内部路由：

    POST /internal/cua/llm/v1/chat/completions      （挂在 r 上，不走 authRequired）

改用**每 run 一次性 Bearer token**：

- Go 生成 32 位 hex token，经环境变量传给子进程：`CUA_MODEL_BASE_URL=http://127.0.0.1:<port>/internal/cua/llm/v1`、`CUA_MODEL_API_KEY=<token>`。
- 子进程把它当普通 OpenAI `api_key` 用；代理侧校验失败 → **401**。
- token → run 的映射存在内存里（`byToken`），用于取"创建该 run 的用户"的 Key（用户自填优先，回落服务端环境变量）。
- 端口来源：优先请求 `Host` 里的端口，其次 `APP_PORT`，最后回落 8080。

### 4.2 请求 / 响应与多模态映射

- 请求体是**标准 OpenAI chat completions（非流式）**；`messages[].content` 可能是字符串，也可能是数组。
- 数组元素：`{"type":"text","text":"…"}` 或 `{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}`。
- 实现把 data URI **解出 MIME 与原始字节**（`cuaDecodeDataURI`）→ `llm.Message.Images`；文字 part 用换行拼接。
  **只接受 `data:` URI**：代理不代下载外链图片（本地 Ollama 也只吃内联字节）。
- 未知 part 类型（audio / refusal 等）**忽略而不阻断**整次调用。
- 响应：`{"choices":[{"message":{"role":"assistant","content":"…"}}]}`。
- 上游报错 → 4xx/5xx 且 body 形如 `{"error":{"message":"…"}}`，**必须把上游真实原因带出来**
  （实现专门解包 eino / go-openai 两层的 `APIError` 与 `RequestError.Body`，避免把"上下文超限"说成"key 不对"）。

### 4.3 `max_tokens` 与空回复 ✅（Lead 实测，2026-10-11）

- 实测 `deepseek-flash` 在 `max_tokens` 过小时**返回空 content**（模型内部 reasoning 先吃掉预算）：
  同一张图同一个问题，`max_tokens=80` → 空字符串；`max_tokens=4000` → 正常；
  实测 `completion_tokens` 为 163 / 532 / 2189（差异极大）。
- **真实桌面任务上的实证（二次裁定的依据）**：下限 4096 时，一次真实 run 在 **step 12 死于 `E_MODEL_EMPTY`**，
  而轨迹显示模型当时正在正常推进；同一 prompt 三次调用 `completion_tokens = 414 / 195 / 1339`——
  **长推理步会把 4096 吃光**。CUA 默认只发 1024，所以统一抬到**不低于 16384**（契约 §3.1 二次裁定，2026-10-11）。
- 因此代理**强制下限**：`cuaMinMaxTokens = 16384`——请求里带的更小值会被**抬到 16384**（并打日志），更大的值原样使用。
  实测 16384 下同一请求正常返回（`completion=1339`，content 非空），真实 28 步 run 全程无 `E_MODEL_EMPTY`（见 §9.1）。
- **空 content 一律当错误**：返回 **502** 且 message 带稳定标记 `E_MODEL_EMPTY`，**绝不**把它当合法回复送给 CUA——
  否则现场表现会变成"模型返回结构异常"，把真正的成因掩盖掉。
- CUA 侧对应新增第 21 个错误码 `E_MODEL_EMPTY`（不可重试），见 [03-action-protocol.md](03-action-protocol.md) §5。

### 4.4 视觉模型可用性 ✅（Lead 实测，2026-10-11）

| 模型 | 视觉 | 证据 |
|---|---|---|
| `deepseek-flash` | ✅ 支持，且 GUI 坐标定位准 | 合成截图 1472×920：Save 按钮偏差 **2.2 px**、文件名输入框 **5.0 px**、Cancel 按钮 **24.2 px**，三个全部命中目标 |
| `deepseek-v4-pro` | ❌ 不支持 | 对图片答"我无法查看你上传的图片" |

- 结论：CUA 的默认视觉模型指向 `deepseek-flash`；`status.models` 必须含它、且 **不得含 v4-pro**。
- 注册表事实修正（Lead 批准）：`llm/catalog.go` 原 deepseek 端点只列 `deepseek-chat` / `deepseek-reasoner`，
  与线上实际不符；实测该端点真实返回 `deepseek-flash` / `deepseek-v4-pro`，故补入这两个 id（事实纠正），
  并把 `deepseek-v4-pro` 留在视觉白名单之外。副作用：`/api/models` 也会多出这两个模型（预期）。

### 4.5 多模态路径的既有缺陷修复 ✅（Lead 批准）

cua-go 打通本节时发现 `llm/provider.go` 的 OpenAI 多模态路径**原样调用必炸**：
go-openai 的 `ChatCompletionMessage.MarshalJSON` 不允许 `Content` 与 `MultiContent` 并存，
而 eino openai 后端会把两者一起带上 → 上游 502 `can't use both Content and MultiContent properties simultaneously`。

两处修复（均有单测 + 黑盒证据）：

1. `toSchemaMessages`：有图片时 `Content` 留空，文字改由 multi-content 的第一个 text part 承载（ollama 后端本就从 parts 取文字，行为不变）。
2. 新增 `Registry.Generate`（非流式入口）：代理是"请求非流式 + 响应非流式"，走 `Stream` 时 eino 会强制 `stream:true`，上游普通 JSON 会被读成空 content。

> 这条修复也影响 RAG 的图片问答路径（此前只跑过 ollama 后端，故未暴露）。属**修好既有隐患**，不改变 `cua/1` 协议。

### 4.6 测试缝（仅测试用环境变量）

生产**不设**这三个变量；它们让模型代理可以被黑盒验证（不打真实上游）：

| 变量 | 语义 | 默认 |
|---|---|---|
| `CUA_LLM_TOKEN` | 代理的每 run token。**已设则所有 run 都用这个值**（不再随机），并原样传给子进程 | 未设 → 每 run 随机 32 位 hex |
| `CUA_LLM_UPSTREAM` | 覆盖代理的**上游 base_url**，原样使用（OpenAI 兼容服务自带 `/v1`） | 未设 → 注册表里该端点的 BaseURL |
| `CUA_LLM_ENDPOINT_OVERRIDE` | 指定覆盖哪个端点 | `deepseek` |

生效时启动日志打印：`cua: 模型代理上游已覆盖 → endpoint=… base_url=…`。若指定的端点不存在，会打日志并**不改动**注册表。

另有进程侧的测试 / 运维缝（`web/cua.go` 顶部注释，都不改变对外 HTTP 形状）：

| 变量 | 语义 |
|---|---|
| `CUA_SIDECAR` | 显式指定 sidecar 可执行文件路径 |
| `CUA_PYTHON` | 显式指定 python 可执行文件（默认 `python`） |
| `CUA_EXTRA_ARGS` | 追加给子进程的额外参数（空格分隔），例如 `--mock` 可离线安全跑通 |

---

## 5. 接口 I4：前端「电脑操控」tab

✅ 已实测（真实 Chromium 探针 **13 PASS / 0 FAIL / 0 SKIP**）。

**真实浏览器证据**（verifier 的 [../verify/frontend-probe.mjs](../verify/frontend-probe.mjs)，2026-10-11）：
`../verify/tmp/frontend-probe-live-02.log` 汇总 **PASS=13 FAIL=0 SKIP=0**，逐项含
`F1a-page`（真实页面 / 含 cua 面板）、`F1b-js-served`（GET /static/js/cua.js → 200）、`F1c-css`、
`F2a-tab`（#/cua 入口与面板）、`F2b-js-api`、`F2c-sse-close`、`F2d-stop`、`F2e-no-new-dep`（无 CDN/import/require）、`F2f-trajectory`、
`F3a-tab-visible`（真实浏览器加载后面板可见）、`F3b-controls`（任务输入 / 模型选择 / 启动按钮等 9 个控件）、
**`F3c-dryrun-render`（dry-run 启动后轨迹渲染出条目）**、`F3d-no-js-error`（页面无 JS 异常）。
更早一次**不带 `--start`** 的采样（`tmp/frontend-probe-live-01.log`）为 12 PASS / 0 FAIL / **1 SKIP**，
SKIP 的正是 `F3c-dryrun-render`——两次对照说明该项确实由「起真实服务跑一次 dry-run」覆盖，而不是静态检查混过去的。
verifier 报告另记了一条过程事实：本轮发现并修复了 `#view-cua` 的**嵌套 bug**，修复后复测 **13 项全过**（[../verify/integration-independent.md](../verify/integration-independent.md) §1 末行）。

- 路由 `#/cua`，侧栏入口与既有 tab 同款；`VIEW_TITLES` 与 `showView()` 各加一处最小改动。
- 能力：看环境状态 / 填任务 / 选模型（**只列视觉模型**）/ 启动 / 停止 / **实时看轨迹与截图** / 看最终报告。
- 轨迹每条至少显示：步号、`thought`、`action`、`ok`/`error`、耗时。
- SSE 用原生 `EventSource`：`close()` 挂在 `beforeunload` / `pagehide` / 切走视图上；
  连续断开若干次后**停止自动重连**并提示（避免 404 时无限刷日志），提示文案明确"CUA 进程不受影响，可刷新重连"。
- 前端按 `kind` 分别处理 `run_start` / `frame` / `frame_image` / `observation` / `step` / `action_result` / … ，
  其中 `frame_image` 是独立分支（不依赖与 `frame` 配对），符合契约第 1 节。
- **不引入任何新前端依赖**，保持无构建步骤。

---

## 6. 接口 I5：启动方式与打包

### 6.1 开发态（当前可用）✅

Go 侧选子进程的两级回落（`cuaCommand`）：

1. **优先** sidecar 可执行文件：`CUA_SIDECAR` 指定的路径 → 否则找 `bin/cua-sidecar(.exe)` → 再找"可执行文件同级的 `bin/` 目录"。
2. **回落** `python -m cua.main`（`CUA_PYTHON` 可指定解释器，默认 `python`）。

因此开发态只要本机有 Python 与依赖，`go run .` 起后端、打开 `#/cua` 就能跑；离线演练可加 `CUA_EXTRA_ARGS=--mock`。

### 6.2 打包态 ✅ 已落地（安装包"装上能跑"仍 ⏳）

契约 §5 的打包链路**已落地**（task-19 于 2026-10-11 16:3x 落盘，本节按实测产物重写）：

| 环节 | 产物 / 脚本 | 实测 |
|---|---|---|
| sidecar 构建 | [../../packaging/cua.spec](../../packaging/cua.spec)（93 行）+ [../../packaging/cua_sidecar_entry.py](../../packaging/cua_sidecar_entry.py)（31 行）；`npm run build:cua-sidecar` → `python -m PyInstaller --noconfirm --distpath bin --workpath build/cua-sidecar packaging/cua.spec` | ✅ 产物 `bin/cua-sidecar.exe` 存在（**48,711,704 字节**；task-26 重建，**含 task-25 的剪贴板输入修复**） |
| Go 二进制 | [../../scripts/build-go.js](../../scripts/build-go.js)（14 行），`npm run build:go` | ✅ `bin/blog-my.exe` 存在 |
| 安装包 | `package.json` 的 `"dist": "node scripts/build-go.js && electron-builder"`；`build.win.target = nsis`，`extraResources` 把 `bin/` 与 `templates/` 打进包 | ✅ `dist/blog-my Setup 0.1.0.exe`（**185,244,782 字节**，约 176.7 MiB）+ `dist/win-unpacked/` |
| Electron 壳 | [../../electron/main.js](../../electron/main.js)（267 行）：起 Go 二进制、开窗口，退出时 `taskkill /T /F` 收树 | ✅ 已落地（task-23 修掉「安装后 app 起不来后端」） |

**sidecar 优先分支已实测生效**（[../verify/integration-independent.md](../verify/integration-independent.md) §6）：sidecar 落盘**前**，`status.notes` 明确写"未找到 sidecar，回落 python -m cua.main"；落盘**后**同一个 run **实测到 `cua-sidecar` 进程真的被拉起**（`sidecar_seen_max=1`）、`status.notes` 变为空；停止后 `leftover_sidecar=0 leftover_fakespawn=0 leftover_app=0`（无孤儿进程）。
因此 §6.1 的第一级分支**不再只是纸面设计**，而是本机上的真实路径。

`packaging/cua_sidecar_entry.py` 还有一个值得记的细节：PyInstaller 单文件 exe 不认 `-m cua.main`，而自检内部会用 `[sys.executable, "-m", "cua.main", ...]` 起子进程验证 UTF-8 往返，
入口因此把 `-m cua.main` 前缀吃掉，让**打包产物与源码态跑同一份自检、同一套判据**。

> ⏳ **安装包"装上能跑"仍缺仓库内证据**：task-23 修掉了「安装后 app 起不来后端」这个缺陷（说明这条路径**被走到过**），`dist/*.exe` 也已按 task-26 刷新；
> 但仓库里**没有**"安装 → 启动 Electron 应用 → 点 `#/cua` 跑一次"的留档，verifier 也登记「前端只在 Chromium 里验过，没在 Electron 壳里点过 `#/cua`」（报告 §9 第 6 条）。
> **终验 task-28 尚未出具**，因此本节仍标 ⏳；对外写"已支持一键分发"之前要补这一步。

---

## 7. 事件保留与内存上限 🔶（契约 §2.3 v1 补充，Lead 裁定）

- 起因：cua-ui 实测内存占用。
- 实测数据：单帧 `frame_image` **9.4 KB**，一个 200 步 run 的事件约 **1.9 MB**；按 50 条 run 上限，最坏常驻约 **95 MB**。
- 规则：**每个 run 最多保留最近 10 条 `frame_image`**（`cuaMaxFrameImages = 10`，丢掉更早的图片事件）；
  **所有非图片事件全部保留**（体积小，是轨迹回放的主体）。按此上限，50 条 run 的图片内存约 **4.7 MB**。
- 合法性：`frame_image` 在契约第 1 节就是可选事件，前端收不到也要能工作，所以**丢弃历史图片不违约**。
- 实现细节：只裁"已存储的历史快照"；**正在跑的 run 的实时流不受影响**（实时订阅者早就收到过那一帧）。
- ⏳ **历史仅存内存**：服务重启后 `GET /api/cua/runs` 为空——这是限制，不是缺陷（契约明写）。

---

## 8. 已知限制（如实汇总）

| # | 限制 | 等级 | 依据 |
|---|---|---|---|
| L1 | 历史**仅存内存**，服务重启后 `/api/cua/runs` 为空 | 🔶 契约明确定义 | 契约 §2.3；`web/cua.go` cuaManager 无持久化 |
| L2 | 每个 run 只保留最近 **10 条** `frame_image` | 🔶 契约明确定义 | 契约 §2.3；`cuaMaxFrameImages = 10` |
| L3 | 同一时刻**只允许一个 run** | 🔶 契约明确定义 | 契约 §2.2；`reserve()` 单闸门 |
| L4 | 非 `--events` 模式的人类可读输出**仍按控制台 locale 编码**（本次未改） | 🔶 既有行为 | `web/cua.go` spawn + `cua/main.py` `_force_utf8_stdio` 只在事件流模式下生效 |
| L5 | `GET /api/cua/runs` **前端未消费**（只有 HTTP 面）；task-22 进行中 | 🔶 实现现状 | 实测 `cua.js` 仅 3 处 `apiJSON` 调用 |
| L6 | **安装包"装上能跑"未验**：`packaging/`、`bin/cua-sidecar.exe`、`dist/*.exe` 均已落地，sidecar 优先分支已实测生效，但"安装 → 启动 Electron → 跑一次"无证据 | ⏳ 未验证 | 实测产物存在；[../verify/integration-independent.md](../verify/integration-independent.md) §9 第 6 条 |
| L7 | 真实 VLM 端到端操控闭环、坐标定位**系统性**评测、多显示器 / 混合 DPI、真实弹窗 OCR | ⏳ 未验证 | 见 [01-architecture.md](01-architecture.md) §13 R3/R4；[../verify/task-7-cua-contract-tests.md](../verify/task-7-cua-contract-tests.md) 第六节。注：`deepseek-flash` 已有一次坐标实测（契约 §3.2），但换分辨率 / 换模型无证据 |
| L8 | 非 Windows 平台只保证杀**直接子进程**（不算进程树） | 🔶 设计如此 | `web/cua.go` `killTree` 注释：交付目标是 Windows/Electron |
| L9 | **真实云端**与**模型输入图字节**两条链路无判据覆盖：上游一律是自建桩 / 本机 ollama，未对真实云端 `deepseek-flash` 做端到端；"子进程真正发给模型的图片字节 == 上游收到的字节"无判据 | ⏳ 未验证 | 同上报告 §9 第 2、3 条 |
| L10 | `frame_image` 上限只验到 **16 条那一次**（该 run 因 `E_STALL` 在第 15 步结束），未跑到 200 步量级；前端未在 **Electron 壳**里点过 `#/cua` | ⏳ 未验证 | 同上报告 §9 第 5、6 条 |
| L11 | 修复前的**原始代码无法亲手复跑**（修复已落地）：`0xd0@53` / 87 个 U+FFFD 那组数字是 Lead 实测，verifier 用"受控同型故障"复现同类损坏 | 🔶 归属已标注 | 同上报告 §9 第 1 条；[../verify/c14-encoding-record.md](../verify/c14-encoding-record.md) §2 |
| L12 | **Windows 动态 TCP 保留段会漂移**：实测 8300 曾 bind `EACCES`、8317 / 2343 曾**中途** `CONNREFUSED`，而 `netsh` 显示 8476–8575 等区间在变——固定端口在长链路里不可靠 | ⏳ 环境问题（有缓解） | Lead 实测（task-27 转记）。缓解：`APP_PORT=0` 让系统分配端口（Electron 壳本来就是这么做的） |
| L13 | **繁忙桌面会反复抢焦点**：真实 run 期间出现第三方安装程序、浏览器模态弹窗，模型花了大量步数对抗弹窗 | ⏳ 环境因素，**非模型能力问题** | Lead 实测（task-27 转记）；必须把环境干扰与模型能力分开结论 |

> **测试方法提醒**：自动化验收脚本**只能按 PID 杀进程**（`taskkill /PID <pid> /T /F`），不要用镜像名批量杀（`/IM`）——那会波及用户在跑的同类程序。

---

## 9. 验收证据

### 9.1 真实端到端验收（Lead，2026-10-11）✅ —— 本项目的决定性证据

统一链路**真跑通**：Electron/Go → `/api/cua/*` → `bin/cua-sidecar.exe` → 模型代理 → `deepseek-flash` → pyautogui。

- 任务「打开记事本输入 hello from CUA 保存为桌面 cua-demo.txt」→ **28 步 done**。
- 关键轨迹：第 28 步时标题栏均为 `cua-demo.txt`，编辑区内已有一行 `hello from CUA`，状态栏 `Ln 1, Col 15 / 14 characters / Plain text / UTF-8`，且**标题无未保存标记**；
  `RUN_END {"ok": true, "steps": 28}`；`TARGET_EXISTS=True  TARGET_CONTENT='hello from CUA'`。
- **独立复核（不看模型自述）**：`E:\虚拟C盘\cua-demo.txt` 存在、**14 字节**、raw bytes == `b'hello from CUA'`。
- **输入层**：6 次 `type` 动作的 `type_path` **全部是 `clipboard`**（task-25 的剪贴板路径在真实 run 里生效）。
- **门禁**：`run_end` `ok=true`，全程**没有** `E_MODEL_EMPTY`（16384 生效）。

> ⚠️ 归属与边界：以上是 **Lead 的真实环境实测**，不是 verifier 的独立复验；**终验 task-28 尚未出具**（task-23~26 之后还没有第三方复验报告）。

### 9.2 其它实测（本人复跑 / verifier 过程证据）

| 证据 | 命令 | 结果 |
|---|---|---|
| 编译 | `go build ./...` | 退出码 0，无输出 |
| 全量 Go 测试 | `go test ./... -count=1` | 退出码 0：`ok blog-my/rag 0.062s`、`ok blog-my/web 0.294s`（其余包 `[no test files]`） |
| Go 侧集成单测 | `go test ./web/ -run TestCua -count=1 -v` | **14/14 PASS**，`ok blog-my/web 0.325s`（2026-10-11 16:42 复跑） |
| CUA 离线全链路自检 | `python -m cua.main --selfcheck` | **通过 87 项 / 失败 0 项**，退出码 0（2026-10-11 19:37 本人复跑；MockScreenshot + DryRunDevice，不碰真实键鼠） |
| 契约测试套件 | `python cua/tests/run_tests.py` | **run=91 failures=0 errors=0 skipped=0**，退出码 0 |
| `E_MODEL_EMPTY` 独立复验 | `python -X utf8 cua/tests/it_model_empty.py` | **判据 18 组 / 问题 0 项**，退出码 0（含变异自检：坏实现会被抓住） |
| 字节级编码复验（stdout） | `python -X utf8 docs/verify/events-jsonl-check.py …` | E8a/E8b/E8c/E8e 全 **PASS**；负向对照 **11/11** 全被抓（verifier，[../verify/c14-encoding-record.md](../verify/c14-encoding-record.md)） |
| 字节级编码复验（Go/SSE） | `node docs/verify/integration-probe.mjs` | C14a/C14b/C14c 全 **PASS**（35234 字节、22 码点全等、18 条事件）；负向对照 **16/16** 全被抓 |
| 前端真实浏览器探针 | `node docs/verify/frontend-probe.mjs` | **PASS=13 FAIL=0 SKIP=0**，含 dry-run 渲染（`../verify/tmp/frontend-probe-live-02.log`） |
| 集成层独立复验（报告） | verifier task-18（黑盒、自建桩） | **PASS=39 FAIL=0 WARN=0 SKIP=0**；门禁 build/vet/test 全 0、gofmt 干净（[../verify/integration-independent.md](../verify/integration-independent.md)） |
| stop 收进程树 | `docs/verify/tmp/treekill-probe.mjs` | 假子进程再起孙进程：stop 前两个 PID 都 `alive=true`、stop 后都 `alive=false`，`Get-Process` 残留 **0** |
| `frame_image` 上限（15 步 run） | `docs/verify/tmp/memcap-probe.mjs` | 实时 SSE **16 条不裁**、事后重放 **10 条**（保留最近 `[6..15]`）、非图片事件 **82 = 82 全留** |
| §3.4 多模态修复未伤 ollama 链路 | `docs/verify/tmp/provider-probe.go` | P1 ollama 文本 → "收到"；P2 ollama + 真 1x1 JPEG → **"这张图是黑色的。"**；P3 openai 后端 body 的 `content` 是**数组** |

> **集成层的独立复验报告已出具**：[../verify/integration-independent.md](../verify/integration-independent.md)（**191 行**，verifier，task-18，2026-10-11 16:3x）。
> 结论：**全量集成探测 PASS=39 / FAIL=0 / WARN=0 / SKIP=0**（normal 上游 + 固定 token，含 **emoji 向量**）；`--selfcheck` **84/0**（**报告出具时点**，task-25 后为 **87/0**）；契约套件 **91/0**；§7 门禁 `go build` / `go vet` / `go test -count=1` **全 0**、`gofmt -l` 干净。
> 负向对照：集成层 **16/16**、事件流 **11/11**、套件自带 **11/11** 全被抓（报告原话："没有对照的绿不写进判定表"）。
> 报告 §9 另列了它**没能验证**的 7 条，本文已折进 §8 的 **L6–L11**；引用集成轮结论请以该报告为准。
> 另：`integration-baseline.log` 早前采样里记过一次 `go test ./...` 变红（`TestCuaLLMProxyEmptyContentIsError`），报告 §7 门禁为 0，本人 2026-10-11 16:32 复跑亦为**绿**（退出码 0），该红未在当前代码上复现。

Go 侧 14 个用例覆盖（含 `cuaSidecarResolution`：sidecar 路径解析与回落）：`cuaDecodeDataURI` / `cuaConvertMessages` / `cuaUpstreamErrorInfo` / `cuaVisionModels` /
`cuaCreateRunBoundaries`（400/409 边界）/ `cuaStatusAlwaysOK` / 代理鉴权 / mock 后端正常路径 /
**空 content 必须报错** / 上游原因透传 / **图片历史 10 条上限** / 空列表仍是数组 / **max_tokens 下限**。

---

## 10. 参考

- [06-integration-contract.md](06-integration-contract.md) —— 集成契约 v1（冻结，本文的依据）
- [03-action-protocol.md](03-action-protocol.md) —— 动作协议 `cua/1`（21 个错误码，含 `E_MODEL_EMPTY`）
- [../api-contract.md](../api-contract.md) 第 10 节 —— `/api/cua/*` HTTP 契约逐字段
- [01-architecture.md](01-architecture.md) —— CUA 自身架构、预算分档与风险 R1–R8
- [../verify/task-7-cua-contract-tests.md](../verify/task-7-cua-contract-tests.md) —— verifier 独立契约测试报告（含未覆盖面）
- [../verify/c14-encoding-record.md](../verify/c14-encoding-record.md) —— C14 非 ASCII 往返保真验证记录（字节级判据 + 负向对照）
- [../verify/cua-independent.md](../verify/cua-independent.md) —— task-9 框架 / 文档一致性独立复验报告。**注意**：其 `selfcheck 81/0` 与「20 个错误码」是**当时快照**；截至 2026-10-11 19:37 为 **87/0** 与 **21 码**
- [../verify/frontend-probe.mjs](../verify/frontend-probe.mjs) —— 前端真实浏览器探针（本次 13 PASS / 0 FAIL / 0 SKIP）
- [../verify/integration-independent.md](../verify/integration-independent.md) —— **集成层独立复验报告**（task-18，191 行）：全量探测 PASS=39 / FAIL=0，§9 列出 7 条未覆盖面（本文 §8 L6–L11 即其折入）
