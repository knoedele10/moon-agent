# AI 自主工具调用 + 记忆系统：设计说明（01-design）

> **依据**：[00-contract.md](00-contract.md)（接口契约 v1，**冻结**，Lead 裁定 2026-10-12）。本文只解释**为什么这样设计**与**与 DSH 的对应关系**；出现冲突时以契约为准。
>
> 🔶 = 契约/本文已定义的设计；✅ = 本次实际跑过的命令；⏳ = 尚未验证。
> 采样时间：**2026-10-12 14:4x–14:50:41（Asia/Shanghai）**。**实现状态见 §0**：核心、前端与 `web/agent.go` 接线**均已落地**；14:50:41 快照下 build / vet / 全部单测 / CUA 门禁**全通过**。

---

## 0. 实测状态（本人复跑，2026-10-12 14:50:41）

### 0.1 门禁

| 命令 | 结果 |
|---|---|
| `go build ./...` | 退出码 **0** ✅ |
| `go vet ./...` | 退出码 **0** ✅ |
| `go test ./... -count=1` | 退出码 **0** ✅（`agent` 4.539s / `agent/fs` 4.868s / `llm` 4.663s / `rag` 3.924s / `web` 4.257s 全 `ok`） |
| **CUA 红线** `go test ./web/ -run TestCua -count=1` | **`ok blog-my/web 0.562s`**，退出码 **0** ✅ —— CUA 模块未被碰坏 |

通过用例数（`-v` 计 `--- PASS`）：`agent` **14**、`agent/fs` **27**、`llm` **15**、`rag` **26**，共 **82**。

> **负向对照为什么可信**：14:47–14:49 之间我**亲眼看到过红**——`TestSandboxRejectsSymlinkEscape`（软链接逃逸是否被拒，**契约 §0 红线**）、`TestConcurrentEditsOneWins`、以及 `llm` 的 `TestT36Merge*`（乱序 / 无 Index 分片归并）当时都在 FAIL，实现补齐后转绿。**先红后绿**证明这些用例确实在断言行为，而不是恒真的绿。
> 代价是：这段时间仓库在**并行开发中**（`agent/fs/tools.go` 与 `web/agent.go` 的 mtime 就在 14:48），所以**任何单点快照都可能立刻过期**。

### 0.2 已落地文件（行数实测）

| 文件 | 行数 | 作用 |
|---|---|---|
| `agent/loop.go` | 350 | 主循环 `Run`、六条语义、两道上限 |
| `agent/tool.go` | 165 | `Tool`/`Result`/`Exec`/`Registry` |
| `agent/stream.go` | 45 | **`StreamFunc` 缝合点**（见 §8.1） |
| `agent/fs/observation.go` | 82 | 三态观察表 |
| `agent/fs/errors.go` | 86 | 9 个错误码 + 面向模型的文案（见 §6.1） |
| `agent/fs/sandbox.go` | 108 | 路径沙箱 |
| `agent/fs/atomic.go` | 61 | 原子写 |
| `agent/fs/version.go` + `version_windows.go` + `version_unix.go` + `version_other.go` | 28 + 20 + 21 + 10 | 版本令牌（分平台） |
| `llm/tools.go` | 199 | `ToolCall`/`ToolDef`/`withTools`/分片归并 |
| 前端 `templates/chat/index.html` | 322 | 3 页签 + 右上角密钥弹层 |
| 前端 `templates/static/js/chat.js` | — | 工具卡片（`:621-790`）、记忆面板（`:36-37, 418-433`） |
| `agent/fs/tools.go` | — | **四个 fs 工具本体**（read/write/edit/read_image） |
| `agent/fs/fs_test.go` + `sandbox_test.go` | — | fs 单测 + **沙箱负向对照**（含软链接逃逸） |
| `agent/loop_test.go` | — | 循环语义的假模型脚本化单测（14 个） |
| `llm/tools.go` 的测试与 `llm/` 其他 | — | llm 15 个用例（含分片归并负向对照 `TestT36Merge*`） |
| `rag/memory.go` + `rag/memory_tool.go` | — | 记忆注入（`<memory>`、top-k=5/上限 20）+ 两个记忆工具 |
| `web/agent.go` | — | **接线层**：双闸门、工具组装、SSE 转发、落库 |

