# task-43 独立验收：AI 自主工具调用 + 记忆系统（后端与安全部分）

> 立场：实现方的自证不算数。本文每条结论都有**我自己跑出来的原始命令与输出**；没验到的一律标 ⏳。
> 写域：`docs/verify/**` + `agent/**/*_test.go`（自写用例，文件名与实现方不冲突）。**没有改任何实现代码**，没有 git 写操作。

## 0. 结论速览

| 契约 §9 | 项 | 判定 | 关键证据 |
|---|---|---|---|
| 9.1 | build / vet / test / gofmt 全清 | ✅ | build=0 vet=0，`gofmt -l .` = **0 行**，`go test -count=1 ./...` 全 ok |
| 9.2 | CUA 未受影响 | ✅ | CUA 冻结路径 `git status` **0 行**改动 + `go test ./web/ -run TestCua` ok（共享文件 hunk 逐行核对见 §2）|
| 9.3 | 向后兼容：tools 缺省时事件序列与改造前一致 | ✅ | 真路由 + 真 MySQL + 真会话签名：只有 delta…done；A/B 反证见 §5 |
| 9.4 | 工具真能跑（mock 脚本 read+write）| ✅ | 文件真被改、事件顺序 read→write→done、轨迹落库 2 条 |
| 9.5 | 观察策略三态 / 版本守卫 / 原子写 | ✅ | 6 条自写用例，含 **mtime 单独变化也要 STALE**、被拒读不得记 absent |
| 9.6 | 沙箱红线（含负向对照）| ✅ | 8 条对抗用例（junction/链/`..`/绝对路径/长路径/目录/ADS/正控）+ 变异体证明灵敏度 |
| 9.7 | 记忆可用 + 检索失败不阻断 | ✅ | 工具级 + 块级 + **真 Ollama 端到端注入** + 独立进程降级对照 |
| 9.8 | 前端 3 页签 / 密钥右上角 / 记忆面板 / 工具卡片 / CUA 回归 | ✅ | **verifier 自写探针**：结构层 28 PASS / 0 FAIL；真 deepseek-chat 层 **36 PASS / 0 FAIL**；3 组负向对照全红（详见 §11）|
| 9.9 | 负向对照 | ✅ | 两组变异 + 两组 A/B；变异体全部能编译，还原用 sha256 证明 |
| — | Lead 修的 agent.SetStreamer 生产缺口 | ✅ | 真机 A/B：真二进制 done；去掉安装行的变异二进制**复现**未安装模型调用实现 |

## 1. 我写了什么（全部独立用例，不是复跑实现方的）

| 文件 | 内容 |
|---|---|
| `agent/fs/verify_sandbox_test.go` | junction 逃逸（write/read/深层/edit/read_image）、junction 指向根内**必须放行**、链接链、`..` 多种写法 + 前缀兄弟目录、根外绝对路径 + `\\?\` 长路径、目录非普通文件、冒号/ADS |
| `agent/fs/verify_observation_test.go` | 符号链接能力独立确认、**只改 mtime 也必须 STALE**、陈旧优先于字面量匹配、被拒读不得记 absent、三态流转、createIfAbsent 输给外部创建、无 tmp 残留、并发 edit 5 轮 |
| `agent/verify_http_it_test.go` | 真路由（web.Setup，含 authRequired）+ 真 MySQL + 自签会话 cookie：向后兼容 A/B、工具事件负向对照、read+write 循环、记忆降级、Setup 装缝断言、记忆正/负端到端 |
| `agent/verify_memory_test.go` | memory_search/memory_write 真假后端、`<memory>` 块生成与失败传播、topK 夹取、MaxToolCalls/MaxIterations 边界 |
| `agent/verify_db_diag_test.go` | env 门控的只读诊断（tool_trace 隔离性）与验收记忆文档清理 |

复跑（全部默认跳过，需显式开闸）：

```powershell
go test ./agent/fs/ -run TestVerify -count=1                    # 沙箱/观察（无外部依赖）
$env:AGENT_IT="1"; go test ./agent/ -run TestVerify -count=1     # HTTP/记忆（真 MySQL）
$env:VERIFY_MEMORY_E2E="1"; go test ./agent/ -run TestVerifyMemoryInjectionEndToEnd -count=1 -v  # 真 Ollama 正面
$env:VERIFY_DB_DIAG="1"; go test ./agent/ -run TestVerifyDBIsolationDiag -count=1 -v            # 隔离性诊断（只读）
$env:VERIFY_DB_CLEAN="1"; go test ./agent/ -run TestVerifyDBCleanAcceptanceMemory -count=1 -v   # 清理验收文档
```

## 2. CUA 冻结（git 核对 —— 实现方全程没跑过 git）

```
$ git status --short -- cua web/cua.go web/cua_llm.go web/cua_test.go electron packaging templates/static/js/cua.js
（空）；同一路径集合的改动行数 = 0
$ go test ./web/ -run TestCua -count=1
ok  blog-my/web
```

**一个如实的细节**：冻结路径本身零改动，但**共享文件**里有 3 处 hunk 引用了 cua（style.css 把 `.app[data-view="cua"] .model-wrap` 与新的 `.memory-panel` 合并；chat.js 的 VIEW_TITLES 删掉 keys/kb 但**保留 cua**；index.html 加了注释）。逐行看过：CUA 的分支与取值都还在，属 UI 任务的合理合并，不是逻辑改动。CUA 渲染层面的复核留给前端那一轮（§9.8）。

## 3. 沙箱红线（最高优先，含负向对照）

### 3.1 我自己的逃逸尝试（全部被拒，且根外无副作用）

```
--- PASS: TestVerifyJunctionEscapeDenied       # 根内 junction → 根外：write/read/深层/edit/read_image 全拒，
                                               #   并断言根外没有出现 pwn.txt、secret.txt 未被改动
