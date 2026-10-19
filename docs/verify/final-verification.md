# task-28 终验：task-19~26 之后的独立复验（verifier）

复验人：verifier  时间：2026-10-11 19:3x~19:5x（Asia/Shanghai）  实例端口：全部用 `APP_PORT=0` 由系统分配
（实测分配过 3119/3666/4223/9588/1370 等，不与任何人抢固定端口）

## 0. 结论摘要

| 任务 | 判定 | 一句话 |
|---|---|---|
| task-19 PyInstaller sidecar + electron-builder | PASS（部分） | sidecar 存在、自检 87/0、electron-builder 配了 extraResources(bin+templates) + nsis；**没打真实安装包**（见 §7） |
| task-20 §3.1 空 content=E_MODEL_EMPTY + 放大 max_tokens | PASS | 空 content 已在 task-18 验过（502 + code=E_MODEL_EMPTY）；放大部分见 task-24 |
| task-21 UTF-8 加固 | PASS | task-18 的 C14（含 emoji 向量）仍全过；sidecar 产物本身也是 UTF-8 输出 |
| task-22 前端历史列表 | PASS | 真浏览器里 `#cua-history-list` 渲染出 1 条，计数「1 条」 |
| **task-23 打包态启动** | **FAIL → 已修复** | 原 FAIL：后端能起但壳解析不出 URL（READY 在 stderr，main.js 只扫 stdout）；**task-30 已修、task-31 独立复验 PASS**，见 task-31-reverify.md |
| task-24 max_tokens 下限 16384 | PASS | 我的桩实测 6 个向量全对（含「大值不被压回」对照） |
| task-25 type 走剪贴板 | PASS | 我自己的假设备 18 条判据全过（含 clear_first/submit 顺序、两条退化路径） |
| task-26 重建 sidecar | PASS | 直接跑 `bin/cua-sidecar.exe`，事件流里 `type_path=clipboard` |

门禁与自检：`go build/vet/test -count=1` 全 0、`gofmt -l .` 空；`--selfcheck` **python 87/0**、**sidecar 87/0**；
`cua/tests/run_tests.py` **91/0**。（84 -> 87 是 task-25 新增 3 条自检，不是回归。）

## 1. task-24 max_tokens 下限（Lead 重点 3）

**不采信单测**：自己起桩（`docs/verify/it-upstream-capture.mjs`，记录每次请求体），用固定 token 直连代理，
再看**上游真正收到**的 `max_tokens`。

| 发送 | 上游收到 | 判定 |
|---|---|---|
| 8 | 16384 | PASS |
| 1024 | 16384 | PASS |
| 16383 | 16384 | PASS |
| 16384 | 16384 | PASS |
| 20000 | **20000** | PASS |
| 不带该字段 | 16384 | PASS |

`20000` 与 `16384` 原样透传才是关键：**它是「下限」而不是「一律写死 16384」**。
同一组向量在**当前源码新构建**与**仓库现成产物 `bin/blog-my.exe`（19:30 构建）**上都 6/6 通过。
证据：docs/verify/tmp/maxtokens-02.log（源码）、docs/verify/tmp/maxtokens-04.log（产物）。

> 自纠（假红）：我第一轮探针用的是 **16:17 的旧二进制**（task-24 之前的），实测抬到 4096 —— 这是旧口径，不是缺陷。
> 重新构建 + 用 19:30 产物复测后全过。教训：**探针必须先确认被测产物的时间戳**。

## 2. task-25 剪贴板主路径 / 退化 / clear_first（Lead 重点 4）

我自己的假设备 + 自己的断言（`docs/verify/tmp/clipboard-probe.py`），**18 条判据全过**：

| 场景 | 期望 | 实测 |
|---|---|---|
| ASCII 文本 | 走剪贴板 | `type_path=clipboard`，ops=`[set_clipboard,key_down,press(v),key_up]`，无逐字符 write |
| 中文 + emoji | 走剪贴板 | `type_path=clipboard` |
| 剪贴板「半可用」（写成功拿不到旧值） | 仍走剪贴板 | `type_path=clipboard`（这条实现者的自检没覆盖） |
| 剪贴板不可用 + ASCII | 退化整段 write | `type_path=write`，ops=`[write]` |
| 剪贴板不可用 + 中文 | 退化逐字符并记录跳过 | `type_path=char_by_char`，`type_skipped_chars=2` |
| `clear_first=True` | 先 Ctrl+A 再输入 | ops 顺序 `key_down(ctrl)→press(a)→key_up(ctrl)→set_clipboard→key_down(ctrl)→press(v)→key_up(ctrl)` |
| `submit=True` | 回车必须在最后 | 最后一步 = `press(enter)` |
| 配置关掉剪贴板开关 | 回到旧路径 | ASCII→`write`；中文→`char_by_char` |
| 空文本 | 不碰输入通道 | `type_path=empty`，ops 为空 |