### 0.3 尚未落地 / 尚未验证 ⏳

- ~~`web/agent.go` 不存在~~ → **已落地并接线**：`web/chat.go:125` 收 `tools` 字段、`:207-210` 在 `tools && AGENT_TOOLS` 时调 `runAgentChat`（`web/agent.go:206`）。
- ~~`agent/fs` 无测试文件~~ → **已有 27 个用例且当前通过**（含沙箱负向对照）。但那是**实现方自己的单测**，**独立复验尚未做** ⏳。
- **真实模型仍未验** ⏳：模型是否真的"自主判断"该用工具，**未用真实模型跑过**（`agent` 的 14 个用例用假模型脚本——确定性测试缝，不等于真实模型行为）。

---

## 1. 要解决什么

用户四条需求 → 本项目的落点：

| # | 需求（原话） | 落点 | 状态 |
|---|---|---|---|
| 1 | CUA 保留不动，当分出来的东西 | CUA 文件**一行不改**（契约 §0 红线）；两者唯一交集是 `llm.Registry`，只做**追加**扩展 | ✅ 实测 `TestCua` 通过 |
| 2 | 知识库与对话合二为一，知识库是 **AI 的记忆系统** | `/api/kb/*` 路由**不变**；前端由独立页签移入对话视图 | 前端 ✅ 已改（记忆面板）；后端记忆工具 ⏳ 未见落地 |
| 3 | API（密钥）按钮接到右上角 | 页签 5 → **3** 个；`#/keys` 仍可达（弹层） | ✅ 实测：页签为 对话/模型/电脑操控 |
| 4 | 实现 `fs/tool-fs` 的 read/write/edit/read-image，**AI 自主判断** | `agent` 循环 + `agent/fs` 四工具 + `llm` 工具调用 | 核心 ✅ 已落地 / 接线 ⏳ |

**为什么"自主判断"要落在后端循环里**：判断依据是模型对**工具返回内容**的理解——它得先 `read` 看到文件内容，才知道下一步 `edit` 什么。
所以不是"前端问一次要不要用工具"，而是**服务端多轮循环**（契约 §3）。

---

## 2. 架构总览 🔶（结构已落地，接线待完成）

```
浏览器（3 个页签：对话 | 模型 | 电脑操控；密钥在右上角）
   │  POST /api/chat {conversation_id, model, content, tools:true}
   ▼
web/agent.go（接线：组装 Config、转发 agent.Event → SSE）        ← ⏳ 尚未落地
   │
   ├─▶ 记忆检索（top-k）→ 拼进 system prompt 的 <memory>…</memory>    ← 失败不阻断
   ▼
agent.Run 循环（agent/loop.go）                                  ← ✅ 已落地
   │  每轮：history + Tools → StreamFunc(...)（agent/stream.go）
   │        ├─ 有 tool_calls → 发 EvToolCall → 执行工具 → 发 EvToolResult → 结果回灌 → 下一轮
   │        └─ 纯文本 → delta 已实时发出 → EvDone → 结束
   ▼
模型后端（llm/tools.go：WithTools 新实例）                        ← ✅ 已落地；不支持 → ErrToolsUnsupported
   ▲
   │ 工具注册表：agent.Registry
   ├── agent/fs：read / write / edit / read_image（沙箱 + 观察策略 + 原子写）   ← ✅ 已落地
   └── memory：memory_search / memory_write                                    ← ⏳ 未见落地
```

