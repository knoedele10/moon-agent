# AI 自主工具调用 + 记忆系统：接口契约 v1.1（冻结）

> Lead 裁定，2026-10-12。**本文件冻结后各写域并行**；改动需 Lead 复核并升版本。

## 变更记录

| 版本 | 变更 | 依据 |
|---|---|---|
| v1 | 初版冻结 | Lead |
| **v1.1** | 补 `StreamFunc` 注入缝、`ErrToolDefInvalid`、`RoleTool`、mock 脚本缝；`MaxToolCalls` 措辞改为「整次运行累计」；补环境变量名；补 `roleToolTrace` 轨迹落库约定 | 实现期由 cua-arch / cua-code / cua-go / Lead 提出，Lead 逐条批准 |

### v1.1 新增条款

**A. `agent.StreamFunc` 注入缝（cua-arch 补，契约 v1 遗漏）**
v1 的 `Config`/`Run` 里没有"谁来调模型"的入口，而验收又要求"用假模型单测循环"。补：

```go
// 签名与 (*llm.Registry).Stream 完全一致
type StreamFunc func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error)

func SetStreamer(f StreamFunc)   // 进程级单例，仅应在启动时设置一次；测试内并发用例请自行串行
func Streamer() StreamFunc
func RegistryStreamer(r *llm.Registry) StreamFunc
```
生产由 web 启动时装：`agent.SetStreamer(agent.RegistryStreamer(registry))`。

**B. `llm` 包新增导出符号（cua-code 补）**
- `ErrToolDefInvalid`：ToolDef 缺 name，或 parameters 不是合法 JSON Schema。
- `RoleTool = "tool"`：v1 注释提到 role=tool 却没给常量。
- `MockTurn{ToolCalls, Text, Err}` + `SetMockScript`/`MockScript`/`ClearMockScript`：mock 后端的工具脚本缝。
  **轮** = 输入里携带 `ToolCalls` 的 assistant 消息条数；脚本用完回落默认回显。
- 硬门：`req.Tools` 为空时**完全不归集** tool_calls 分片（取 v1「一字节行为不变」的字面义）。

**C. `MaxToolCalls` 措辞更正**
v1 写「一轮内总调用上限」有歧义。**更正为「整次运行累计」**：次数按 `Run` 全程累加，达到上限即发 `EvError` 并停止。

**D. 环境变量（web 接线层实现）**

| 变量 | 含义 | 缺省 |
|---|---|---|
| `AGENT_TOOLS` | 总开关，认 `1/true/on/yes` | 关闭 |
| `AGENT_FS_ROOT` | 文件工具根目录；**未配则文件工具不注册** | 空 |
| `AGENT_MAX_ITERATIONS` | 覆盖默认 12 | 0 → 用默认 |
| `AGENT_MAX_TOOL_CALLS` | 覆盖默认 24 | 0 → 用默认 |

**E. 工具轨迹落库（Lead 定）**
工具调用轨迹用 `role="tool_trace"` 单独落一行，`Content` 是 JSON 数组：
`[{"name","ok","summary","detail"}]`。
**历史重建时必须跳过这个 role**——否则模型会把自己上一次的工具输出当成对话正文读进去。
不新增数据库列（`message` 表结构不变，无迁移风险）。

## 0. 边界与红线

| 项 | 规定 |
|---|---|
| **CUA（电脑操控）** | **完全冻结，一行不改**。`web/cua.go`、`web/cua_llm.go`、`web/cua_test.go`、`cua/**`、`packaging/**`、`electron/**`、`templates/static/js/cua.js` 的 CUA 逻辑均不动。视为已分叉的独立模块。 |
| **向后兼容** | `llm.Registry.Stream` / `Generate` 的**现有行为不得改变**（CUA 的模型代理依赖 `Generate`）。新增能力只能靠**追加字段/新增方法**。 |
| **安全** | 文件工具**只允许在配置的根目录内操作**，必须拒绝 `..` 穿越、根外绝对路径、符号链接逃逸。这是红线，需独立复验。 |
| **默认安全** | 工具默认**关闭**，由环境变量开启；未开启时行为与现在完全一致。 |

## 1. 需求（用户原话拆解）

1. CUA 保留不动，当分出来的东西。
2. **知识库与对话合二为一**——知识库是 **AI 的记忆系统**。
3. **API（密钥）按钮接到右上角**。
4. 实现 `fs/tool-fs` 的功能（read / write / edit / read-image）：**AI 根据用户对话自主判断是否用工具**，参考 DSH 的判断方法。

