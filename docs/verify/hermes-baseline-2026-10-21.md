# Hermes 改造前质量门基线（2026-10-21）

> 角色：验证工程师（Gatekeeper）。
> 本文件只记录**实测**结果；没有执行的项目一律写「未验证」，不做推测。
> 采集窗口：2026-10-21 10:29–10:50（+08:00）。基准提交：`7e9528a`（main）。
> 脚本：`scripts/verify-hermes.ps1`（见 §9）。改造前后都用它跑同一条命令。

---

## 1. 环境

| 项目 | 实测值 | 来源 |
| --- | --- | --- |
| OS | Microsoft Windows NT 10.0.26200.0 | `[System.Environment]::OSVersion` |
| Go | go1.25.3 windows/amd64 | `go version` |
| go.mod | `module blog-my` / `go 1.25.3` | `go.mod` |
| Node | v24.16.0 | `node -v` |
| pnpm | 未安装（`pnpm -v` → CommandNotFoundException） | `pnpm -v` |
| GOPATH | `C:\Users\woshiss\go` | `go env` |
| GOPROXY | `https://goproxy.cn,direct` | `C:\Users\woshiss\AppData\Roaming\go\env` |
| 默认 GOCACHE | `C:\Users\woshiss\AppData\Local\go-build` | `go env GOCACHE` |
| 默认 GOMODCACHE | `C:\Users\woshiss\go\pkg\mod` | `go env GOMODCACHE` |
| 仓库内可用缓存 | `.gocache-hermes\`、`.gomodcache-hermes\`、`.gotmp-hermes\`（未跟踪，且**没有被 .gitignore 覆盖**） | `git status` |

**环境约束（实测，影响全队）**：本会话的受限 shell 令牌**不能写**默认 Go 缓存目录。
不做处理直接跑 `go vet`/`go test` 会出现大量
`writing go.mod cache: ... Access is denied`（写 `C:\Users\woshiss\go\pkg\mod\cache`）
和 `open C:\Users\woshiss\AppData\Local\go-build\...: Access is denied`。
把 `GOCACHE` / `GOTMPDIR` / `GOMODCACHE` 指到**工作区内**可写目录后一切正常（脚本已内置）。

---

## 2. git 现状（原样记录）

全程**未**执行 stash / checkout / reset / commit / clean，工作树原样保留。

`git rev-parse HEAD` = `7e9528a5a6cf081d6a0f45c19f76c582b43a8c20`，分支 `main`
`git log --oneline -5`：`7e9528a docs: expand project README` / `4b19f18 chore: reorganize project structure` / `4a01844 Initial commit`

T0（10:29:56）`git status --porcelain`：**12 个修改 + 4 个未跟踪**

```
 M .gitignore
 M electron/main.js
 M electron/preload.js
 M internal/agent/fs/sandbox.go
 M internal/web/agent.go
 M internal/web/agent_pending.go
 M internal/web/agent_settings_test.go
 M internal/web/cua.go
 M internal/web/cua_test.go
 M internal/web/user.go
 M templates/chat/index.html
 M templates/static/css/style.css
