# 使用与安全边界（03-usage）

> **依据**：[00-contract.md](00-contract.md)（冻结）与**实读代码**。✅ = 实测；🔶 = 契约设计；⏳ = 未验证。
> 采样时间 **2026-10-12 14:50:41（Asia/Shanghai）**。

---

## 0. 实测快照（14:50:41）

| 命令 | 结果 |
|---|---|
| `go build ./...` | 退出码 **0** ✅ |
| `go vet ./...` | 退出码 **0** ✅ |
| `go test ./... -count=1` | 退出码 **0** ✅（`agent` 4.539s / `agent/fs` 4.868s / `llm` 4.663s / `rag` 3.924s / `web` 4.257s 全 `ok`） |
| **CUA 红线** `go test ./web/ -run TestCua -count=1` | **`ok blog-my/web 0.562s`**，退出码 **0** ✅ |

通过的用例数（`-v` 计 `--- PASS`）：`agent` **14**、`agent/fs` **27**、`llm` **15**、`rag` **26**，共 **82**。

> **负向对照为什么可信**：这几个包里的负向用例**不是摆设**——本人在 14:47–14:49 亲眼看到它们**红过**：
> `TestSandboxRejectsSymlinkEscape`（软链接逃逸是否被拒）、`TestConcurrentEditsOneWins`、`llm` 的 `TestT36Merge*`（乱序/无 Index 分片归并）
> 当时都在 FAIL，随后（实现补齐后）转绿。**先红后绿**说明它们确实在断言行为，而不是恒真的绿。
> 注意：这段时间仓库在**并行开发中**（`agent/fs/tools.go` 与 `web/agent.go` 的 mtime 就在 14:48），所以任何单点快照都可能立刻过期。

---

## 1. 一分钟了解

- **默认关闭**：不配环境变量时，`/api/chat` 的行为与加此功能**之前完全一致**（契约 §0）。
- **两道闸门**：① 服务端 `AGENT_TOOLS` 开启；② 请求体 `tools: true`。**两个都满足**才走 agent 循环（`web/agent.go:67`）。
- **越界即拒**：文件工具只能动 `AGENT_FS_ROOT` 内的文件；`..`、根外绝对路径、**软链接逃逸**、非普通文件全部拒绝。
- **不读不改**：没 `read` 过的文件，`edit` 被拒（`FS_NOT_OBSERVED`）、`write` 也不会覆盖它。
- **没配根目录 = 文件工具一个都不注册**（`web/agent.go:102-110` 实测）——模型"无从写盘"。

---

## 2. 怎么开启

### 2.1 环境变量（**Lead 2026-10-12 批准**，实测代码位置如下）

| 变量 | 作用 | 默认 | 实测代码位置 |
|---|---|---|---|
| `AGENT_TOOLS` | **总开关** | 关闭 | `web/agent.go:34-43`（`1`/`true`/`on`/`yes` 视为开） |
| `AGENT_FS_ROOT` | 文件工具根目录（绝对路径） | 空 | `web/agent.go:50-52`；**为空则四个 fs 工具不注册**（`:102-110`） |
| `AGENT_MAX_ITERATIONS` | 覆盖迭代上限 | `12` | `web/agent.go:224` → `agent.loop.go:55` |
| `AGENT_MAX_TOOL_CALLS` | 覆盖工具调用上限 | `24` | `web/agent.go:225` → `agent.loop.go:59` |

```powershell
# 示例（变量名已获批准）
$env:AGENT_TOOLS = "1"
$env:AGENT_FS_ROOT = "E:\blog-my\workspace"   # 建议指向专门的工作目录
$env:AGENT_MAX_ITERATIONS = "12"
$env:AGENT_MAX_TOOL_CALLS = "24"
```


**桌面应用**：打包态的配置来自用户配置目录（`userData/.env`），Electron 启动时会自动补齐缺失的
`AGENT_*`（默认开启；沙箱为 `userData/workspace`，启动时自动创建），所以桌面端与网页端行为一致。
想单独关掉桌面端的工具调用，把 `AGENT_TOOLS` 显式写成 `0` 即可（显式值优先，不会被默认值覆盖）。
> ⚠️ `AGENT_FS_ROOT` **不要指向家目录或代码仓库根**：本轮**没有**"危险操作二次确认"这类护栏（契约未要求），
> 模型在根目录内可以自由 `write`/`edit`。

