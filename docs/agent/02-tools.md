# 工具定义：四个 fs 工具 + 两个记忆工具（02-tools）

> **依据**：[00-contract.md](00-contract.md) §4 / §6（**冻结**）。本文的**参数、返回、错误码全部来自实读代码**（不是契约转述），
> 引用行号可复现。采样时间 **2026-10-12 14:50:41（Asia/Shanghai）**。✅ = 实读/实跑；🔶 = 契约设计；⏳ = 未验证。
>
> **实现状态（14:50:41）**：六个工具**已落地**（`agent/fs/tools.go`、`rag/memory_tool.go`），`/api/chat` 的 `tools` 字段**已接线**（`web/chat.go:125, 207-210`），
> `go build`/`go vet`/`go test ./...` 与 CUA 门禁**全通过**。**但六个工具仍未被真实请求触发过** —— 见 §11。
>
> **2026-10-25 增补**：新增 `grep`、`glob`（工作区检索）与 `update_plan`（任务计划），
> `edit` 改为成功后自动回显修改区域，`list` 增加 `offset` 分页，`read` 对超过 8MB 的文本走流式窗口。
> 设计与验证详见 [13-aci-upgrades.md](13-aci-upgrades.md)；下面 §1 的工具表已同步新增行。

---

## 1. 工具总表

| 工具 | 代码位置 | 作用 | 副作用 | 返回图片 |
|---|---|---|---|---|
| `read` | `agent/fs/tools.go:146` | 读文本文件（带行号） | 记录观察 `present`/`absent` | — |
| `write` | `agent/fs/tools.go:220` | 创建或覆盖整个文件 | **改文件系统** | — |
| `edit` | `agent/fs/tools.go:300` | 字面量替换 | **改文件系统** | — |
| `read_image` | `agent/fs/tools.go:409` | 读图片交给模型看 | 记录观察 | ✅ `Result.Image` |
| `list` | `agent/fs/tools.go` | 列目录（`limit`/`offset` 分页） | 只读 | — |
| `grep` | `agent/fs/search.go` | 按正则/字面量搜索文件内容，返回 `路径:行号: 内容` | 只读 | — |
| `glob` | `agent/fs/search.go` | 按通配符找文件（支持 `**`） | 只读 | — |
| `restore` | `agent/fs/checkpoint.go` | 恢复到最近一次修改前的文件检查点 | **改文件系统** | — |
| `restore_turn` | `agent/fs/checkpoint.go` | 整组撤销最近一轮改过的所有文件 | **改文件系统** | — |
| `update_plan` | `agent/plan.go` + `web/agent_plan_tool.go` | 更新本会话任务计划，每轮瞬态注入 | 仅进程内状态 | — |
| `memory_search` | `rag/memory_tool.go:47` | 检索长期记忆 | 只读 | — |
| `memory_write` | `rag/memory_tool.go:123` | 写入一条记忆 | **写记忆库** | — |
| `skill_search` | `web/skill.go:275` | 按关键词搜已启用技能 | 只读 | — |
| `skill_read` | `web/skill.go:342` | 读取技能完整正文 | 只读 | — |

构造入口：`agentfs.Tools()`（`agent/fs/tools.go`，返回四个基础 fs 工具）、`agentfs.ToolsWithMutation()`（含 list/move/delete）、`agentfs.SearchTools()`（grep/glob）、`agentfs.RecoveryTools()`（restore）、`rag.NewMemoryTools(backend, userID)`（`rag/memory_tool.go:30`）、`skillTools(userID)`（`web/skill.go:388`，返回两个技能工具）、`planTool{}`（`web/agent_plan_tool.go`）。
**工具名即模型可见名**（`Name()` 直接进 `llm.ToolDef.Name`）。

---

## 2. 通用约定

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() map[string]any                  // JSON Schema object
    Execute(ctx context.Context, args json.RawMessage, ex *Exec) (Result, error)
}