?? Dockerfile
?? _dsh_probe.txt
?? docs/agent/11-hermes-parity.md
?? templates/static/js/public-access.js
```

`git diff --stat`（T0）：`12 files changed, 417 insertions(+), 12 deletions(-)`。

> 10:30 之后其他工程师开始施工（新增 `internal/agent/evolve/`、`internal/agent/toolsys/`、
> `internal/web/hermes_guard.go` 等），dirty 条目数从 16 涨到 34。**基线以 T0 为准**，时间线见 §7。

---

## 3. 命令结果

| # | 命令 | 退出码 | 耗时 | 失败包 | 结论 |
| --- | --- | --- | --- | --- | --- |
| 1 | `go build ./...` | 0 | 9,360 ms | — | **绿**（10:29:56，施工开始前） |
| 2 | `go vet ./...`（默认缓存） | 1 | 8,724 ms | — | 环境性失败，无代码结论（§4.1） |
| 3 | `go vet ./...`（工作区缓存，10:32:36） | 1 | 52,725 ms | `blog-my/internal/agent/evolve` | 并发施工中的新包，非基线（§7） |
| 4 | `go vet` 11 个改造前既有包 | 0 | — | — | **绿**（`go list ./...` 共 13 个包，减去施工中的 evolve、toolsys = 11） |
| 5 | `gofmt -l cmd internal` | 0 | 71 ms | — | **绿**，未格式化文件 **0** 个 |
| 6 | `go test -count=1 ./internal/agent/... ./internal/llm/...` | 1 | 58,775 ms | `internal/agent/fs`、`internal/agent/toolsys` | 见 §5、§6 |
| 7 | `go test -count=1 ./internal/web/...` | 0 | 4,275 ms | — | **绿**（MySQL/Ollama 集成用例自跳过） |
| 8 | 整门（11:00，脚本默认 full 口径，HEAD `7e9528a` + 36 项 dirty） | 1 | 191 s | `gofmt`: `internal\web\hermes_guard.go` | build 绿 25.8 s、vet 绿 15.8 s、**test 8/8 包全绿** 149.7 s、gofmt 红（施工方新文件，差 1 个空格对齐） |

> **`go vet` 偶发假失败（实测一次）**：10:55 一次整门运行里，`go build ./...` 刚 PASS，
> 紧接着的 `go vet ./...` 却报 `go.exe : no Go files in E:\blog-my` 并退出 1；
> 随后单独连跑 3 次 `go vet ./...` **全部 exit=0**，`go list ./...` 正常返回 13 个包。
> 判定为并发改写工作树时的**瞬时加载失败**，非代码问题。脚本已加：命中该特征时**自动重试一次**
> 并在汇总里记录 `retried once after transient "no Go files" load error`，避免门禁出现幽灵红。

**关于 `gofmt -l .`**：不要用它做门禁。实测它会输出**上万行**路径，全部位于被 .gitignore
忽略的 `build\gomodcache\`（第三方模块源码）与 `build\gotmp\`，不是本项目源码。
门禁口径固定为 `gofmt -l cmd internal`——仓库跟踪的 193 个 `.go` 文件全部在 `cmd/`、`internal/` 下
（`git ls-files '*.go'` 实测）。

---

## 4. 环境性失败 / 自跳过（不计入基线红）

### 4.1 Go 缓存被沙箱拒绝
默认缓存目录在工作区外，受限令牌不可写 → `go vet`/`go test` 直接以
`Access is denied` 退出。这不是代码缺陷。改用工作区内 `GOCACHE` 后同一命令可正常跑完。

### 4.2 需要 MySQL 的集成用例（全部自跳过，实测输出）
`chat_paging_test.go:18`、`soul_test.go:32`、`soul_evolve_test.go:31`、
`agent_http_it_test.go:136/235/267`、`agent_perm_http_it_test.go:144`、
`agent_autonomy_test.go:49`、`agent_profile_test.go:53`、`verify_http_it_test.go:164/209/241/323/470`
—— 均要求 `AGENT_IT=1` / `AGENT_PROFILE_IT=1` / `VERIFY_*` 才运行。
**`internal/web` 之所以是绿，是因为这些集成用例跳过了，不是它们通过了。**

### 4.3 需要 Ollama 的 live 用例（自跳过）
`agent_autonomy_live_test.go:20`：`set AGENT_LIVE_MODEL to an installed Ollama model`。

### 4.4 符号链接权限
`verify_observation_test.go:40` 实测：`os.Symlink` → `A required privilege is not held by the client`，
测试自动回退到 junction。这是被测环境事实（未开启开发者模式/无 SeCreateSymbolicLinkPrivilege），非代码缺陷。

### 4.5 受限 shell 无法写仓库内部分目录（实测）
`E:\blog-my\docs\`、`scripts\`、`build\`、`artifacts\` 用 shell 写入被拒（`Access is denied`），
仓库根目录与新建 `.tmp-*` 目录可写。文件写入工具可以正常写 `docs/verify/`。
本报告与脚本均由文件写入工具落盘；`scripts/verify-hermes.ps1` 只需被**执行**（读权限），不受影响。

---

## 5. 基线红：`internal/agent/fs` 2 个用例（根因已定位）

| 用例 | 位置 |
| --- | --- |
| `TestSandboxRejectsSymlinkEscape` | `internal/agent/fs/sandbox_test.go:162` |
| `TestVerifyJunctionInsideRootAllowed` | `internal/agent/fs/verify_sandbox_test.go:158` |

现象（原文）：`指向根内的链接应当可读: [sandbox: 拒绝访问根目录之外的路径]`，
`指向根内的链接被误拒：[sandbox: 拒绝访问根目录之外的路径]`。
即**根目录内**的 junction 被误判成越界。

复现性：4 次独立运行稳定失败（0.02–0.06 s），把 `TMP`/`TEMP` 指到工作区内仍失败，
`go test -count=1` 关缓存仍失败 → 非偶发、非临时目录位置问题。

根因（实测，探针：`.tmp-verify/probe/main.go`，逻辑逐行照抄 `internal/agent/fs/sandbox.go`）：

1. `os.Readlink(junction)` 返回 `\\?\Global\C:\...\root\real`（NT 命名空间前缀）。
2. `normalizeNTPath` 用 `strings.ToUpper(p)` 做比较，但前缀表写的是混合大小写
   `\\?\Global\`，`\\?\GLOBAL\...` 与 `\\?\Global\` **不匹配**。
3. 于是落到短前缀 `\\?\`，只剥掉 `\\?\`，剩下 `Global\C:\...`。
4. 该串没有卷名（`filepath.VolumeName` = ``），逐段重建后得到
   `\Global\C:Users\...\root\real\ok.txt`。
5. `within(root, 解析结果)` = `false` → 返回「拒绝访问根目录之外的路径」。

**HEAD 对照**：用 `go test -overlay` 把 `internal/agent/fs/sandbox.go` 换成 HEAD 版本
（不修改工作树），同样这 2 个用例失败；overlay 有效性用「换成故意写坏的 sandbox.go → 编译报错」
的对照组验证过。结论：**该红在 HEAD 就存在**；工作区里未提交的 `normalizeNTPath` 改动是
为了修它，但大小写比较写错，没修好（`sandbox.go` mtime = 2026-10-20 23:09，非本轮施工产生）。

**后续状态（实测）**：10:45:52 **Lead** 修复了 `internal/agent/fs/sandbox.go`——
`normalizeNTPath` 改为 `strings.HasPrefix(upper, strings.ToUpper(prefix))`（就是上面定位的大小写比较问题），
并按「先剥 NT 前缀、再 `filepath.Clean`」的顺序处理（当前代码：函数入口即 `normalizeNTPath`，第 194 行）。
11:02 / 11:07 两次复测：`TestSandboxRejectsSymlinkEscape`、`TestVerifyJunctionInsideRootAllowed` **均 PASS**
（`go test -run ... -v` exit=0），11:00 整门 `go test` 8/8 包绿。

> **定性**：这是**改造前就存在的 WIP bug**（HEAD 版本同样红，见上 overlay 对照），
> 已由 Lead 修复。**不计入本轮 Hermes 改造的回归**。
> 本文件保留「改造前是红的」这一事实记录，作为后续「没弄坏」的对照基线。

---

## 6. 用例统计（实测）

### 6.1 基线口径（改造前既有 6 个包，排除施工中的 evolve / toolsys）

`go test -count=1 -json`，10:52 采集：

| 指标 | 数值 |
| --- | --- |
| 唯一用例（含子测试） | **594** |
| PASS | **574** |
| FAIL | **2**（均在 `internal/agent/fs`，见 §5） |
| SKIP | 18 |

```
blog-my/internal/agent          pass
blog-my/internal/agent/fs       fail   <- 基线红
blog-my/internal/agent/skill    pass
blog-my/internal/agent/toolkit  pass
blog-my/internal/llm            pass
blog-my/internal/web            pass
```

### 6.2 全量口径（10:42 采集，**包含**并发施工中的新包）

| 指标 | 数值 |
| --- | --- |
| 唯一用例（含子测试） | 685 |
| PASS | 662 |
| FAIL | 5 |
| SKIP | 18 |

包级结果：

```
blog-my/internal/agent          pass      blog-my/internal/agent/fs       fail
blog-my/internal/agent/evolve   pass(无测试文件)   blog-my/internal/agent/skill    pass
blog-my/internal/agent/toolkit  pass      blog-my/internal/agent/toolsys  fail
blog-my/internal/llm            pass      blog-my/internal/web            pass
```

失败明细：
- `internal/agent/fs`：2 个（§5，**基线红**）
- `internal/agent/toolsys`：`TestCoerceRepairsArguments` + 2 个子测试
  （`decorators_test.go:167/191`）——该包 10:30 才出现、未跟踪，属并发施工，**不计入基线**；
  10:48 复跑时已由施工方修复。

**基线结论：改造前既有包的用例失败数 = 2，全部位于 `internal/agent/fs`（junction 误判）。**
11:07 复测：该 2 个用例已随 §5 的 Lead 修复转绿 → 同一口径现在应为 **FAIL=0**（用例总数不变）。

---

## 7. 时间线：为什么基线只能以 T0 为准

| 时刻 | 事件（实测） |
| --- | --- |
| 10:29:56 | `go build ./...` = **0**；此时 `evolve/`、`toolsys/` 尚不存在 |
| 10:30:04 起 | `internal/agent/{eino,loop,tool,tool_state}.go` 被改动 |
| 10:30–10:33 | 新目录 `internal/agent/evolve/`、`internal/agent/toolsys/` 逐个落盘（mtime 秒级变化） |
| 10:32:36 | `go vet ./...` 报 `evolve/candidate.go:193: st.lastErr undefined` |
| 10:34:12 | `go build ./...` 报 `toolsys/decorators.go` 4 个编译错误 |
| 10:36 | `go test` 中 `toolsys` = `[build failed]` |
| 10:42–10:48 | `toolsys` 已可编译、用例通过；`internal/web/hermes_guard.go` 出现且 gofmt 未通过 |
| 10:44:48 / 10:45:18 | `internal/agent/parallel.go`（新文件）与 `loop.go`、`tool_state.go` 的并行执行改动落盘（非本轮验证人员所为，见 §11） |
| 10:45:52 | **Lead** 修复 `internal/agent/fs/sandbox.go`（NT 前缀大小写比较 + Clean 顺序，见 §5） |
| 10:47 | 整门：`build` PASS、`vet` PASS、`gofmt` **FAIL**（`internal\web\hermes_guard.go`）、`go test` FAIL（fs 2 个） |
| 10:55 | 整门出现 1 次 `vet` 瞬时假失败（`no Go files in E:\blog-my`）；随后单跑 3 次全部绿 → 脚本已加重试 |
| 11:00 | 整门：`build` PASS、`vet` PASS、`go test` **8/8 包全绿**、`gofmt` FAIL（`hermes_guard.go` 差 1 个空格对齐） |
| 11:07 | `internal/agent/fs` 两个 junction 用例复测 PASS（基线红已由 Lead 消除，不算本轮回归） |

所以：**`go build`/`go vet` 的「绿」只对改造前既有包成立**；改造期间同一工作树 15 分钟内
变化多次，任何「全量绿」结论都必须带时间戳和 T0 对照才有意义。

---

## 8. 未验证

- `go test ./...` 全仓（`cmd/...` 等其余包）：**未运行**
- 需要真 MySQL 的集成套件（`AGENT_IT=1` 全部）：**未运行**
- 需要真 Ollama 的 live 用例：**未运行**
- `-race`：**未运行**
- Electron / 前端 / 打包（`electron/`、`templates/`、`packaging/`、`Dockerfile`）：**未验证**
- 并发施工中的 `internal/agent/evolve`、`internal/agent/toolsys`、`internal/web/hermes_guard.go` 的正确性：不在基线范围

---

## 9. 质量门脚本

`scripts/verify-hermes.ps1`

```powershell
# 全量口径（改造后验收用）
powershell -NoProfile -File scripts\verify-hermes.ps1