### 2.2 请求侧的开关

`POST /api/chat` 请求体新增可选字段 `tools`（bool，默认 false；`web/chat.go:125` 实测）：

```json
{ "conversation_id": 12, "model": "deepseek-chat", "content": "把 notes/a.txt 里的 world 改成 CUA", "tools": true }
```

| 条件 | 行为 |
|---|---|
| `tools` 省略 / `false` | 老路径（`postChat` 原流程），**与改造前一致** |
| `tools=true` 且 `AGENT_TOOLS` 未开 | **仍走老路径**（实测逻辑：`toolsRequested && agentToolsEnabled()`，`web/agent.go:67`；`web/chat.go:207-210` 只在两者都满足时调 `runAgentChat`） |
| `tools=true` 且 `AGENT_TOOLS` 开 | 走 agent 循环（记忆注入 + 六个工具） |

---

## 3. 前端怎么用（实测）

| 变化 | 之前 | 现在 | 实测位置 |
|---|---|---|---|
| 页签 | 5 个 | **3 个：对话 / 模型 / 电脑操控** | `templates/chat/index.html:23-27` |
| 密钥 | 独立页签 | **右上角按钮 → 弹层**；`#/keys` 路由仍可达 | `index.html:49, 169-176`（`chat.js` 的 `handleRoute` 特判） |
| 记忆（知识库） | 独立页签 | **并入对话视图**的记忆面板（文档计数） | `chat.js:36-37, 418-433` |
| 工具调用 | 无 | 对话流里渲染**工具卡片** | `chat.js:621-790` |
| 电脑操控（CUA） | 测试版角标 | **不变** | `index.html:26`（CUA 逻辑未动） |

---

## 4. 用起来是什么样

不需要用户"点名工具"——用户正常提问，模型自己决定是否动手。
**系统提示词实测**（`web/agent.go:147-152`）给模型定了 5 条准则：

1. 能直接回答就直接回答，**不要为了用工具而用工具**；
2. 需要了解文件内容时先 `read`；**修改已有文件前必须先 read，否则会被拒绝**；
3. 优先用 `edit` 做定点修改，只有整体重写时才用 `write`；
4. 工具返回错误时，**按错误提示纠正参数后重试**，不要原样重复同一个调用；
5. 完成后用简洁的中文告诉用户做了什么、结果如何。

| 用户说 | 模型可能做的 | 用户看到 |
|---|---|---|
| 「看看 notes/a.txt 写了什么」 | `read` | `read` 卡片（文件名 + 可展开的带行号内容） |
| 「把里面的 world 改成 CUA」 | `read`（若没读过）→ `edit` | 两张卡片 + 一段结论 |
| 「这张截图里是什么」 | `read_image` | 卡片；**图片作为多模态消息喂给模型**，不进 SSE |
| 「记住：部署用 8300 端口」 | `memory_write` | 写入卡片（含去重提示） |
| 「我们上次讨论的方案叫啥」 | `memory_search` 或走自动注入 | 检索卡片 / 直接回答 |

**记忆的两种出现方式**（契约 §6，实测）：
- **自动注入**：`tools=true` 时先按用户这条消息检索（`kbMemoryPrompt`，`web/agent.go:217`），把结果拼进 system prompt；**用户看不到卡片**；检索失败**降级为无记忆**、不阻断（`rag/memory.go:16` 铁律）。
- **主动检索**：模型自己调 `memory_search`，这时能看到卡片与命中条目。

---

## 5. 安全边界

### 5.1 路径沙箱（红线，契约 §0/§4.1）—— `agent/fs/sandbox.go`
- 只能操作 `Exec.Root` 内路径；解析后仍要判前缀（挡"根内软链接指向根外"）。
- 拒绝四类：根外绝对路径、`..` 穿越、**符号链接逃逸**、非普通文件（目录/设备）→ `FS_SANDBOX_DENIED`。
- 实测：`agent/fs/sandbox_test.go` 有针对性负向用例（`TestSandboxRejectsSymlinkEscape` 等），14:50 快照下**通过**；但**独立复验尚未做** ⏳。