--- PASS: TestVerifyJunctionInsideRootAllowed  # 链接指向根内必须放行（防过度拦截；写落到真实目录）
--- PASS: TestVerifyJunctionChainEscapeDenied  # j1 → j2 → 根外 的链接链也拒
--- PASS: TestVerifyTraversalSpellings         # ../ ..\ a/../../ ./../ .. 以及同前缀兄弟目录 AAA-evil
--- PASS: TestVerifyAbsoluteAndLongPathPrefix  # 根外绝对路径、C:\Windows\win.ini、\\?\ 长路径前缀
--- PASS: TestVerifyNonRegularTargetRejected   # read/read_image 作用目录 → FS_SANDBOX_DENIED
--- PASS: TestVerifyColonInPath                # 冒号/ADS 形态 → FS_IO_ERROR（失败关闭，根外无文件）
```

### 3.2 独立确认实现方那句「本机无 SeCreateSymbolicLinkPrivilege」

```
    verify_observation_test.go:40: 符号链接**建不了**（确认实现方说法）：
      os.Symlink: A required privilege is not held by the client.
--- PASS: TestVerifySymlinkCapability
```

真符号链接分支在这台机器上**测不了**；用 junction 作为等价物做了覆盖（实现方说法**成立**，我独立复现了错误）。POSIX 分支仍然只是编译过 ⏳。

### 3.3 负向对照：把修复改回第一版写法，我的用例必须转红

变异体放在**模块外**（`%TEMP%\agent-verify\mut-sandbox-evalsymlinks.go`），用 `go test -overlay` 注入 —— 原始文件**一个字节都没动**：

```
变异前 sandbox.go sha256 = 8DA15D8689D8B5C1DC28D0A770F55C73F60B6273341E753F919B1090753C31E8
变异体 go vet -overlay = 0                     ← 先证明「注入后能编译」（不能编译的注入不算证据）
go test -overlay ... -run TestVerifyJunction:
    verify_sandbox_test.go:101: 逃逸成功：write esc/pwn.txt 没被拒
    verify_sandbox_test.go:183: 链接链逃逸成功
    --- FAIL  （我的断言确实有灵敏度）
原始实现下同一组用例: ok
变异后 sandbox.go sha256 = 8DA15D8689D8B5C1DC28D0A770F55C73F60B6273341E753F919B1090753C31E8  ← 与变异前一致
```

**结论**：第一版的 EvalSymlinks 写法确实挡不住 Windows junction（我独立复现了那个洞），现在的逐段 Lstat+Readlink 修复**真的挡住了**，不只是过了实现方那一个用例。

## 4. 观察策略 / 版本守卫 / 原子写 / 并发

```
--- PASS: TestVerifyMtimeOnlyChangeIsStale     # 内容与大小都不变、只动 mtime → FS_STALE_VERSION；
                                               # 且陈旧 + old_string 根本不存在时仍报 STALE（绝不报找不到匹配）
--- PASS: TestVerifyThreeStateFlow             # 未读先 edit → FS_NOT_OBSERVED；读缺失 → FS_NOT_FOUND；write 可创建；
                                               # 外部删除后再 read → 确认不存在；edit → FS_NOT_FOUND