# 改造前口径（排除施工中的 evolve / toolsys）
powershell -NoProfile -File scripts\verify-hermes.ps1 -Scope baseline
```

行为：
- 依次跑 `go build ./...`、`go vet ./...`、`gofmt -l cmd internal`、指定包 `go test -count=1`；
- **不 fail-fast**，四步都跑完再汇总，一次看清全部问题；
- 任一步失败 → 退出码 **1**，并打印 **失败包名**（以及 `--- FAIL:` 用例名与 `file.go:行号` 摘要）；
- 自动把 `GOCACHE`/`GOTMPDIR` 指到工作区内可写目录（`.gocache-hermes` → `.verify-cache` → `.tmp-verify` 依次探测），
  避免 §4.1 的沙箱拒绝；
- 全程非交互：不读键盘、不弹窗、不改工作树（只读 git 状态）；
- 输出为纯 ASCII：Windows PowerShell 5.1 在无 BOM 时按 GBK 解码 `.ps1`，中文注释会导致
  `字符串缺少终止符` 解析失败（实测踩过），因此脚本内说明一律用英文，中文说明放在本文件。

实测（10:47）：`-Scope baseline` 与默认口径均按预期返回 **1**，失败包与失败用例打印正确。

---

## 10. 复现命令

```powershell
cd E:\blog-my
$env:GOCACHE      = 'E:\blog-my\.gocache-hermes'
$env:GOMODCACHE   = 'E:\blog-my\.gomodcache-hermes'
$env:GOTMPDIR     = 'E:\blog-my\.gotmp-hermes'
# 必须先设缓存，否则 go 命令会因沙箱 Access is denied 直接失败（见 §4.1）