## 2. `llm` 包扩展（写域：`llm/**`）

```go
// 新增：一次工具调用
type ToolCall struct {
    ID        string `json:"id"`
    Name      string `json:"name"`
    Arguments string `json:"arguments"` // 原始 JSON 字符串，不预先解析
}

// 新增：暴露给模型的工具定义
type ToolDef struct {
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// 扩展 Message（纯追加，现有字段不动）
type Message struct {
    Role       string     `json:"role"`   // system | user | assistant | tool
    Content    string     `json:"content"`
    Images     []Image    `json:"-"`
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`  // assistant 消息携带
    ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时必填
    ToolName   string     `json:"tool_name,omitempty"`
}

// 扩展 ChatRequest（纯追加）
type ChatRequest struct {
    Model       string
    Messages    []Message
    Temperature float64
    MaxTokens   int
    Tools       []ToolDef // 新增；为空时行为与现在完全一致
}

// 新增事件类型
const EventToolCall EventType = "tool_call"

type Event struct {
    Type     EventType
    Content  string
    ToolCall *ToolCall // 仅 EventToolCall 时非空
    Err      error
}

var ErrToolsUnsupported = errors.New("llm: 该后端不支持工具调用")
```

**`Stream` 的扩展规则（关键）**：
- `req.Tools` 为空 → 走原路径，**一字节行为不变**。
- `req.Tools` 非空 → 后端须实现 `model.ToolCallingChatModel`（Eino v0.9.21），
  用 **`WithTools()`（不可变、并发安全）**，**不要用已废弃的 `BindTools`**。
- 后端不支持时返回 `ErrToolsUnsupported`。
- 流式响应里 `schema.Message.ToolCalls` 会分片到达，按 `ToolCall.Index` 归并后再发 `EventToolCall`。

**mock 后端必须支持工具调用**（测试缝）：由请求里的脚本决定第几轮返回哪个工具调用，
使整条 agent 循环可在无真实模型的情况下确定性测试。

## 3. `agent` 包（写域：`agent/**`，不含 `agent/fs/**`）

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() map[string]any // JSON Schema object
    // args 是原始 JSON；由工具自己解析并校验。
    Execute(ctx context.Context, args json.RawMessage, ex *Exec) (Result, error)
}

type Result struct {
    Content string         `json:"content"`  // 给模型看的文本（工具结果的正文）
    Display map[string]any `json:"display,omitempty"` // 给前端看的结构化摘要（可选）
    IsError bool           `json:"is_error"`
    // 若工具产生了图片（read_image），放在这里；循环会把它作为一条 user 消息追加
    Image *ImagePart `json:"-"`
}

type ImagePart struct {
    MIME string
    Data []byte
}

type Exec struct {
    SessionID string
    Root      string // 工具可操作的根目录（绝对路径）
    Signal    context.Context
}

type EventKind string
const (
    EvDelta      EventKind = "delta"
    EvToolCall   EventKind = "tool_call"
    EvToolResult EventKind = "tool_result"
    EvDone       EventKind = "done"
    EvError      EventKind = "error"
)

type Event struct {
    Kind      EventKind
    Content   string          // delta
    ToolCall  *llm.ToolCall   // tool_call
    ToolName  string          // tool_result
    ToolID    string          // tool_result
    OK        bool            // tool_result
    Summary   string          // tool_result：给前端一行摘要
    Detail    string          // tool_result：完整结果（前端折叠展示）
    Display   map[string]any  // tool_result
    Err       error
}

type Config struct {
    Model         string
    APIKey        string
    SystemPrompt  string
    MaxIterations int // 0 → 默认 12
    MaxToolCalls  int // 0 → 默认 24（一轮内总调用上限，防失控）
    Tools         []Tool
    Exec          *Exec
}

// Run 跑完整循环：模型 → 若有 tool_calls 则执行 → 结果回灌 → 再来一轮，
// 直到模型给出纯文本回复或触顶。事件通过 emit 回调投递（同步调用，调用方负责并发安全）。
func Run(ctx context.Context, cfg Config, history []llm.Message, emit func(Event)) error
```

**循环语义（必须遵守）**：
1. 每轮把 `history` 发给模型（带 `Tools`）。
2. 该轮若产生 `tool_calls`：先发 `EvToolCall`，执行每个工具，发 `EvToolResult`，
   把 assistant(tool_calls) + 每条 tool 结果**追加进 history**，进入下一轮。
3. 该轮若是纯文本：`delta` 已实时发出，发 `EvDone`，结束。
4. 触顶（迭代/调用数）→ 发 `EvError`，错误文案要说明触顶原因。
5. **工具报错不回滚循环**：把错误作为 `is_error=true` 的 tool 结果回灌给模型，让它自己决定下一步。
6. 每个工具执行前检查 `ctx` 取消。

## 4. `agent/fs` 包（写域：`agent/fs/**`）

四个工具，名字与参数**照抄 DSH**：

| 工具 | 参数 | 输出 |
|---|---|---|
| `read` | `file_path`(必), `offset`?, `limit`? | 带行号的文本（`N\t内容`），默认最多 2000 行 |
| `write` | `file_path`(必), `content`(必) | Created / Updated |
| `edit` | `file_path`(必), `old_string`(必), `new_string`(必), `replace_all`? | 成功文案；`replace_all=false` 时 `old_string` 必须唯一匹配 |
| `read_image` | `file_path`(必) | 文本确认 + 把图片作为 `Result.Image` 返回 |

**校验（照 DSH）**：`file_path` 非空白；`edit` 的 `old_string` 非空且 **`old_string != new_string`**；`write` 允许空 `content`。

### 4.1 路径沙箱（红线）
- 解析后必须落在 `Exec.Root` 之内；用 `filepath.EvalSymlinks` 后再判前缀。
- 拒绝：根外绝对路径、`..` 穿越、符号链接逃逸、非普通文件（目录/设备）。
- 违反 → 错误码 `FS_SANDBOX_DENIED`，文案形如 `[sandbox: 拒绝访问根目录之外的路径]`。

### 4.2 观察策略（照 DSH 的 `fs-observation-policy`）
每会话（`SessionID`）一张表：`map[targetKey]Observation`，`Observation{Kind: present|absent, Version string}`。
**三态而非两态**——"确认不存在"必须与"没见过"区分：

| 状态 | write | edit |
|---|---|---|
| 没见过 | `createIfAbsent` | `FS_NOT_OBSERVED` |
| 确认不存在 | `createIfAbsent` | `FS_NOT_FOUND` |
| 存在 | `replaceIfVersion(v)` | CAS 基准 `v` |

- `read` **成功**→ 记 `present{version}`；`read` 失败（不存在）→ 记 `absent`（**照 DSH：读不到也要记**）。
- `write`/`edit` 成功后 → 用写后的版本更新记录。

### 4.3 版本令牌（照 DSH）
```go
// dev:ino:size:mtimeNs:ctimeNs
func versionOf(fi os.FileInfo) string
```
Windows 上 `syscall.Win32FileAttributeData` 可取 `LastWriteTime`/`CreationTime`（100ns 精度）；
Go 的 `os.FileInfo.Sys()` 在 Windows 返回 `*syscall.Win32FileAttributeData`。
**跨平台**：非 Windows 用 `*syscall.Stat_t` 的 `Dev/Ino/Size/Mtim/Ctim`。

### 4.4 原子写（照 DSH 的 `writeFileAtomic`）
1. 同目录建暂存文件 `.名字.随机.tmp`，写入 → **`f.Sync()`** → `Close()`
2. 发布：
   - `createIfAbsent` → **`os.Link(temp, target)`**（原子"不覆盖创建"）；
     `EEXIST` → `FS_NOT_OBSERVED`（"不能覆盖没读过的文件"）
   - 覆盖 → `os.Rename(temp, target)`（Windows 上 `os.Rename` 会覆盖，Go 1.20+ 语义）
3. 清理暂存文件；**提交后清理失败要吞掉**（不能把成功的写变成失败）

### 4.5 错误码与面向模型的措辞（照 DSH 的 `remediateFsError`）
provider 只管原因，**给模型看的话由工具层统一**：
- `FS_NOT_OBSERVED` → `cannot modify "X": file has not been read — read the file, then retry`
- `FS_STALE_VERSION` → 保留原因 + ` — re-read the file, then retry`
- 错误码原样保留供机器路由。

### 4.6 编辑 = 一个临界区
`edit` 必须在**同一把每目标锁**内完成：探版本 → 校验版本守卫 → 读 → 字面量匹配 → 重写。
**版本守卫必须在字面量匹配之前**——陈旧编辑要报 `FS_STALE_VERSION`，绝不报"找不到匹配"。

## 5. SSE 事件契约（写域：`web/**`）

现有 `delta` / `done` / `error` **保持不变**（前端向后兼容）。**新增两个事件**：

```
event: tool_call
data: {"id":"call_abc","name":"read","arguments":{"file_path":"a.txt"}}

event: tool_result
data: {"id":"call_abc","name":"read","ok":true,"summary":"读取 a.txt（12 行）","detail":"1\thello\n2\t...","display":{}}
```

`POST /api/chat` 的请求体**追加**一个可选字段 `tools`（bool，默认 false）：
- `false`/`省略` → 现有行为，一个字节不变。
- `true` → 走 agent 循环。

**记忆自动注入**：`tools=true` 时，用用户这条消息检索记忆库 top-k，
以 `<memory>…</memory>` 块拼进 system prompt。检索失败**不得阻断对话**（降级为无记忆）。

## 6. 记忆（知识库）契约（写域：`web/kb.go`、`rag/**`）

- **路由保持不变**（`/api/kb/*` 全部保留，避免破坏）。
- 定位改为「AI 的记忆系统」：前端从独立页签**移入对话视图**。
- 新增两个工具（由 AI 自主决定何时调用）：
  - `memory_search(query, limit?)` → 检索记忆，返回条目文本
  - `memory_write(text, tags?)` → 写入一条记忆
- 自动注入与工具调用**共存**：注入解决"顺带想起"，工具解决"主动翻找"。

## 7. 前端契约（写域：`templates/**`）

| 改动 | 要求 |
|---|---|
| 页签 | 由 5 个减为 **3 个**：`对话` | `模型` | `电脑操控`。移除 `密钥`、`知识库` 页签 |
| 密钥 | 移到**右上角**按钮（`#/keys` 路由仍可达，但入口在右上角；点开为面板/弹层） |
| 记忆 | 并入**对话视图**（可折叠面板），复用现有 `kb.js` 的能力 |
| 工具卡片 | 对话流里为每次 `tool_call`/`tool_result` 渲染一张卡片：工具名 + 参数摘要 + 状态 + 可折叠的完整结果 |
| CUA | `#/cua` 视图与 `cua.js` 的 CUA 逻辑**不动**（含测试版角标与警告弹窗） |

## 8. 写域划分（禁止越界）

| 成员 | 写域 |
|---|---|
| **Lead** | `docs/agent/**`、`docs/api-contract.md`、本契约 |
| llm 工具调用 | `llm/**` |
| agent 核心 | `agent/*.go`（**不含** `agent/fs/**`） |
| fs 工具 | `agent/fs/**` |
| 记忆 | `web/kb.go`、`rag/**` |
| web 接线 | `web/chat.go`、`web/web.go`（仅路由行）、新增 `web/agent.go` |
| 前端 | `templates/**`（**不含** `templates/static/js/cua.js` 的 CUA 逻辑） |
| 验证 | `docs/verify/**`、`agent/**/*_test.go`、`llm/**/*_test.go` |

## 9. 验收标准（§7 门禁）

1. `go build ./...` / `go vet ./...` / `go test -count=1 ./...` / `gofmt -l .` 全清。
2. **CUA 未受影响**：`go test ./web/ -run TestCua -count=1` 全过；`git diff` 里 CUA 相关文件**零改动**。
3. **向后兼容**：`tools` 缺省时 `/api/chat` 的 SSE 事件序列与改造前一致。
4. **工具真能跑**：用 mock 后端脚本化一次 `read` + 一次 `write`，断言文件系统真的变了、事件序列正确。
5. **观察策略生效**：未读先写 → `FS_NOT_OBSERVED`；读后文件被外部改 → `FS_STALE_VERSION`；读缺失文件后 `write` 可创建、`edit` 报 `FS_NOT_FOUND`。
6. **沙箱红线**：`..` 穿越、根外绝对路径、符号链接逃逸**全部被拒**（负向对照必须有）。
7. **记忆**：`memory_search`/`memory_write` 可用；检索失败不阻断对话。
8. **前端**：3 个页签、密钥在右上角、记忆在对话视图内、工具卡片能渲染；CUA 页签与弹窗仍正常。
9. 负向对照：全绿必须配负向对照才算证据。