--- PASS: TestVerifyDeniedReadDoesNotRecordAbsent # 读目录被拒后 edit 报 NOT_OBSERVED（不是 NOT_FOUND）
--- PASS: TestVerifyCreateIfAbsentLosesRaceToExternalCreator # 外部抢先创建 → FS_NOT_OBSERVED，外部内容未被覆盖
--- PASS: TestVerifyNoTmpLeftovers             # 成功写 / 失败写之后都不留 .tmp
--- PASS: TestVerifyConcurrentEditsExactlyOneWins # 我自写并发用例：2 goroutine × 5 轮，恰好 1 成功 + 1 STALE
--- PASS: TestVerifyMaxToolCallsBoundary       # MaxToolCalls=2 → ErrMaxToolCalls 且 tool_result 恰好 2 次
--- PASS: TestVerifyMaxIterationsBoundary      # MaxIterations=2 → ErrMaxIterations（实现方承认 AGENT_MAX_* 未压测，已补）
```

## 5. 向后兼容与工具链路（真 HTTP + 真 MySQL）

用 `web.Setup` 装**完整路由**（含 authRequired），会话 cookie 按 middleware.go 的算法自己签（HMAC-SHA256(APP_SECRET, uid)）——顺带独立复核了会话签名确实能过鉴权。

```
老路径事件序列 = [delta delta delta delta delta delta delta delta done]
AGENT_TOOLS=1 但请求不带 tools = [delta ... done]        ← 开关开着、请求没要，也仍是老路径
负向对照（tools=true 且开关开着）= [tool_call tool_result delta done]  ← 证明上面那条不是永远为真的假证据
read+write 循环 = [tool_call tool_result tool_call tool_result delta delta done]
  · read 的 detail 里含盘上的真实内容；dst.txt 内容 = copied-by-tool
  · tool_trace 落库 2 条（read、write）
```

改造前的事件集合（`git show HEAD:web/chat.go` 里 sendEvent 的全部取值）= delta / done / error；且 `git diff HEAD -- web/chat.go` 显示老路径**只增不改**（新增 Tools 字段、跳过 tool_trace 历史消息、shouldUseAgent 早返回）。源码与实测两侧都成立。

## 6. 记忆系统

```
--- PASS: TestVerifyMemoryTools            # memory_search 回命中正文、query/limit 传下去；memory_write 正文传下去；
                                           # 后端报错时如实报错（不假装检索到空结果）
--- PASS: TestVerifyMemoryBlock            # 生成的块含 <memory> 与命中正文；失败时返回 error 且不产出块；topK 夹取
--- PASS: TestVerifyMemoryDegradation      # 独立进程、嵌入端点不可达：SSE 仍以 done 收尾、回复正常、
                                           #   system prompt 里没有 <memory>（长度 1209）
--- PASS: TestVerifyMemoryInjectionEndToEnd # 真 Ollama(11434) 入库 201 + 录音 streamer 抓到 system prompt：
                                           #   含 <memory> 且含本轮刚写入的暗号（system 长度 1814）
```

## 7. Lead 修的生产缺口：agent.SetStreamer 只在测试里装过

按「单测绿 ≠ 生产可用」独立复验，三层证据：

1. **断言**：web.Setup(r, reg) 之后 agent.Streamer() != nil —— 我的 HTTP 用例**自己不再装缝**，一旦 Setup 漏装整条链路直接炸（不再被测试自己装的那行盖住）。
2. **变异（能编译）**：把 web/web.go 的安装行换成 agent.SetStreamer(nil)（保留 import 使用以便编译）→ go vet -overlay = 0，我的断言 FAIL、HTTP 用例也 FAIL。
3. **真机 A/B**：

```
真二进制  @ http://127.0.0.1:10697：含[未安装模型调用实现]=False  含[event: done]=True  含[event: error]=False
变异二进制 @ http://127.0.0.1:8184 ：含[未安装模型调用实现]=True   error 负载 = agent: 未安装模型调用实现
残留 prod-app = 0 ; mut-app = 0        ← 杀进程只用 PID + taskkill /PID <pid> /T /F，没用过 /IM
```

web/web.go sha256（我全程没改它，变异体在模块外）：7F961B27E92EE927E80865500BD3E42B91F82CFA207D45F70774A1E75C0DD7E1。

## 8. 我发现的问题（如实列出）

**F1（会让「全绿」不成立，但不是产品缺陷）**：web/agent_http_it_test.go 对消息表的查询**没有按会话过滤**（model.DB.Order 全局 Find），而真 MySQL 是所有用例/所有人共享的。实测它会取到别人的轨迹行：

```
全局 Order(id asc) 的第一条 tool_trace = msg#51 conv=28，内容是别人 read 一条「蓝鲸计划 / 暗号 ORCA-7731」的轨迹
库里最近 6 条 tool_trace 分属 user=7cb2c8a0-... / 00000000-0000-0000-0000-000000000001 / 77223a87-... 等
→ $env:AGENT_IT="1"; go test ./web/ -run TestChatHTTP  →  --- FAIL: TestChatHTTPToolsEndToEnd
  （它断言 tool_trace[0].Name == write，实际取到了别人的 read 行）