| 层 | 写域 | 职责 | 状态 |
|---|---|---|---|
| `llm` | `llm/**` | 工具调用的**传输能力**（`Tools` 字段、`EventToolCall`、按 Index 归并分片） | ✅ `llm/tools.go` 199 行 |
| `agent` | `agent/*.go` | 循环语义、事件、上限、触顶 | ✅ `loop.go`/`tool.go`/`stream.go` |
| `agent/fs` | `agent/fs/**` | 四工具的沙箱/观察/原子写 | ✅ 代码已落地，**无测试** ⏳ |
| 记忆 | `web/kb.go`、`rag/**` | 检索/写入记忆 | ⏳ 工具侧未见落地 |
| 接线 | `web/chat.go`、`web/agent.go` | `tools` 开关、SSE 转发 | ⏳ **未落地** |
| 前端 | `templates/**` | 工具卡片、记忆面板、右上角密钥 | ✅ 已落地 |

---

## 3. 核心设计一：intent 与 mutation 分离 🔶

**问题**：`write` 面对一个文件，可能有两种合法意图——"没有就创建"与"有就覆盖"；`edit` 则必须先确认自己看过这个文件。
如果让"读文件状态"和"改文件"挤在一个函数里，就会出现**用旧状态做新决定**的竞态。

**做法**：把一次写操作拆成两段，中间隔着一次明确的决策：

| 段 | 输入 | 输出 |
|---|---|---|
| **intent（决策）** | 本会话对该目标的观察记录 | `createIfAbsent` 或 `replaceIfVersion(v)`；`edit` 还可能是**拒绝** |
| **mutation（执行）** | intent + 参数 | 原子落盘；成功后用**写后的版本**更新观察记录 |

**与 DSH 对照**（源码实测，采样 2026-10-12）：

- `dsh-fs-observation-policy/lib/index.js:48-49` 原文：
  `Decide the write intent: unseen or confirmed absent ⇒ createIfAbsent; confirmed present ⇒ replaceIfVersion at the observed version.`
- 同文件 `:54-57`：`return prior?.kind === "present" ? {…} : { kind: "createIfAbsent" }`
- 同文件 `:60-68`：`edit` 的版本守卫同样先决策——**没见过报 `FS_NOT_OBSERVED`、确认不存在报 `FS_NOT_FOUND`、存在则给出基准版本**。

**本项目在此基础上强化的一条**（契约 §4.6，已在 `agent/fs/errors.go:5` 的包注释里写成结构性约束）：
`edit` 是**一个临界区**，且**版本守卫必须在字面量匹配之前**——陈旧编辑必须报 `FS_STALE_VERSION`，**绝不能**报"找不到匹配"。
理由：后者会把"你的视图过期了"误诊成"这文件里没有这段文字"，模型会据此改成"换个字符串再试"，于是**在错误的文件版本上反复空转**。

---

## 4. 核心设计二：观察策略三态 🔶

**两态是不够的**。"没读过这个文件"与"读过、确认它不存在"是**不同的世界状态**，两态会把它们压成同一个"不存在"：

- 对 `edit`：把"没读过"当成"不存在" → 模型收到"文件不存在"，可能去 `write` 一个新文件，**把用户原有内容盖掉**。
- 对 `write`：把"没读过"当成"存在" → 直接覆盖一个**模型从未看过的**文件。

所以每会话的观察表是 `map[targetKey]Observation`，`Observation{Kind: present|absent, Version}`，**`Kind` 缺失即"没见过"**（契约 §4.2，已落地于 `agent/fs/observation.go`）：

| 状态 | `write` 的 intent | `edit` |
|---|---|---|
| **没见过**（表中无记录） | `createIfAbsent` | **拒绝** `FS_NOT_OBSERVED` |
| **确认不存在**（`absent`） | `createIfAbsent` | **拒绝** `FS_NOT_FOUND` |
| **存在**（`present{v}`） | `replaceIfVersion(v)` | 以 `v` 为 CAS 基准 |