go build ./...
go vet ./...
gofmt -l cmd internal
go test -count=1 ./internal/agent/... ./internal/llm/...
go test -count=1 ./internal/web/...
go test -count=1 -json ./internal/agent/... ./internal/llm/... ./internal/web/... > test-json.log
```

证据文件（未跟踪，供复核）：`.tmp-verify/test-json.log`（685 用例的 JSON 事件流）、
`.tmp-verify/sandbox_head.go` 与 `.tmp-verify/overlay-head.json`（HEAD 对照）、`.tmp-verify/probe/main.go`（junction/Readlink 探针）。

---

## 11. 写入清单（审计用：本轮验证人员只写了这些）

本轮验证人员（verifier）在本工作区**只写了下表这些文件**，**没有创建或修改任何 `internal/**` 文件**：

| 文件 | 用途 | 是否纳入版本控制 |
| --- | --- | --- |
| `docs/verify/hermes-baseline-2026-10-21.md` | 本报告 | 是（待提交） |
| `scripts/verify-hermes.ps1` | 质量门脚本 | 是（待提交） |
| `docs/verify/__probe.md` | 沙箱写盘探针残渣，**待删除** | 否（建议删） |
| `.tmp-verify/**`（约 9 个文件） | `test-json*.log` 用例事件流、HEAD overlay 对照、junction 探针、testtmp | 否（`.tmp*/` 已 gitignore） |

对 `internal/**` 的全部操作均为**只读**：`Get-Content`、`Select-String`、`git diff/show`、
`go build / vet / gofmt / test`（其中 `gofmt -l` 只列出文件、不改写）。

关于 `internal/agent/parallel.go`（10:44:48）与 `loop.go`、`tool_state.go`（10:45:18）的并行执行改动：
**不是本轮验证人员所为**。那两个时间点本会话的 shell 只在跑只读的 `go test -count=1 -json`；
且团队内另有 `tools-engineer`、`evolve-engineer`、`hermes-analyst` 三个 teammate 在同一工作区写 `internal/**`。

受限 shell 无法删除 `docs/` 下的文件：对 `docs/verify/__probe.md` 实测
`Move-Item` 与 `Remove-Item` 均返回 `对路径的访问被拒绝`，故该探针文件需由具备写权限的一方删除。