```

不是产品回归（工具链路本身全绿，见 §5），是**用例不隔离**。修法：按 conversation_id 过滤（我的用例第一版有同样隐患，已在我这边改掉并加了 vConvIDs）。

**F2（本轮已修）**：web/web.go 一度带 UTF-8 BOM，gofmt -l . 出 1 行；报给 Lead 后已修，现在 gofmt -l . = 0。（我自己的 agent/verify_http_it_test.go 也飘过一次注释对齐，gofmt -w 修好。）

**F3（测试方法学约束）**：web.kb() 把嵌入端点缓存在**进程级单例**里，同一进程内改 OLLAMA_BASE_URL 不生效 —— 我的第一版用例把这点当成了 FAIL，查清后改成「降级证据必须另起进程」。生产无影响（env 启动时固定），但任何在进程内切端点的降级对照都是**无效证据**。

**F4（低危，失败关闭）**：write 传冒号/ADS 形态的名字时，给模型看的错误文本会带**绝对路径**（例如 io error on "sub.txt:evil": link C:\Users\woshiss\...\.sub.txt:evil.<rand>.tmp ...: The parameter is incorrect.）。沙箱没被绕过（根外无文件），但 §4.5 要求面向模型的措辞只给相对路径，这里漏了底层 OS 错误原文。

**F5（措辞/顺序的细微差异）**：edit 作用于**目录**时报 FS_NOT_OBSERVED（观察守卫在前），read/read_image 报 FS_SANDBOX_DENIED（非普通文件检查在前）。三个入口都**拒绝**了，红线成立；只是错误码取决于检查顺序。另外确认**被拒的读不会把目标记成「确认不存在」**（不污染后续判定）。

## 9. 未覆盖面（⏳ 明确没验）

1. ⏳ **前端（§9.8）**：3 页签、密钥右上角、记忆并入对话视图、工具卡片渲染、CUA 页签与弹窗 —— task-41 还在做，等 Lead 通知后单独出一份。
2. ⏳ **真实云端模型的工具调用**：全部用 mock 驱动，没联网实测任何真模型（实现方也承认）。
3. ⏳ **真符号链接分支**：本机无权限建符号链接（已独立确认），只有 junction 覆盖；POSIX 版本令牌分支只编译未运行。
4. ⏳ **TOCTOU / 硬链接**：resolve 与真正读写之间没有 fd 加固，硬链接也没防护 —— 我没做利用演示，实现方也列为已知缺口。
5. ⏳ **Electron 壳内**：所有 HTTP/浏览器层验证都在无壳环境，未在 Electron 里跑。
6. ⏳ **agent.SetStreamer 是进程级单例**：并发用例下按包切换 streamer 不安全（实现方注释里也写了），我没压这条并发。

## 10. 我造成的一次事故（如实记录）

我把 -overlay 用的变异**片段**（docs/verify/tmp/mut-sandbox-evalsymlinks.go）放在模块内，被 go build ./... 收进去，**把全仓构建弄挂了**（undefined: Error）。Lead 报来后按推荐方案处理：变异体挪到模块外 %TEMP%\agent-verify\，docs/verify/tmp/provider-probe.go 一并改成 .go.txt；现在 docs 下 .go 文件数 = **0**，go build ./... = 0，负向对照仍可复现。教训：**给 -overlay 用的片段绝不能留在模块内**。

证据日志：`docs/verify/tmp/task43-gates.log`。
---

## 11. §9.8 前端验收（verifier 自写探针，不复用 cua-ui 的探针）

探针是我自己写的 `docs/verify/agent-ui-probe.mjs`（**不读也不复跑** `%TEMP%\cua-ui-verify\` 里那份），断言与它的不重合。

```powershell
# 起服务（隔离根目录：全流程只在我的自建目录里操作，绝不碰真实文件）
$env:AGENT_TOOLS="1"; $env:AGENT_FS_ROOT="$env:TEMP\agent-verify\ui-root"; $env:APP_PORT="10722"; $env:APP_HOST="127.0.0.1"
& "$env:TEMP\agent-verify\blog-my-ui.exe"
# 结构+交互层（不需要模型）
node docs/verify/agent-ui-probe.mjs --base http://127.0.0.1:10722 --fs-root "$env:TEMP\agent-verify\ui-root"
# 真 deepseek-chat + 真 fs 工具层（自主工具调用复现 + 工具卡片）
node docs/verify/agent-ui-probe.mjs --base http://127.0.0.1:10722 --fs-root "$env:TEMP\agent-verify\ui-root" --with-model
# 负向对照前先证明探针自身「能跑」（等价于编译通过）
node --check docs/verify/agent-ui-probe.mjs   # exit=0
```

**汇总**：结构+交互层 `PASS=28 FAIL=0 SKIP=1`；真模型层 `PASS=36 FAIL=0 SKIP=0`。

| Lead 的要求 | 判定 | 我跑出来的证据 |
|---|---|---|
| 1. 三页签 chat/models/cua，旧 keys/kb 页签与区块为 0 | ✅ | 源码 `data-view` 集合 = [chat,models,cua]；浏览器 `a.view-tab` 恰 3 个，逐个点击后 `.app[data-view]` 跟随；`#view-keys,#view-kb` 的 DOM 计数 = 0 |
| 2. 密钥在右上角 / 弹层开关 / 焦点回按钮 / `#/keys` 路由可达 | ✅ | `#keys-open.closest(".topbar") != null`；点击后 `#keys-dialog.open=true` 且按钮 `aria-expanded=true`；关闭后 `document.activeElement.id == "keys-open"`、`aria-expanded=false`；`#/keys` 直达仍能打开弹层 |
| 3. 记忆面板在对话视图内 / 默认折叠 / 其他视图隐藏 / CRUD 真能跑 | ✅ | `details#memory-panel` 初始 `open=false`；可见性 `{chat:true, models:false, cua:false}`；面板内新增 → `#kb-doc-list` 出现我的标记 `VERIFY-UI-…` → 删除（原生 confirm 自动接受）→ 列表里消失 |
| 4. 工具卡片 运行中 / 完成 / 失败 / 可折叠 / 长结果内部滚动 | ✅ | 真模型一次跑出 2 张卡：MutationObserver 记录到 `tool-card is-running` 帧，完成后 `is-ok` 且状态「完成」；`details.tool-card-detail` 默认 `open=false`、点开后正文非空；长结果 `pre.tool-card-body` `overflowY=auto`、`maxHeight=240px`、`scrollHeight=7143 > clientHeight=238`；失败卡 `is-error` + 状态「失败」+ 摘要 `read：失败 — [sandbox: 拒绝访问根目录之外的路径]` |
| 5. CUA 回归 | ✅ | 页签能进、`.view-tab-beta` 文本 = 测试版、`#cua-beta-dialog` 弹出且 `role=dialog` / `aria-modal=true` / `aria-labelledby` 齐；ESC 关闭后焦点回 CUA 页签（`data-view=cua`）；`#view-cua` 内 `.tool-card/.tool-cards` 计数 = **0**（零卡片泄漏）|
| 6. 390px 无溢出 + 零真实 JS 异常 | ✅ | 三个视图 `scrollWidth == clientWidth == 390`（无横向溢出）；`pageerror = 0`；`console.error` 仅剩 `/favicon.ico` 的 404（按 `msg.location().url` 精确排除）|