**记录的时机（照 DSH）**：`read` **成功** → 记 `present{version}`；`read` **失败（文件不存在）** → **也要记** `absent`。
第二句是这套机制里最容易被漏掉的一半——**"失败"同样是一条必须记住的观察**，否则"读过、确实没有"会被降级回"没见过"，
`edit` 就会在两个错误之间反复横跳（已写成 `agent/fs/errors.go:4` 的结构性约束）。

**跨会话隔离**：观察表按 `SessionID` 分表，避免 A 会话的读取替 B 会话背书。

---

## 5. 核心设计三：原子写 🔶

目标：**要么旧内容完整，要么新内容完整**，不允许出现被截断的半个文件（进程崩溃、断电、并发写）。

流程（契约 §4.4，已落地于 `agent/fs/atomic.go`）：

1. 在**同一目录**建暂存文件 `.名字.随机.tmp`（同目录才能保证 rename 是原子的），写入 → `f.Sync()` → `Close()`
2. 发布：
   - `createIfAbsent` → `os.Link(temp, target)`：**原子地"不覆盖创建"**；若目标已存在 → `EEXIST` → 返回 `FS_NOT_OBSERVED`（"不能覆盖没读过的文件"）
   - 覆盖 → `os.Rename(temp, target)`
3. 清理暂存文件；**提交成功之后的清理失败必须被吞掉**——不能让一次成功的写，因为删不掉临时文件而变成失败。

**与 DSH 对照**（源码实测）：

- 骨架一致：`dsh-fs-local/lib/index.js:541` 有 `await handle.sync()`，`:559/:561` 用 `await rename(tempPath, absolutePath)` 发布；
  `dsh-atomic-write/lib/index.js:35` 同样是 `await rename(temp, filename)`，`:66` 的暂存名形如 `\${filename}.\${randomBytes(6).toString("hex")}.tmp`。
- **差异（本项目追加，不是照抄）**：`createIfAbsent` 用 **`os.Link`** 实现"不覆盖创建"。DSH 的 `dsh-atomic-write` 实测走的是 `rename`；
  本项目为了让"没读过就不许覆盖"这条守卫**在文件系统层也是原子的**（而不是只靠进程内的观察表），追加了这一分支。契约 §4.4 明确写了它。

### 5.1 版本令牌 🔶（已落地：`agent/fs/version*.go`，分平台 4 文件）

```
dev:ino:size:mtimeNs:ctimeNs
```

**与 DSH 逐字一致**：`dsh-fs-local/lib/index.js:145-146` 实测 `function versionOf(info) { return FsVersion(\`\${info.dev}:\${info.ino}:\${info.size}:\${info.mtimeNs}:\${info.ctimeNs}\`); }`。

- **Windows**：`os.FileInfo.Sys()` 返回 `*syscall.Win32FileAttributeData`，取 `LastWriteTime` / `CreationTime`（100ns 精度）。
- **非 Windows**：`*syscall.Stat_t` 的 `Dev/Ino/Size/Mtim/Ctim`。
- **为什么带 `size`**：同秒内的两次写入若 `mtime` 精度不够，`size` 变化仍能把版本区分开；`ctime` 则兜住"内容没变但被替换"的情形。
- ⏳ `version_other.go`（既不 Windows 也非 unix 的平台）的具体降级策略**未读、未验**。

---

## 6. 核心设计四：面向模型的错误措辞 🔶

**原则：provider 只管原因，给模型看的话由工具层统一**（契约 §4.5）。

分层（DSH 实测印证了这两层真的存在）：

| 层 | DSH 实测 | 本项目 |
|---|---|---|
| provider（原因） | `dsh-fs-observation-policy/lib/index.js:67` 抛 `edit requires reading "X" first`，码 `FS_NOT_OBSERVED` | 抛带错误码的原因 |
| 工具层（措辞） | `dsh-tool-fs/lib/index.js:477` 重写为 `cannot modify "X": file has not been read — read the file, then retry`；`:478` 对 `FS_STALE_VERSION` 拼 ` — re-read the file, then retry` | **实测逐字一致**（见 §6.1） |