**task-26（sidecar 是否含此修复）**：直接跑打包产物 `bin/cua-sidecar.exe --mock --events stdout`，
22 条事件、`run_end ok=true`，其中 `action_result` 的 `type_path=clipboard` —— 产物里确实是剪贴板路径。
证据：docs/verify/tmp/clipboard-01.log、docs/verify/tmp/sidecar-mock-events.log。

## 3. task-22 前端历史列表

在真浏览器（Playwright/Chromium）里展开 `#cua-history`、点刷新，`#cua-history-list` 渲染出 **1 条**，
计数元素显示「1 条」；整个前端探针 PASS=14 FAIL=0 SKIP=0。证据：docs/verify/tmp/frontend-14.log。

## 4. task-23 打包态启动 —— **FAIL**（本轮唯一实质未过项）

> **后续（2026-10-11 20:3x）**：cua-go 在 task-30 新增 `electron/ready.js`（两条流接进同一探测器 + 按行匹配）。
> 我在 task-31 独立复验 **PASS**：两方向均可解析、穷举 47 个切分点无残缺 URL、突变对照能区分、
> 真实打包应用实测 `loading http://127.0.0.1:1606` + `loadURL resolved` + `window shown 1282x802`，该地址 health=200。
> 详见 docs/verify/task-31-reverify.md；未覆盖面只剩「Setup 安装包未重出」。

**判定**：后端在打包布局下**能起**，但 **Electron 壳永远解析不出后端 URL**，即「安装后 app 起后端」这条验收没达成。

**根因（两行代码对不上）**：

- `main.go:50`：`log.Printf("READY http://%s", ln.Addr().String())` —— Go 的 `log` 默认写 **stderr**。
- `electron/main.js:138-150`：`readyRe` 只挂在 `serverProc.stdout.on("data", onOut)`；
  stderr 回调（第 151 行）只做转发，**不做同样的匹配**。

**复现（我照打包布局搭的，可照抄）**：把 `bin/blog-my.exe` 与 `templates/` 放进一个 `resources` 样目录，
`cwd=` 该目录，配置以**环境变量**传入（安装态就是这么传的），`APP_HOST=127.0.0.1 APP_PORT=0`：

- 后端进程存活 ✓，`READY` 行指向的 `/api/health` → **200** ✓（本次端口 3666）
- 把 main.js 的同一个正则 `READY\s+(http://\S+)` 跑在 **stdout** → **无命中**（壳拿不到 URL）
- 同一个正则跑在 **stderr** → 命中 `http://127.0.0.1:3666`

**影响**：`waitForServer` 的 promise 不 resolve，窗口拿不到 `serverUrl`；用户看到的是装好了却用不了。

**归属**：`git log -S readyRe -- electron/main.js` = `babdb7c`（Day 9 基线），main.go 的 READY 打印同样自 Day 9 起就是 `log.Printf`；
即这是**既有错配**，task-23 重写了路径与配置来源（userData/.env、resourcesPath、APP_PORT=0）但没碰这条，
所以 task-23 的验收标准仍未达成。**没提交过任何修复**——修法在实现者那边，建议二选一或都做：

1. `main.go` 把 READY 行打到 **stdout**（其余日志留在 stderr）；
2. `main.js` 在 stderr 回调里也用同一个 `readyRe` 匹配（防御性，两流都认）。

并补一条自动化回归：起一次打包布局 → 断言**壳那侧**能解析出 URL（而不是只看后端 exit code）。
证据：docs/verify/tmp/packaged-stdout.log（无 READY）、docs/verify/tmp/packaged-stderr.log（有 READY）。

## 5. task-19 / 20 / 21 覆盖面