### 11.1 真模型自主工具调用：我自己的复现（独立于 cua-ui 那条）

真 `deepseek-chat`（服务端默认 key 来自 `.env` 的 `DEEPSEEK_API_KEY`）+ 真 fs 工具，提示词要求「先 read `notes.md`，再 write 到 `copy.md`」。结果：

```
[PASS] B-real-model-wrote :: 盘上真出现了 copy.md，内容是模型自主写出的
       证据: {"path":"…/ui-root/copy.md","head":"﻿# 验收用笔记\n暗号：UI-VERIFY-7731\n负责人：verifier\n"}
[PASS] B-toolcard-ok :: 有卡片转成完成态   证据: 2
```

暗号 `UI-VERIFY-7731` 是我自己造的；根目录 `%TEMP%\agent-verify\ui-root` 也是我自己建的，**没有碰任何真实文件**。另外我反过来要求它读 `C:/Windows/win.ini`，沙箱拒绝并渲染成失败卡片 —— 等于在 UI 层又独立验了一次 §9.6 红线。

### 11.2 负向对照（3 组，探针自身先过 `node --check` = 0）

| 编号 | 变异（运行时拦截，不改仓库文件）| 必须转红 | 实测 |
|---|---|---|---|
| N1 | 拦截 HTML，抽掉 `#keys-open` 按钮 | 密钥断言 | `B-keys-topbar` **FAIL** ✓ |
| N2 | 拦截 HTML，把 `#memory-panel` 的 id 改掉 | 记忆断言 | `B-memory-collapsed` + `B-memory-scope` **FAIL** ✓ |
| N3 | 拦截 `chat.js`，把 `onTool(event, tool)` 替换成 `void 0`（`changed=true`）| 工具卡片断言 | `B-toolcard-exists/ running/ ok/ collapsible/ scroll/ error` **6 项全 FAIL**（PASS=29 FAIL=6）✓ |