**为什么这值得单独写一节**：契约 §4.5 描述的文案是**工具层**（面向模型的那层），而 DSH 的 observation-policy 层抛的是另一句话。
两层不是矛盾，是分工——若后来人只看到 provider 层的原文，会误以为契约写错了。**`agent/fs/errors.go:29-30` 把这条分工写进了注释。**

### 6.1 实测：九个错误码的最终文案（`agent/fs/errors.go:15-25, 37-69`）

契约冻结其中 4 个；实现另有 5 个（`agent/fs/errors.go` 实测）：

| 错误码 | 模型可见文案（`Error()` 实测） | 模型应做的动作 | 可否重试 |
|---|---|---|---|
| `FS_NOT_OBSERVED` | `cannot modify "X": file has not been read — read the file, then retry` | 先 `read`，再重试 | ✅ 可（先补齐观察） |
| `FS_STALE_VERSION` | `cannot modify "X": the file changed on disk since it was read — re-read the file, then retry` | **重新 `read`** 后再编辑 | ✅ 可（重读后） |
| `FS_NOT_FOUND` | `cannot edit "X": file does not exist — create it with write first` | 改用 `write` 创建 | ✅ 可（换工具） |
| `FS_SANDBOX_DENIED` | `[sandbox: 拒绝访问根目录之外的路径]`（带具体原因时为 `[sandbox: <原因>]`） | 改路径；**不要重试同一路径** | ❌ 不可 |
| `FS_INVALID_ARGS` | `invalid arguments: <原因>` | 修正参数 | ✅ 可 |
| `FS_NO_MATCH` | `cannot edit "X": old_string not found in the file` | 重新 `read` 确认内容 | ✅ 可 |
| `FS_AMBIGUOUS_MATCH` | `cannot edit "X": old_string appears <次数> — pass replace_all=true or use a longer unique string` | 用 `replace_all` 或更长串 | ✅ 可 |
| `FS_IO_ERROR` | `io error on "X": <原因>` | 视原因 | ⏳ |
| `FS_IMAGE_TOO_LARGE` | `cannot read image "X": <原因>` | 换小图 | ✅ 可 |

**关键**：错误码原样保留（供机器路由与日志聚合），**但模型看到的是"下一步该怎么办"**。
同一句"失败"，若写成"找不到匹配"，模型会去猜字符串；写成"文件已过期，请重读"，模型才会做唯一正确的动作。
**措辞是控制回路的一部分，不是文案。**

---

## 7. 路径沙箱（红线）🔶

文件工具**只允许在 `Exec.Root` 之内**操作（契约 §0、§4.1；已落地于 `agent/fs/sandbox.go`）：

1. 解析路径 → **`filepath.EvalSymlinks`** → 再判前缀是否落在根内（先解析再判前缀，才能挡住"根内的软链接指到根外"）。
2. 拒绝四类：**根外绝对路径**、**`..` 穿越**、**符号链接逃逸**、**非普通文件**（目录/设备）。
3. 违反 → `FS_SANDBOX_DENIED`，文案 `[sandbox: 拒绝访问根目录之外的路径]`。

DSH 同侧证据：`dsh-fs-local/lib/index.js:221` 有 `if (info.isSymbolicLink()) return "symlink"`——**符号链接被单列一类拒绝**，不是顺手跟随。

> ⚠️ 这是**红线**，契约要求**独立复验**且负向对照必须全拒。
> **实测状态（14:50:41）**：`agent/fs/sandbox_test.go` 有针对性的负向用例（含 `TestSandboxRejectsSymlinkEscape`）且**当前通过**；14:47 时同一个用例还红着（"期望 FS_SANDBOX_DENIED，实际成功"）。
> 但这些仍是**实现方自己的单测**，**独立复验尚未做** ⏳ —— 契约 §9 的要求没有被这次快照满足。

---

## 8. agent 循环语义 🔶（`agent/loop.go` 已落地）

契约 §3 的六条，逐条对应到设计意图与实测位置：