type Result struct {
    Content string         // 给模型看的正文
    Display map[string]any // 给前端看的结构化摘要
    IsError bool           // 工具级失败（回灌给模型，不终止循环）
    Image   *ImagePart     // 仅 read_image
}
```

| 约定 | 实测规则 |
|---|---|
| `args` | 原始 JSON，**由工具自己解析并校验**（`agent/fs/tools.go:49 decodeArgs`） |
| 参数校验失败 | 返回 `FS_INVALID_ARGS` / `MEMORY_BAD_ARGS`，并带上 `Display` 里的错误码 |
| 失败语义 | 返回 `error` → 循环折成 `is_error` 结果回灌，**不回滚循环**（`agent/loop.go:165-176`） |
| 错误码 | 机器路由用 `Display`/错误码；给模型看的正文由 `Error()` 统一重写（`agent/fs/errors.go:37-69`） |
| 记忆工具的用户身份 | `Exec` 只有 `SessionID`/`Root`，**没有用户**；所以记忆工具在**构造时**绑定 `userID`，每次请求现建一组（`rag/memory_tool.go:20-21`） |
| Schema 严格度 | 记忆工具声明了 `additionalProperties: false`（`rag/memory_tool.go:71,146`）；**fs 工具未声明**（实测差异，非缺陷但值得知道） |

---

## 3. `read` —— 读文本文件 ✅

| 名 | 类型 | 必填 | 默认 | 实测说明 |
|---|---|---|---|---|
| `file_path` | string | ✅ | — | 相对根目录，或根目录内的绝对路径（`tools.go:157`） |
| `offset` | integer | — | 1 | 从第几行开始（`tools.go:158`） |
| `limit` | integer | — | **2000**（`defaultReadLimit`，`tools.go:16`） | 最多读多少行（`tools.go:159`） |

`Description()`（`tools.go:148-151`）明确告诉模型：**"修改文件前必须先 read，否则 write/edit 会被拒绝"**——把观察策略写进工具描述，模型才不会反复踩。

**返回正文**（`tools.go:202-212`）：
- `行号<TAB>内容`（`numberLines`，`tools.go:110-141`）
- 空文件 → `(文件为空)`（`:203-205`）
- 截断 → 正文末尾追加 `... (共 N 行，已显示 M 行；用 offset=K 继续)`（`:206-208`）
- `Display`：`file_path`、`lines`、`shown`、`bytes`、`truncated`（`:211`）

**观察副作用**：成功 → `present{读后的版本}`（`:198-200`，注意是**读之后再 Stat 一次**，避免把读期间的改动记成旧版本）；
不存在 → **记 `absent`**（`:180-182`，契约 §4.2 的"失败也要记"已落实）。

**错误码**：`FS_NOT_FOUND`（`:184`，正文 `cannot read "X": file does not exist`）、`FS_IO_ERROR`（`:186,195`）、`FS_SANDBOX_DENIED`（非普通文件，`:189`）

---

## 4. `write` —— 创建或覆盖 ✅

| 名 | 类型 | 必填 | 实测说明 |
|---|---|---|---|
| `file_path` | string | ✅ | 相对根目录或根内绝对路径（`tools.go:231`） |
| `content` | string | ✅ | **允许空串，但不能缺省**（`:232,246-248` 实测：缺省报 `FS_INVALID_ARGS`） |

**intent 决策**（`tools.go:255-277`，锁内完成）：

| 观察状态 | 行为 |
|---|---|
| 未知（`ObsUnknown`）/ 确认不存在（`ObsAbsent`） | `createIfAbsent = true`（`:261`） |
| 存在但磁盘上已消失 | 当创建处理（`:265-266`） |
| 存在、版本一致 | `createIfAbsent = false`（覆盖） |
| 存在、**版本不一致** | `FS_STALE_VERSION`，正文含 `expected … found …`（`:269-273`） |
| 非普通文件 | `FS_SANDBOX_DENIED`（`:267-268`） |

**父目录**：`file_path` 的父目录不存在时自动创建（`MkdirAll`，仍严格限制在根目录内）；
父路径被普通文件占用等失败情况报 `FS_IO_ERROR`，文案只带相对路径。目标就是根目录本身时不建目录。

**成功正文**：`Created <path> (<N> 字节)` 或 `Updated <path> (<N> 字节)`（`:285-291`）。
**`Display`**：`file_path`、`created`(bool)、`bytes`、`version`（`:291`）。
**观察副作用**：写入后用**写后的版本**更新（`:283`）。

---

## 5. `edit` —— 字面量替换（一个临界区）✅

| 名 | 类型 | 必填 | 默认 | 实测说明 |
|---|---|---|---|---|
| `file_path` | string | ✅ | — | `tools.go:312` |
| `old_string` | string | ✅ | — | 必须**逐字符一致**（含缩进）；空/纯空白 → `FS_INVALID_ARGS`（`:313,329-331`） |
| `new_string` | string | ✅ | — | 与 `old_string` **完全相同**也被拒（`:314,332`） |
| `replace_all` | boolean | — | **false** | false 时要求**唯一匹配**（`:315`） |

**执行顺序**（`tools.go:340-390`，整段在同一把 `lockTarget` 锁内）：
**探版本 → 三态守卫 → `os.Stat` → 版本比对 → 读 → 匹配 → 重写**

- 守卫：`ObsUnknown` → `FS_NOT_OBSERVED`（`:347-348`）；`ObsAbsent` → `FS_NOT_FOUND`（`:349-350`）
- **版本守卫在匹配之前**（`:364-372`，代码注释原话："陈旧编辑要报 `FS_STALE_VERSION`，绝不能报「找不到匹配」"）
- 匹配：`count == 0` → `FS_NO_MATCH`（`:380-381`）；`count > 1 && !replace_all` → `FS_AMBIGUOUS_MATCH`（`:383-384`，正文含 `N times`）

**成功正文**：`Edited <path> (<N> 处替换)`（`:399`）。**`Display`**：`file_path`、`replacements`、`version`（`:400`）。

---

## 6. `read_image` —— 读图片给模型看 ✅

| 名 | 类型 | 必填 | 实测说明 |
|---|---|---|---|
| `file_path` | string | ✅ | `tools.go:420` |

- `Description()`（`:411-414`）：**png/jpeg/gif/webp/bmp**，"文本文件请用 read"。
- MIME 由**内容嗅探**决定（`http.DetectContentType`，`:459`），不是看扩展名；嗅探结果不是 `image/*` → `FS_INVALID_ARGS`（`:460-462`）。
- **大小上限 10 MB**（`maxImageBytes = 10 << 20`，`:17`）；超限 → `FS_IMAGE_TOO_LARGE`，正文 `cannot read image "X": <N> 字节超过上限 <M> 字节`（`:450-452, errors.go:60-61`）。
- **成功正文**：`已读取图片 <path>（<mime>，<N> 字节）`（`:468`）；`Display`：`file_path`、`mime`、`bytes`（`:469`）。
- **图片本体走 `Result.Image`**（`:470`），由循环作为一条 **user 消息**追加（`agent/loop.go:186-192`）——**不出现在 SSE 里**。

---

## 7. `memory_search` —— 主动翻找记忆 ✅

| 名 | 类型 | 必填 | 默认 | 实测说明 |
|---|---|---|---|---|
| `query` | string | ✅ | — | 自然语言或关键词；纯空白 → `MEMORY_EMPTY_"UERY`（`memory_tool.go:59-62,86-90`） |
| `limit` | integer | — | **5**（`DefaultMemoryTopK`） | schema `minimum 1 / maximum 20`（`MaxMemoryTopK`）；服务端另有 `ClampMemoryLimit` 兜底（`rag/memory.go:51-58`） |

**成功正文**（`memory_tool.go:101-110`）：
- 命中：`记忆里找到 <N> 条与「<query>」相关的内容：\n\n<n 段正文>`
- **未命中不是错误**：`记忆里没有找到与「<query>」相关的内容。`（`:102-105`；让模型得到"想不起来"这个事实）
- `Display`：`count`、`query`、`hits`（摘要数组，`:100`）

**错误码**：`MEMORY_BAD_ARGS`、`MEMORY_EMPTY_"UERY`、`MEMORY_SEARCH_FAILED`（`:84-97`，最后一个正文为 `记忆检索失败：…`）。
**失败不阻断对话**：以 `is_error` 回灌，模型可换词再试或老实说想不起来（`:93-98` 注释）。

---

## 8. `memory_write` —— 写一条记忆 ✅

| 名 | 类型 | 必填 | 实测说明 |
|---|---|---|---|
| `text` | string | ✅ | 一句自足的话；纯空白 → `MEMORY_EMPTY_TEXT`（`memory_tool.go:135-138,161-165`） |
| `tags` | array&lt;string&gt; | — | 可选标签（`:139-143`） |

**成功正文**（`:177-186`）：
- 新增：`已记住（记忆 id=<id>，标题《<title>》，共 <N> 个片段）。`
- **重复**：`这条记忆已经存在（记忆 id=<id>，标题《<title>》），没有重复写入。`（去重是实测行为，`:177-181`）
- `Display`：`id`、`kind`、`title`、`chunk_count`、`duplicated`（`:173-176`）

**错误码**：`MEMORY_BAD_ARGS`、`MEMORY_EMPTY_TEXT`、`MEMORY_WRITE_FAILED`（`:159-171`）。
**工具描述里明确约束模型**：只记真正有长期价值的内容，不要记寒暄/临时问题/AI 自己的推测（`:126-128`）。

---

## 9. 错误码总表（实测）

### 9.1 fs 工具（`agent/fs/errors.go:15-25`，9 个）

| 错误码 | 模型可见正文（`Error()` 实测） |
|---|---|
| `FS_SANDBOX_DENIED` | `[sandbox: <具体原因>]`；无原因时 `[sandbox: 拒绝访问根目录之外的路径]` |
| `FS_NOT_OBSERVED` | `cannot modify "X": file has not been read — read the file, then retry` |
| `FS_STALE_VERSION` | `cannot modify "X": the file changed on disk since it was read (expected …, found …) — re-read the file, then retry` |
| `FS_NOT_FOUND` | `cannot edit "X": file does not exist — create it with write first`（`read`/`read_image` 场景另有 `cannot read …` / `cannot read image …`） |
| `FS_INVALID_ARGS` | `invalid arguments: <原因>` |
| `FS_NO_MATCH` | `cannot edit "X": old_string not found in the file` |
| `FS_AMBIGUOUS_MATCH` | `cannot edit "X": old_string appears <N> times — pass replace_all=true or use a longer unique string` |
| `FS_IO_ERROR` | `io error on "X": <原因>` |
| `FS_IMAGE_TOO_LARGE` | `cannot read image "X": <N> 字节超过上限 <M> 字节` |

> 契约 §4.5 只冻结了前 4 个；后 5 个是实现新增（`agent/fs/errors.go` 实测）。**模型看到的是"下一步怎么办"**：先 `read` / 重新 `read` / 改用 `write` / 加 `replace_all`。

### 9.2 记忆工具（`rag/memory_tool.go` 实测，5 个）

`MEMORY_BAD_ARGS`、`MEMORY_EMPTY_"UERY`、`MEMORY_SEARCH_FAILED`、`MEMORY_EMPTY_TEXT`、`MEMORY_WRITE_FAILED`。
它们出现在 `Display` 里（`memoryErrorDisplay`），**且同时返回一个 error**——循环会把 error 折成 `is_error` 结果。

### 9.3 llm 层（`llm/tools.go:32-40` 实测）

| 错误 | 含义 |
|---|---|
| `ErrToolsUnsupported` | 后端没实现 `model.ToolCallingChatModel`；**开流之前**返回 |
| `ErrToolDefInvalid` | 工具定义非法（缺 name / parameters 不是合法 JSON Schema）——**契约外新增**，Lead 2026-10-12 已批准，待补进契约 v1.1 |

---

## 10. 一次完整交互长什么样（SSE 形状）

`web/agent.go:209-277` 实测会发生这些帧：

```
event: tool_call
data: {"id":"call_1","name":"read","arguments":{"file_path":"notes/a.txt"}}

event: tool_result
data: {"id":"call_1","name":"read","ok":true,"summary":"…","detail":"1\thello\n2\tworld","display":{…}}

event: delta
data: {"content":"…"}

event: done
data: {}
```

> 帧格式与老路径共用同一个 `sendEvent` 闭包（`web/agent.go:170` 注释："两条路径的 SSE 帧格式天然一致"）。
> 事件字段的权威定义见 [../api-contract.md](../api-contract.md) 第 11 节。
> 这些帧由 `runAgentChat`（`web/agent.go:239-280`）实测发出：`tool_call` 带 `id`/`name`/`arguments`（`:249-253`），`tool_result` 带 `id`/`name`/`ok`/`summary`/`detail`/`display`（`:262-269`）。
> **尚未用真实模型端到端跑过** ⏳。

---

## 11. 未覆盖面 ⏳（时间戳 2026-10-12 14:50:41）

- ~~`web` 包编译不过~~ → **已修复**（14:48 补回 `"time"` import）；14:50:41 快照 `go build`/`go vet`/`go test ./...` 全部 exit 0。
- ~~`/api/chat` 的 `tools` 字段未接线~~ → **已接线**：`web/chat.go:125` 收 `tools`，`:207-210` 在 `tools && AGENT_TOOLS` 时调 `runAgentChat`（`web/agent.go:206`）。**但六个工具仍未被真实请求触发过** ⏳。
- ~~两个 fs 用例失败~~ → **已转绿**（14:49→14:50）。**这条历史仍然保留**，因为它同时证明沙箱/并发负向对照**真的会红**：
  `TestSandboxRejectsSymlinkEscape`（软链接逃逸，契约 §0 红线）、`TestConcurrentEditsOneWins`、以及 `llm` 的 `TestT36Merge*`（乱序 / 无 Index 归并）当时都在 FAIL，实现补齐后转绿。
- ~~CUA 红线门禁跑不了~~ → **已恢复**：14:50:41 `go test ./web/ -run TestCua -count=1` → `ok blog-my/web 0.562s`，exit 0 ✅。
  （14:48 前后曾因同包编译失败而 `[build failed]`，那是**编译问题，不是 CUA 代码坏了**。）
- **无真实模型验证** ⏳：模型是否真会"自主判断"用工具，**未用真实模型跑过**。
- **本文未做的实测**：六个工具的**端到端**（模型 → 工具 → 文件系统真的变了）**一次都没跑过**；上述返回正文来自**读代码 + 单测断言**，不是端到端实跑输出。
- **独立复验未做** ⏳：fs / llm / rag 的用例都是本人复跑，不是第三方复验。

---

## 12. 参考

- [00-contract.md](00-contract.md) —— 接口契约 v1（冻结）
- [01-design.md](01-design.md) —— 设计说明与 DSH 对照
- [03-usage.md](03-usage.md) —— 开启方式、用法、安全边界
- [../api-contract.md](../api-contract.md) 第 11 节 —— `/api/chat` 的 `tools` 字段与两个新 SSE 事件

---

## 13. 新增工具（2026-10-12）

在原有 read / write / edit / read_image 之外，补齐了一套完整的文件工具与三个通用工具。
全部工具经真实模型端到端跑过（见 03-usage.md 第 9 节）。

### 13.1 文件工具（需要工作区）

| 工具 | 参数 | 行为与约束 |
|---|---|---|
| `list` | `dir_path?`, `limit?` | 列出目录直接子项（目录名以 / 结尾）。只读，不记录观察。 |
| `move` | `from_path`, `to_path` | 重命名/移动。**源必须先 read**（版本一致）；**目标已存在则拒绝**，不覆盖；目标父目录自动创建。 |
| `delete` | `file_path` | 删除。**必须先 read**（版本一致）；**只删普通文件**，目录/设备一律拒绝；删除不可逆。 |

`move` / `delete` 与 `write` / `edit` 共用同一套观察守卫（`FS_NOT_OBSERVED` / `FS_STALE_VERSION`）和同一套路径沙箱（`FS_SANDBOX_DENIED`）。
删除后观察表记为 absent，再 `write` 同名文件属于新建（不会撞上 `FS_NOT_OBSERVED`）。

### 13.2 PDF 读取

`read` 遇到 `.pdf`（或魔数为 `%PDF`）时改走文本层抽取，不再把二进制当文本按行号输出。
支持未加密、文本用 `Tj`/`TJ` 操作符的常见 PDF（含 `FlateDecode` 压缩流）；
**扫描件、加密 PDF、CID 字体会抽不出或抽错** —— 扫描件请改用 `read_image`。不引第三方 PDF 库是刻意的取舍。

### 13.3 通用工具（不需要工作区）

| 工具 | 参数 | 说明 |
|---|---|---|
| `calculator` | `expression` | 递归下降求值：`+ - * / % ^`、括号、`sqrt/abs/round/floor/ceil/log/log10/exp/sin/cos/tan/pow/min/max`、常量 `pi`/`e`。除零等非法输入报错。 |
| `datetime` | `action`, `datetime?`, `to?`, `amount?`, `unit?`, `timezone?` | `now` / `add` / `diff`；单位 seconds~years；时区内嵌 tzdata（Windows 也能用 IANA 名称）。 |
| `web_search` | `query`, `max_results?` | 走 DuckDuckGo HTML 入口（无需 API Key），返回标题/链接/摘要。解析不出结果就报错，**不编造结果**。 |
| `web_reader` | `url`, `max_chars?` | 抓网页并抽正文（去脚本/样式/标签）。**默认拒本机与内网地址**（防 SSRF）；只支持 http/https。 |

**联网工具的健壮性（2026-10-12 升级，思路抄自 Agent Reach）**：

- `web_search` 现在是**多入口自动回退**：DuckDuckGo HTML → DuckDuckGo Lite → Bing。
  单入口改版/限流/403 是常态，只靠一家就等于“搜索没法用”。
- **失败入口降权**：刚失败过的入口排到最后，避免每次都先等一个已知不可用的入口超时；
  全部失败时仍会重试，不做永久禁用。
- 全部失败时错误文案带**每个入口的原因**（网络不通 / HTTP 403 / 没解析出结果），
  而不是一句笼统的“搜索失败”。
- `web_reader` 直连失败（403/超时）或**抓到反爬验证页**时，自动改用 Jina Reader 备用阅读器。
- **反爬页识别**：Cloudflare 的 `Just a moment...`、`performing security verification`、
  `ray id` + `/cdn-cgi/challenge-platform/` 等特征会被识别出来并明确报错——
  把验证页当正文喂给模型，比报错糟糕得多。特征只在开头 4KB 里找，正常文章提到这些词不会误判。
| `ask_user` | `question`, `options?` | 向用户提问并**停轮**：发 `need_input` 事件，用户下一句话就是回答。工具不能替用户编答案。 |

`web_search` 依赖外网：网络不通时返回明确错误（“搜索请求失败”），由模型向用户如实转达。

### 13.4 工作区可由用户设置

文件工具的沙箱根目录不再只能靠 `AGENT_FS_ROOT`：用户在界面上设置的目录存在数据库里，**按用户生效**，优先级高于环境变量。
接口见 [../api-contract.md](../api-contract.md) 第 12 节。

界面入口有两处：输入框左下角的「工作区」按钮（桌面端弹**系统目录选择框**，网页端退化为手填路径），
输入框左下角还有一个档位选择器（plan / confirm / auto），说明见下一节。
以及「记忆」面板里的工作区卡片。桌面端的目录选择靠 `electron/preload.js` 暴露的 `pickDirectory()`，

### 13.5 三档自主模式（2026-10-12 新增）

用户可在输入框左下角切换，存在 `agent_setting.agent_mode`（按用户），未设时看服务端 `AGENT_MODE`，最后回落 `confirm`。

| 档位 | 行为 | 实现位置 |
|---|---|---|
| `plan` | 只列计划，**一个工具都不注册**，提示词明确禁止执行 | `agentToolsFor` 早退 |
| `confirm` | 只读工具直接用；风险工具（write/edit/move/delete/memory_write）被 `confirmTool` 包住，不执行而是停轮问用户 | `web/agent_confirm.go` |
| `auto` | 维持原有的自主执行 | —— |

`confirm` 档的放行条件很严：**必须**「上一条 assistant 消息是确认请求」且「用户这条消息是短肯定答复」。
模型自己重试同一个调用不会被当成确认——它伪造不了用户消息。

### 13.6 Word / Excel 解析

### 13.7 Office 文档的读与写（2026-10-12 新增）
### 13.8 记忆系统的雍健性（2026-10-13 新增）

背景：记忆的读写原本**硬依赖**本地 Ollama 的嵌入模型。Ollama 一停，检索直接降级成“本轮无记忆”、写入直接被丢弃，
而且只写一行服务端日志——用户看到的是助手突然失忆且毫无提示。下面四处改动把它变成一条有降级的链。

| 环节 | 原行为 | 现行为 |
|---|---|---|
| 检索：嵌入可用 | 语义 + 关键词 + 实体融合（topK） | 不变 |
| 检索：嵌入**不可用** | 直接放弃，本轮无记忆 | 自动改走 `SearchLexical`：词面（中文二元组）+ 实体，**不依赖任何模型** |
| 检索：专名漏抽 | — | 新增**反向实体匹配**：库里存的实体反过来到查询里找 |
| 写入：嵌入不可用 | 丢弃这一条 | **仍然存下内容**（向量留空，`dim=0`），先靠关键词可搜 |
| 向量回填 | 无 | 自愈式：某次查询**成功嵌入**（证明服务活着）后，后台分批补算 `dim=0` 的块，两次间隔 ≥ 10 分钟 |
| 降级可见性 | 只写服务端日志 | 发 `memory` SSE 事件，聊天气泡显示「记忆本轮未生效」/「本轮记忆未保存」+ 悬停看原因 |

写入失败会返回警告（`saveAutoMemory` 的返回值）而不是静默丢弃：下一轮发现“助手忘了”时，
你能立即知道是**当时就没存上**，而不是记忆系统“没查到”。

抽取质量：提示词里写成硬规则——**只抽结论、不抽过程**，明确排除工具/脚本执行流水
（如「运行了 ppt-master/scripts/xxx.py」「已保存 id=…」）、动作日志、文件名/路径本身。

相关文件：`rag/store_lexical.go`（关键词检索 + 反向实体匹配）、`rag/backfill.go`（向量回填）、
`rag/extract.go`（抽取规则）、`web/kb.go` 与 `web/agent_memory.go`（降级可见性）。


**读**：`read` 现在支持三种 Office 格式，且对表格有专门处理——
docx 的表格按行拼成 `单元格 | 单元格`（单元格内不额外换行），xlsx 按行列用制表符分隔，
pptx 按页码分段输出（`[第 N 张幻灯片]`）。

**写**：新增 `office_edit` 工具，三个动作：

| action | 支持格式 | 说明 |
|---|---|---|
| `create` | docx / xlsx | 生成最小可用 OOXML（有文字/表格/单元格，无样式主题） |
| `replace` | docx / xlsx / pptx | 在 XML 文本节点里做字面量替换 |
| `save_as` | docx / xlsx / pptx | 整包另存到新路径（不覆盖已有目标） |

安全约束与 `write`/`edit` 完全一致：路径必须在工作区内（`FS_SANDBOX_DENIED`）、
覆盖已存在文件前必须先 `read`（`FS_NOT_OBSERVED`）、读过之后文件又被外部改过则拒绝（`FS_STALE_VERSION`）。
在「风险操作先问」档位下，`office_edit` 与 write/edit/move/delete 一样会先弹确认。

**明确不支持的两件事（如实声明）**：

1. **pptx 不能从零新建**。只带 slide 的包缺 `slideMaster`/`slideLayout`/`theme`，
   PowerPoint 会报“内容有问题”——与其生成一个打不开的文件，不如直接拒绝并给出替代做法
   （先用 PowerPoint 建模板，再用 `replace` 改文字）。
2. **`replace` 只在同一个文本节点内匹配**。Word 常把一句话拆进多个 `<w:r>`，
   跨 run 的情况这里匹配不到，会以 `FS_NO_MATCH` 如实报错，而不是装作改好了。

生成/修改后的文件都用与 `write` 同一套原子落盘（同目录临时文件 → fsync → 发布）。


`read` 现在支持 `.docx`（抽 `word/document.xml` 的正文段落）与 `.xlsx`（抽共享字符串 + 各工作表单元格，行内用制表符分隔）。
docx/xlsx 本质是 zip + XML，所以**没有引入任何第三方库**。
能力边界：只抽文字，不还原样式/公式/图表/批注；图片型内容抽不出来，会明确报错。
只暴露这一个动作，不开放任意文件读写或命令执行。

### 13.7 技能工具（2026-10-12 新增）

两个只读工具，用来按需查阅技能（"怎么做某件事"的说明书）。它们**所有档位都注册**（plan 档也给），
绑定当前用户；详细设计与自动检索链路见 [04-skills.md](04-skills.md)。

| 工具 | 参数 | 行为与约束 |
|---|---|---|
| `skill_search` | `query`（必填）、`limit`（默认 **8**，最大 **20**） | 在**已启用**技能的 `name` / `description` / `body` 上做 SQL LIKE，按 `name` 升序返回「名字：简介」（简介截 200 字）。没命中不是错误，正文为"没有匹配的已启用技能。可以换个关键词，或让用户在界面上启用更多技能。"。`Display`：`{query, count}`。 |
| `skill_read` | `name`（必填，精确匹配） | 返回简介 + 正文；正文超过 **6000** 字（`skillBodyMaxRunes`）截断并追加"...（技能正文较长，已截断）"。`Display`：`{name, runes, truncated}`。 |

**失败语义**（都以 `is_error` 回灌给模型，不终止循环）：

- `skill_search`：`query 不能为空`、`参数不是合法 JSON`、`数据库未初始化`、`搜索技能失败: ...`。
- `skill_read`：`name 不能为空`、`参数不是合法 JSON`、`数据库未初始化`；
  找不到 → `没有名为 "X" 的技能（用 skill_search 找准确名字）`；
  技能已关闭 → `技能 "X" 当前是关闭状态，请让用户在界面上启用它`。

**边界**：`skill_read` **没有 offset / 分页参数**，只返回前 6000 字，超长技能的后半部分模型拿不到；
`skill_search` 是 SQL LIKE，中文子串召回弱（没有分词）。真正做中文召回的是对话前的自动检索（见 04-skills.md 第 3 节）。

---

## 14. 2026-10-13：借鉴 Open WebUI 的四项升级

参考 open-webui v0.12.0 的设计（**只学思路、用 Go 重写**；它的许可证禁止改品牌，不搬代码）。

### 14.1 上下文压缩（`web/agent_compact.go`）

长对话把早期消息压成一段滚动摘要，只把「摘要 + 最近 N 条」发给模型。对应它的 `utils/context_compaction.py`。

- 开关：`AGENT_COMPACT`（默认开）、`AGENT_COMPACT_TRIGGER`（默认 24 条）、`AGENT_COMPACT_KEEP`（默认 10 条）。
- 水位：`conversation.summary` + `conversation.summary_upto`（摘要覆盖到哪条 `message.id`），
  距上次摘要又攒够 6 条新消息才重算，避免每轮都调模型。
- 摘要注入在系统提示词末尾，标为「早期对话摘要」并注明与最近消息冲突时以最近为准。
- 失败一律降级为「不压缩、发全量历史」——宁可多花 token，不丢上下文。
- 压缩发生时发一个 `compact` SSE 事件，前端可以提示用户。

### 14.2 风险操作：确认后直接执行（`web/agent_pending.go`）

原来的 confirm 档在用户点确认后，是**把「确认」当普通消息再喂给模型**、指望它重发同一个调用。
实测 qwen2.5:7b 收到「确认」只会回一句「请提供具体参数」，工具根本没跑——用户点了确认却什么都没发生。

现在：被拦下的那次调用原样存进 `agent_pending_tool`（按会话，最多一条），用户确认时**直接执行它**，
把 `[assistant(tool_calls), tool(result)]` 接在历史末尾再让模型继续。与模型听不听话无关。

- 取出即删除：重复点确认不会把删除/覆盖跑两遍。
- 用户回「取消 / 不要 / 算了 …」时清掉待确认调用（`isRejection`）。
- 有些后端（实测 Ollama）不发 `tool_call.id`，这里缺了就补一个确定性 id，
  否则 OpenAI 系后端会因为 `tool_calls[i].id ↔ tool_call_id` 对不上而拒掉整条请求。
- `need_input` 事件新增 `confirm` 字段：前端据此画「确认执行 / 取消」按钮，不再让用户自己猜该回什么字。

### 14.3 `ask_user` 支持候选项（`agent/toolkit/human.go`）

`options` 现在接受两种写法（可混用）：纯字符串，或 `{label, description}`。给了就要 2~5 个有效项。
归一化后的候选项随 `need_input.display.options` 下发，前端画成按钮，点一下就把标签发出去。
没给 `options` 时行为与以前完全一致（只问一句话）。

### 14.4 `skill_write` —— 让模型把流程沉淀成技能（`web/skill_write.go`）

对应 Open WebUI 的 `/skills:create`。参数 `name` / `description` / `body`；
名字会被规整成「小写 + 连字符」的 slug（`normalizeSkillName`），同名整条覆盖（复用 `model.UpsertSkill`）。
工具描述里带**技能写作规范**（何时使用 / 前置条件 / 怎么做 / 速查 / 坑 / 验证），模型照着排。

它**属于风险工具**：写的是技能库而不是用户文件，但会改变助手以后的行为，所以 confirm 档会先问、plan 档不给。
工具总数因此从 20 变为 21（`web/agent_test.go` 已同步）。

### 14.5 跨用户去重互相顶掉（真 bug，已修）

`kb_document.sha256` 原本是一个**全局**唯一索引，而 `FindDocumentBySHA` 的查重不带 `user_id`。
后果：两个用户存同一条内容（例如都说“我住在上海”）时，**后写的那条直接撞唯一键、内容根本没入库**，
而调用方拿到的却是 `duplicated=true`——记忆凭空消失，且没有任何告警。

修法（两处，缺一不可）：

- `model/kb.go`：唯一性从 `sha256` 改成 `(user_id, sha256)` 复合唯一；
- `model/code.go`：启动时显式 `DROP INDEX idx_kb_document_sha256`（AutoMigrate 只建不删，
  老库上不清掉这个索引的话 bug 依旧）；索引不存在时报 1091，当作已清理忽略。
- `rag/store.go`：`FindDocumentBySHA` 加上 `user_id` 条件，去重限定在同一用户内。

实测（2026-10-13）：两个用户存同一段文本 → 各自拿到 156 / 157 两条独立文档；
同一用户重复提交同一段文本 → 第二次 `duplicated=true` 且命中同一条（去重未丢）。

---

## 15. 2026-10-13（第二轮）：多助手 / 提示词库 / 会话与消息编辑

### 15.1 会话与消息的编辑能力

| 方法 | 路径 | 行为 |
|---|---|---|
| PATCH | `/api/conversations/:id` | 重命名会话（title 去空格、按 rune 截到 60） |
| POST | `/api/conversations/:id/regenerate` | 抹掉最后一次回答后重新生成，**SSE 与 /api/chat 同事件** |
| GET | `/api/conversations/:id/export` | 导出 Markdown（带 Content-Disposition） |
| PATCH | `/api/messages/:id` | 改这条 **user** 消息，并**截断其后所有消息**（返回 removed 条数） |
| DELETE | `/api/messages/:id` | 只删这一条 |

实现在 `web/chat_edit.go`；`postChat` 里「建 SSE 头 + sendEvent + 走 agent 还是直连」这段被抽成
`streamReply` 供 regenerate 复用，**postChat 的可观测行为与事件顺序未变**（有测试钉着）。

### 15.2 多助手（Assistant）

表 `agent_assistant`。一个助手 = 一套「人格 + 默认模型 + 档位 + 工作区」。
**核心语义：字段留空 = 继承用户全局设置**，不是「设为空」——所以改全局时，没覆盖过的助手会跟着变。
这条语义在 `model/assistant.go`、`web/assistant_resolve.go`、前端表单三处一致。

- 接口：`GET/POST /api/assistants`、`PATCH/DELETE /api/assistants/:id`（`web/assistant.go`）
- 接线：`/api/chat` 与 `/api/conversations/:id/regenerate` 接受 `assistant_id`；
  handler 用 `setAssistantID` 放进 gin.Context，`runAgentChat` 再 `resolveAssistantContext` 解析出
  本轮生效的 root / mode / soul（走 Context 而不加参数，避免改动 7 个调用点的签名）
- 界面：对话页左下角助手下拉（选择存 localStorage，助手被删/停用时自动回落为「不使用助手」）

### 15.3 提示词库（Prompt）

表 `agent_prompt`；接口 `GET/POST /api/prompts`、`PATCH/DELETE /api/prompts/:id`（`web/prompt.go`）。
前端 `templates/static/js/prompts.js`，渲染进 `#view-prompts`。

### 15.4 离线公式渲染

`templates/static/js/math.js`：支持 `$...$` / `$$...$$`、上下标、`\frac`、`\sqrt`、常用希腊字母与符号。
**这是轻量兜底，不是完整 TeX**（无网络，装不了 KaTeX）。不支持矩阵、`\begin{}` 环境、宏、颜色等。
将来要换成真 KaTeX，改 `math.js` 的 `render` 入口即可（文件头写了接入点）。

### 15.5 实测发现：工具多了，小模型会无视系统提示词

排查「灵魂不生效」时抓到的事实（已在 2026-10-13 用抓包复现）：

- 发出去的请求体**是正确的**：`messages[0].role=system`，内容包含灵魂原文；
- 同一个请求体直接打 Ollama：**去掉 tools → 遵守；带 3 个 tools → 遵守；带 21 个 tools → 无视系统提示词**；
- 在系统提示词**末尾再重申一次**灵魂也救不回来。

结论：本地 `qwen2.5:7b` 的注意力被工具模板挤掉，**属于模型能力边界，不是接线问题**。
影响：弱模型 + 大量工具时，灵魂/人设可能不生效。可行对策（按性价比）：
① 用云端模型；② 减少下发的工具数；③ 做成 Open WebUI 的 Tool Search——
工具只发名字、模型按需用 `search_tools` 取完整定义（需要在主循环里支持“取回定义后重发”）。

---

## 16. 2026-10-13（第三轮）：借鉴 Open WebUI 升级已有功能

这一轮不新增整块功能，而是把**已经有但不够好用**的地方向 Open WebUI 看齐。

### 16.1 停止生成（前端）

以前发出去就只能等。现在生成中发送键变成停止键，点击 `AbortController.abort()` 中断请求。
后端本来就用 `c.Request.Context()` 取消上游，所以中断请求就是正确的中断方式（不需要额外接口）。
中止**保留已生成的内容**，不弹错误，只 toast 一句。重新生成/编辑后重生成也走同一条路。

### 16.2 代码块复制（前端）

`md.js` 在 `<pre class="md-code">` 里生成 `.md-code-copy` 按钮（md.js 只产出字符串），
`chat.js` 在消息容器上用**事件委托**处理点击，复制兄弟 `<code>` 的文本。CSS 绝对定位右上角，
`pre` 预留上内边距避免盖住首行。`build/md-check.cjs` 补了 3 条相关断言（未削弱安全断言）。

### 16.3 输入框 `/` 提示词快选（前端）

行首输入 `/` 弹出提示词候选（数据来自已有的 `GET /api/prompts`，最多 8 条，标题模糊过滤）；
↑/↓ 移动、Enter 选中、Esc 关闭；选中后插到光标处并**替换掉 `/关键词`**，保留用户已写的其他内容。
候选打开时 Enter 是选中而不是发送。列表拉取失败就静默不显示（便利功能，不能干扰输入）。

### 16.4 对话参数（前后端贯通）

`/api/chat` 与 `/api/conversations/:id/regenerate` 新增三个可选字段：

| 字段 | 语义 |
|---|---|
| `system_prompt` | **追加**在系统提示词末尾的额外指令（不替换内置提示词）；trim 后按 rune 截到 2000 |
| `temperature` | 0~2，越界夹取；`0` 沿用既有约定 = 「不设置」 |
| `max_tokens` | 1~65536，越界夹取；<=0 = 不设置 |

实现在 `web/chat_params.go`（`chatParams` + `setChatParams` / `chatParamsFromContext` / `normalizeChatParams`），
经 gin.Context 传给 `streamReply` / `runAgentChat`（与助手的传参方式一致，不改函数签名）。
agent 路径把额外指令**追加在末尾**（离用户消息最近，模型更容易遵守）；直连路径在最前面插一条 system。
`agent.Config` 新增 `Temperature` / `MaxTokens`，`llm/ollama.go` 映射到 `Options.Temperature` 与 `Options.NumPredict`；
openai / anthropic / gemini 本来就在用这两个字段。**三个字段全不传时行为与改造前逐字节一致。**

前端：输入行加「参数」按钮，面板按会话存 `localStorage`（`blogmy:chatparams:<id>`，草稿用 `:__draft`），
只把设过的字段发出去。

### 16.5 知识库把「AI 记忆」与「我的文档」分开（借鉴它的记忆页）

- `GET /api/kb/documents?source=all|memory|document`（非法值回落 all）；列表项增加 `source`，响应增加 `total`；
- `GET /api/kb/stats` 增加 `by_source: {document, memory, profile}`（三项之和恒等于 `documents`）；
- 前端加分段筛选（全部 / 我的文档 / AI 记忆）、来源徒标（资料 / AI 记忆 / 用户画像）、逐条删除（二次确认）、按来源的空态文案。

因为不能改 `rag/store.go`，列表查询改在 `web/kb.go` 里直接用 `model.DB`，筛选与计数口径写在 `kbSourceFilter` / `kbCountBySource` 的注释里。

### 16.6 验证与已知缺口

已验证：`go test ./...` 全过；`node --check` 六个前端文件全过；`md-check` PASS；
端到端 `scripts/e2e-upgrade2.cjs` **11/0**（额外系统提示词真的生效：直连路径回复就是提示词要求的那一行；
参数越界被夹取；kb 三个筛选值 + 非法值回落 + `by_source` 三项之和等于 `documents`）。

### 16.7 前端验证：用 jsdom 跑真 DOM

本机 Chrome 在沙箱里起不来（mojo IPC `拒绝访问`，Playwright 同样被拦），
所以改用 **jsdom** 把真实的 `chat/index.html` + 真实 JS 跑起来做交互验证：
`scripts/verify-ui.cjs`（需 jsdom，脚本会去 `JSDOM_PATH` / `NODE_PATH` 找）。

默认验仓库里的 `templates/`；设 `TEMPLATES_DIR` 可以改成验**打包产物**，例如：
`$env:TEMPLATES_DIR='E:\blog-my\dist\win-unpacked\resources\templates'`。
已用这种方式直接对 `dist/win-unpacked` 跑过一遍（30/0），所以“打包版里就是这套界面”是验过的。

做法：拉真页面 HTML → 按 `index.html` 里真实的顺序注入脚本 → `fetch` 桩补上 session cookie
（`/api/chat` 返回**永不结束的流**，用来测「生成中」状态）→ 像用户一样输入/点击/按键。

**30 项全过**，覆盖：代码块复制按钮渲染 + 点击真的进剪贴板；参数面板展开、三个字段、改动按会话写进 localStorage；
`/` 候选弹出、列出提示词、Enter 填入正文并收起；停止键生成中变 `is-stop`/aria-label「停止生成」、点击后恢复且保留已生成内容；
知识库分段控件三个按钮、来源徒标、切到 AI 记忆显示专属空态、`aria-pressed` 选中态、切回全部数据重现。

**仍然没覆盖的**：jsdom 不渲染像素，所以**视觉层面**（实际外观、间距、窄屏换行、遮挡）仍然只能靠人看。
要真截图得在有显示环境的地方跑（本机直接开应用看一眼即可）。