N3 里 `B-real-model-wrote` 仍然 **PASS** —— 说明后端确实执行了工具、文件确实写了，只是前端渲染被掐掉；这条恰好证明「卡片断言绑的是那一行渲染代码」，而磁盘断言与它互相独立。

### 11.3 三个「差点被我误判成产品缺陷」的地方（如实记录）

1. **390px 点页签超时**：第一版探针直接点页签失败，看着像窄屏不可用。我没有当成 FAIL，而是先量几何：`.sidebar` 在 ≤820px 是 `position:fixed; transform: translateX(-101%)`（x=-303），并且有可见的汉堡 `#sidebar-toggle`（aria-label 切换侧栏）。`git show HEAD:templates/static/css/style.css` 里**同一条媒体查询与 `translateX(-101%)` 已存在** → 是**既有**的 off-canvas 设计，不是 task-41 回归。改成「先点汉堡再切页签」后三项全 PASS。
2. **记忆删除看起来没生效**：`kb.js:184` 用 `window.confirm` 二次确认，而 Playwright 默认 **dismiss**（等于点了取消）。加 `page.on("dialog", accept)` 后删除正常，且探针把 confirm 文案记进了 INFO（证据里能看到 `删除「验收记忆 …」？该文档的全部检索块会一起删除，不可恢复。`）。
3. **`is-running` 态看不见**：fs 工具几毫秒就返回，200ms 轮询抓不到那一帧。我没有直接写 FAIL，而是改用 `MutationObserver` 记录每次 class 变化 —— 记录到了 `tool-card is-running` 帧。

### 11.4 `/favicon.ico` 404 归因（按要求单列）

```
$ git grep -n -i favicon HEAD        # 只命中 docs/day23 的历史记录，正文明确写着「后端本就没有该路由」
   docs/day23/notes.md: 「/favicon.ico 无路由…既有行为，不是本次引入；验收脚本已单独排除并注明」
浏览器 console.error 的 URL: http://127.0.0.1:10722/favicon.ico   （404 的 text 里不带 URL，所以按 msg.location().url 排除）
除 /favicon.ico 外：badResponses = 0、failedReqs = 0、pageerror = 0
```

结论：该 404 **早于本次改动**（HEAD 里既无路由也无静态文件，且历史文档已记录），按 URL 精确排除，**不污染**「零真实 JS 异常」的结论。

### 11.5 §9.8 未覆盖（⏳）

1. ⏳ **Electron 壳内**：本轮全部在无壳 Chromium（headless）里跑；壳内行为未验。
2. ⏳ **视觉人工核对**：本会话模型没有图像输入能力，我没有「看过」截图 —— 结论全部来自 DOM/几何/计算样式测量；截图留在 `docs/verify/tmp/ui-390-chat.png`、`ui-390-models.png`、`ui-390-cua.png` 供人眼复核。
3. ⏳ **只测了 390px**（加桌面 1440px），没测 320/768/1024 等断点；没做完整无障碍审计（只断言了 role/aria-modal/aria-labelledby/焦点回归这几个契约点）。
4. ⏳ **CUA 真实运行**未验（按纪律禁止真实 CUA/GUI 运行），只验了页签/角标/弹窗/焦点/零泄漏。