### 5.2 防"没看过就改"（契约 §4.2）—— `agent/fs/observation.go`
- **没见过** + `edit` → `FS_NOT_OBSERVED`；**确认不存在** + `edit` → `FS_NOT_FOUND`；
- **没见过** + `write` → 不覆盖创建（`os.Link`），文件已存在 → `FS_NOT_OBSERVED`；
- 读过但被外部改动 → `FS_STALE_VERSION`，要求**重读后再改**。
- 实测细节：`read` 成功后**再 `Stat` 一次**记录版本（`agent/fs/tools.go:198-200`），避免把读期间的改动记成旧版本。

### 5.3 落盘安全（契约 §4.4）—— `agent/fs/atomic.go`
同目录暂存 → `Sync` → 原子发布（创建用 `Link` / 覆盖用 `Rename`）→ 清理；**提交成功后的清理失败被吞掉**。

### 5.4 失控保护（契约 §3）—— `agent/loop.go`
迭代 **12** / 调用 **24** 两道上限，触顶发 `error` 事件且**文案说明触顶原因**（`loop.go:158-159,197-198` 实测文案含次数与轮数）；
每个工具执行前检查取消（浏览器断开 → 请求 `ctx` 取消 → 循环停，`web/agent.go:283` 注释实测）。

### 5.5 默认最小暴露
- `AGENT_TOOLS` 未开 → 新增能力完全不参与；
- `AGENT_FS_ROOT` 未配 → **四个文件工具不注册**（只剩两个记忆工具）；
- 只读工具（`read`/`read_image`/`memory_search`）无副作用；写入类（`write`/`edit`/`memory_write`）才改东西；
- `/api/kb/*` 路由**全部保留**（契约 §6），既有前端与脚本不被打断。

### 5.6 仍然要人盯的地方 ⏳
- **工具失败不回滚循环**（契约 §3.5）：一次对话可能连续报错直到触顶——卡片要能看出"反复失败"。
- **没有危险操作二次确认**（契约未要求）：靠 `AGENT_FS_ROOT` 划范围。
- **审计**：`runAgentChat` 确实落库——**工具轨迹先于回复**写入同一条会话（`web/agent.go:286-292`）：轨迹 `json.Marshal` 成一条 `roleToolTrace` 角色的消息，回复存 `assistant`。但**该格式未在契约中冻结**，且无专门序列化用例 ⏳。

---

## 6. 排错速查

| 现象 | 最可能的原因 | 怎么办 |
|---|---|---|
| 没有工具卡片、AI 也不动手 | `tools` 没传 true，或 `AGENT_TOOLS` 没开 | 两个都开（§2） |
| 没有文件工具，只有记忆工具 | `AGENT_FS_ROOT` 未配 | 配一个绝对路径（§2.1） |
| `FS_NOT_OBSERVED` | 模型没先 `read` 就想改 | 正常：模型会自己补 `read`；反复出现说明模型不听话 ⏳ 需实测评估 |
| `FS_STALE_VERSION` | 读过之后文件被外部改了 | 重新 `read` 再改 |
| `FS_NOT_FOUND` | 对不存在的文件做 `edit` | 改用 `write` 创建 |
| `FS_SANDBOX_DENIED` | 路径越界（`..`/根外/软链接/目录） | 换根内普通文件；**别重试同一路径** |
| `FS_NO_MATCH` / `FS_AMBIGUOUS_MATCH` | `old_string` 没命中 / 命中多处 | 重新 `read` 对齐内容；多处替换用 `replace_all=true` |
| `FS_IMAGE_TOO_LARGE` | 图片超过 **10 MB**（`agent/fs/tools.go:17`） | 换小图 |
| `MEMORY_SEARCH_FAILED` / `MEMORY_WRITE_FAILED` | 记忆库/DB 故障 | 对话不会断（降级）；看服务端日志 |
| 后端报"不支持工具调用" | `ErrToolsUnsupported`（`llm/tools.go:35`） | 换支持工具调用的后端；mock 后端支持（测试缝） |
| 中途报"触顶" | 迭代 >12 或调用 >24 | 拆任务，或调高 `AGENT_MAX_*` |
| 报"工具定义非法" | `ErrToolDefInvalid`（`llm/tools.go:39`） | 工具 schema 写错了（缺 name / parameters 非 JSON Schema） |