| # | 语义 | 为什么 | 实测位置 |
|---|---|---|---|
| 1 | 每轮把 `history` + `Tools` 发给模型 | 工具定义必须每轮都在，模型才能持续判断 | `loop.go:120-124` |
| 2 | 有 `tool_calls` → 发 `EvToolCall` → 执行 → 发 `EvToolResult` → **assistant(tool_calls) 先入历史，再加每条 tool 结果** | 顺序必须让模型看到因果对，否则重复调用；顺序错了 OpenAI 系后端会直接拒 | `loop.go:144-192` |
| 3 | 纯文本 → 已经实时发过 `delta` → 发 `EvDone` | 前端不必区分"工具轮"与"回答轮" | `loop.go:137-140` |
| 4 | 触顶（迭代 **12** / 调用 **24**）→ `EvError`，**文案必须说明触顶原因** | 否则用户看到"出错了"却不知道是任务太大还是模型失控 | `loop.go:55-59, 156-162, 196-200` |
| 5 | **工具报错不回滚循环**，以 `is_error=true` 回灌 | 工具失败是**信息**不是终止条件 | `executeTool` + `loop.go:165-176` |
| 6 | 每个工具执行前检查 `ctx` 取消（每轮发模型前也检查） | 用户点了停止就该停 | `loop.go:115-118, 152-155` |

实测默认常量：`DefaultMaxIterations = 12`、`DefaultMaxToolCalls = 24`（`loop.go:55/59`；`cfg` 传 0 时取默认）。
实测循环级错误：`ErrMaxIterations`、`ErrMaxToolCalls`、`ErrNoStreamer`、`ErrNilStream`（`loop.go:62-71`）。

**上限为什么是两道**：`MaxIterations` 限"模型思考了多少轮"，`MaxToolCalls` 限"一共动了多少次手"。
只有前者时，一轮里塞 50 个 `write` 仍然会失控；只有后者时，模型可以无限轮空转。

### 8.1 与模型后端之间的唯一缝：`StreamFunc`（`agent/stream.go`，实测）✅

契约 §3 的 `Config`/`Run` 签名是冻结的，里面**没有**"谁来调模型"的入口；而循环又必须能在**不依赖真实后端**的情况下做确定性单测。
于是实现把这件事抽成一个可替换函数（Lead 已在 2026-10-12 确认，将补进契约 v1.1）：

```go
type StreamFunc func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error)

agent.SetStreamer(f)            // 安装（传 nil = 卸载，Run 会返回 ErrNoStreamer）
agent.Streamer()                // 读取
agent.RegistryStreamer(registry) // 生产：把 *llm.Registry 适配成 StreamFunc，接线层一行装上
```

**为什么这样切**：它把"可测试性"做成了结构而不是约定——`agent` 的 14 个用例全部用**脚本化假模型**跑，
无需真实模型、无网络、结果确定（这也正是契约 §9 第 4 条"工具真能跑 + 事件序列正确"能做到的前提）。
`stream.go:19-21` 用 `sync.RWMutex` 保护全局缝，读多写少。

---

## 9. 记忆系统：注入 + 工具并存 🔶（工具侧 ⏳ 未见落地）

知识库从"一个页签"变成"**AI 的记忆**"，两条通路解决两类需求：

| 通路 | 解决什么 | 时机 | 失败行为 |
|---|---|---|---|
| **自动注入**（`<memory>…</memory>` 拼进 system prompt） | "顺带想起"——用户没提，但相关 | `tools=true` 时按用户这条消息检索 top-k | **降级为无记忆，不阻断对话** |
| **`memory_search` / `memory_write` 工具** | "主动翻找 / 主动记下"——模型自己决定 | 模型在循环里自主调用 | 作为 `is_error` 回灌，不影响主流程 |

**为什么两者都要**：注入有成本（每次对话都要检索、要占 context），且只按"用户这条消息"检索；
工具则让模型可以用**它自己想到的查询词**去翻（比如看完文件后去查一个文件里出现的概念）。
反过来只留工具也不行——模型不会每轮都记得去查记忆，于是"本来相关"的信息永远不会出现。