- **task-19**：`package.json > build` 里 `extraResources` 把 `bin/`、`templates/` 放到 `resources/`（与 main.js 的 `process.resourcesPath` 一致），
  `win.target=nsis`；`bin/cua-sidecar.exe` 存在且自检 87/0。**没打真实安装包**（见 §6）。
- **task-20**：空 content → 502 + `code=E_MODEL_EMPTY` 在 task-18 已独立验过（正反对照），本轮未回归。
- **task-21**：C14 编码判据（含 emoji）全过；另外 sidecar 产物的 stdout 是 **UTF-8**，与 Go 侧按 UTF-8 读子进程 stdout 一致。

## 6. 仍未覆盖面（明确列出，别当已验）

1. **真实安装包**：没跑 electron-builder、没装 nsis 包、没双击启动。所以「安装后确实能跑」我只验到
   「打包布局 + 真实 Go 产物能起 + READY 流错配」这三段，**没有端到端装一次**。
2. **Electron 本体没启动**（会开窗口，属于打扰用户桌面）：所以「壳拿到 URL 之后正确加载页面」这段没验；
   而 §4 的 FAIL 是静态+动态两路证据（正则只挂 stdout + 实测 READY 只在 stderr），不依赖启动 Electron。
3. **真实鼠标键盘的 CUA 运行一律没做**（安全红线）。Lead 给的真实结果（28 步 done、`E:\虚拟C盘\cua-demo.txt` 14 字节、
   6 次 `type_path=clipboard`）**我没有复跑、也不作为我的证据**；我这边的等价证据是：剪贴板路径的行为判据（§2）
   与 sidecar 产物里的 `type_path=clipboard`（§2 末）。
4. **sidecar 在无 Python 机器上的独立性**没验（本机有 Python 3.13）；只验到它自带解释器、自检 87/0、能跑出事件流。
5. **task-24 只验了我发的 6 个取值**；真实任务里子进程默认发 1024，属已覆盖形态，但我没有跑完整真实 run 复看。
6. **前端历史列表**只验了「有 1 条 + 计数」；最近 50 条上限、点条目回放、并发分页未逐一验。
7. `--selfcheck` 的 87 条我**没有逐条复算**（那是实现者的用例集），我独立验的是本轮 Lead 点名的行为面。

## 7. 复跑命令

```powershell
# 门禁 + 自检（注意 sidecar 输出是 UTF-8，用 cmd /c 抓原始字节，别用 PowerShell 的 >）
go build ./... ; go vet ./... ; go test -count=1 ./... ; gofmt -l .
python -X utf8 -m cua.main --selfcheck
cmd /c "bin\cua-sidecar.exe --selfcheck > docs\verify\tmp\sidecar-selfcheck.log 2>&1"

# task-24：必须自备桩，且先确认被测产物是新的
node docs/verify/it-upstream-capture.mjs --port 8399 --mode actions --out docs/verify/tmp/mt-capture.json
# 起实例：APP_PORT=0 CUA_LLM_TOKEN=verify-fixed-token-2026 CUA_LLM_UPSTREAM=http://127.0.0.1:8399/v1
# 端口要读 **stderr** 里的 READY 行（这也是 task-23 的缺陷本身）
node docs/verify/tmp/maxtokens-probe.mjs http://127.0.0.1:<port> docs/verify/tmp/mt-capture.json verify-fixed-token-2026

# task-25：离线行为判据（不需要实例）
python -X utf8 docs/verify/tmp/clipboard-probe.py

# task-22：真浏览器（需要实例，--mock 即可）
node docs/verify/frontend-probe.mjs --base http://127.0.0.1:<port> --start

# task-23：打包布局模拟 + READY 流验证
#   把 bin/、templates/、.env 复制到一个 resources 样目录，cwd 设该目录，APP_PORT=0，
#   然后把 main.js 的正则分别跑在 stdout / stderr 两份日志上
```

## 8. 声明

- 本轮只改 `docs/verify/**`（探针与报告），**没有修改任何实现代码，也没有 git commit**。
- 全绿项都配了负向对照或反例（task-24 的 20000 对照、task-25 的两条退化路径、task-23 的双流对照）。
- 两个「假红」我自己发现并更正（旧二进制导致 max_tokens 误判；探针 capture 路径写错导致「桩没收到」），过程记在 §1 与本文档。
- 唯一未过项 = **task-23**（READY 流错配），已按「不放过」原则如实报 FAIL，并给出复现与建议修法。