---

## 7. 未覆盖面 ⏳

- **没有真实模型端到端**：六个工具**从未被真实请求触发过**（本人在 14:50 只跑了单测与门禁，没有真实模型对话）。
- **没有独立复验**：本文所有"通过"都是**我复跑单测**的结果，不等于第三方复验；沙箱红线的负向对照需独立复验（契约 §9）。
- **`web` 包的 agent 分支无单测**：`go test ./web/` 通过，但 `runAgentChat` 的 SSE 事件序列/落库**未见专门用例** ⏳。
- **`agentToolsSummary()`（`web/agent.go:309`）未接路由**：注释称"诊断用"，但 `web/web.go` 里没有对应端点。
- **跨平台未验**：`agent/fs/version_unix.go`/`version_other.go` 分支在本机（Windows）**跑不到**。
- **并发未压测**：多会话并发下观察表隔离、`SetStreamer` 热切换、`fs` 的 `lockTarget` 竞争**未做压力测试**。
- **`AGENT_*` 取值边界未验**：负数/超大值经 `agentIntEnv` 后的行为未见用例 ⏳。

---

## 8. 参考

- [00-contract.md](00-contract.md) —— 接口契约 v1（冻结）
- [01-design.md](01-design.md) —— 设计说明与 DSH 对照
- [02-tools.md](02-tools.md) —— 六个工具的完整参数/返回/错误码
- [../api-contract.md](../api-contract.md) 第 11 节 —— `/api/chat` 的 `tools` 字段与两个新 SSE 事件

---

## 9. 真实模型端到端实测（2026-10-12 18:16–18:22）

本节补上第 7 节第 1 条的缺口：六个工具**已由真实模型经真实 HTTP 链路触发**。实测配置与证据如下。

**配置**：`.env` 写入 `AGENT_TOOLS=1`、`AGENT_FS_ROOT=E:\blog-my\workspace`、`AGENT_MAX_ITERATIONS=12`、`AGENT_MAX_TOOL_CALLS=24`；
前端 `chat.js` 本来就在请求体里带 `tools: true`，无需改动。

| 用例 | 提示 | 实测结果 |
|---|---|---|
| 文件读改（模型：`deepseek-chat`） | 读 `notes/demo.txt`，把 `hello world` 改成 `hello CUA` | SSE 依次给出 `read` → `edit` → `read` 三个 `tool_call`/`tool_result`，最后 `done`；磁盘文件确实由 `hello world` 变为 `hello CUA` ✅ |
| 轨迹落库 | 同上 | `message` 表出现一条 `role=tool_trace` 的 JSON 数组（3 项）与一条 `assistant` 回复 ✅ |
| 记忆写入 | “remember this long-term: my deployment port is 8300” | 触发 `memory_write`，写入 `kb_document` 并完成 bge-m3 嵌入 ✅ |
| 记忆自动注入 | 新会话问“What port should I use for deployment?” | 模型答 “Use port 8300 for deployment.”，未显式调用工具即召回 ✅ |

**回归**：`go build ./...` 与 `go test ./... -count=1` 退出码均为 0（`agent` / `agent/fs` / `llm` / `rag` / `web` 全 `ok`）。

**清理**：测试账号（`e2e_agent_*`）、测试会话、测试记忆行均已从数据库删除；`workspace/notes/demo.txt` 已复位为 `hello world`，可直接重跑同一演示。

**仍未覆盖**：沙箱越界的真实模型路径（模型可能拒绝尝试越界，难以稳定触发）仍以 `agent/fs` 单测为准；多会话并发与 `AGENT_*` 取值边界仍未压测 ⏳。