---

## 10. 向后兼容与 CUA 冻结 🔶（部分已实测）

| 约束 | 机制 | 怎么证明 |
|---|---|---|
| 现有 `llm.Registry.Stream` / `Generate` 行为不变 | `req.Tools` 为空 → 走原路径（`llm/tools.go:78-81` 直接返回原实例）；新增字段与方法**只做追加** | ✅ `go build`/`go vet` 全清 + 现有测试 |
| `/api/chat` 现有 SSE 序列不变 | `tools` 缺省 false → 与改造前一致 | ⏳ 接线未落地，**尚未实测** |
| **CUA 零改动** | `web/cua.go`、`web/cua_llm.go`、`web/cua_test.go`、`cua/**`、`packaging/**`、`electron/**`、`cua.js` 的 CUA 逻辑一律不碰 | ✅ `go test ./web/ -run TestCua -count=1` → `ok blog-my/web 6.160s`（本人复跑） |

**为什么 CUA 的模型代理不受影响**：它只用 `Generate`（非流式、无工具），而本项目的扩展全部是**追加**——
`Message.ToolCalls` 等字段带 `omitempty`，`Tools` 为空时不进入工具路径；`withTools` 在 `Tools` 为空时**原样返回**实例（保留热路径行为）。
这是"从一块已冻结的地基旁加盖"而不是"改建地基"。

> `llm/tools.go:71-98` 实测刻意使用 Eino v0.9.21 的 `WithTools`（返回**新实例**）而非已废弃的 `BindTools`（就地改写，并发互相覆盖工具集）。
> 另实测：`llm` 新增了契约外的 `ErrToolDefInvalid`（工具定义非法：缺 name / parameters 非法 JSON Schema），Lead 2026-10-12 已批准，待补进契约 v1.1。

---

## 11. 未覆盖面 ⏳

- **没有真实模型端到端**：`tools` 字段 + `AGENT_TOOLS` + 真实模型 + 真文件的一次完整对话**一条都没跑过**；六个工具**从未被真实请求触发**。
- **无独立复验**：`agent/fs` 的 27、`llm` 的 15 个用例都是**本人复跑**的结果，不是第三方复验（契约 §9 明确要求沙箱红线独立复验）。
- **`web` 的 agent 分支无专门用例**：`go test ./web/` 通过，但 `runAgentChat` 的 SSE 事件序列与落库未见专门断言。
- ~~记忆工具未落地~~ → **已落地**：`rag/memory_tool.go` 的两个工具 + `rag/memory.go` 的注入（`<memory>`、top-k 5 / 上限 20）均已实现。
- **真实模型未验**：模型是否真的"自主判断"该用工具，**未用真实模型验证**；14 个用例是假模型脚本（确定性 ≠ 真实行为）。
- **跨平台未验**：`version_unix.go`/`version_other.go` 分支本机（Windows）**跑不到**。
- **并发未验**：多会话并发下的观察表隔离、`SetStreamer` 全局缝的热切换**未做压力测试**。
- **DSH 对照的取样范围**：本文引用的 DSH 代码来自本机安装的打包产物 `…\app.asar\dsh\node_modules\@deepseek-ai\<pkg>\lib\index.js`（采样 2026-10-12），**只读了这几个包的相关片段**，不代表 DSH 全貌；DSH 与本项目是**两套独立实现**，对照目的是说明设计取舍，不是宣称等价。

---

## 12. 参考

- [00-contract.md](00-contract.md) —— 接口契约 v1（**冻结**，本文的权威依据）
- [02-tools.md](02-tools.md) —— 六个工具的完整参数 / 返回 / 错误码 / 示例
- [03-usage.md](03-usage.md) —— 怎么开启、怎么用、安全边界
- [../api-contract.md](../api-contract.md) 第 11 节 —— `/api/chat` 的 `tools` 字段与两个新 SSE 事件
