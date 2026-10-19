# task-18 集成层独立复验（verifier）

复验对象：docs/cua/06-integration-contract.md v1（§1 I1 事件流 / §2 I2 REST+SSE / §3 I3 模型代理 / §5 I5 打包回退 / §7 门禁）
复验人：verifier（独立于实现者）  时间：2026-10-11 16:3x（Asia/Shanghai）
复验方式：**只走黑盒**——自己起实例、自己的桩、自己的负向对照；不采信实现者自述，不读实现者的测试结论当证据。

## 0. 结论摘要

本轮 task-18 范围内 **未发现未修复的 FAIL**。契约 §1/§2/§3/§5/§7 判定全部 PASS，
Lead 点名的 5 个重点（C11/C13 自建桩、stop 收进程树、frame_image 上限 10、§3.4 provider + ollama、未验证清单）逐条有实测证据。

| 口径 | 实测 | 证据 |
|---|---|---|
| `--selfcheck` | **84 通过 / 0 失败** | docs/verify/tmp/selfcheck-84.log |
| `cua/tests/run_tests.py` | 91 / 0 | 见 integration-baseline.log [5] |
| 全量集成探测（normal 上游 + 固定 token） | **PASS=39 FAIL=0 WARN=0 SKIP=0** | docs/verify/tmp/integration-probe-live-08.log |
| §7 门禁 | build=0 vet=0 test=0 gofmt=干净 | integration-baseline.log [5] |

**两次口径变更都不是回归**：`--selfcheck` 81 → 83 → 84 均为新增自检项（详见 integration-baseline.log 文首两段）。

## 1. 逐项判定

| 契约条目 | 判定 | 实测要点 | 证据 |
|---|---|---|---|
| §1 I1 JSONL 一行一对象、事件种类齐全 | PASS | E2-jsonl / E2-kinds / E2-span / E2-frame-image 全过；`--events-images` 单独给被拒（E6 exit=2）；默认行为未变（E7） | docs/verify/tmp/events-default-now.log |
| §1 frame_image 字段（media_type/长边 640/可解 base64） | PASS | 5 条 frame_image 全部 image/jpeg、长边 640、base64 可解、且出现在 frame 之后 | 同上 |
| §1 **每 run 最多留最近 10 条 frame_image** | PASS | 见 §3 专项（实时 16 条不被裁、快照 10 条、非图片事件全留、留的正是最近 10 条） | docs/verify/tmp/memcap-02.log |
| §2 I2 status 永远 200 + 字段齐全 | PASS | available/env(screen 2560x1600)/models/notes/python/run_id/running 齐 | integration-probe-live-08.log（C1/C12） |
| §2 runs 参数校验 400 | PASS | task>2000、model 不在 models、max_steps/max_seconds 越界全部 400 | 同上（C3a–i） |
| §2 单实例 409 互斥 | PASS | 并发第二个 run 得 409 `{"error":"已有任务在运行"}` | 同上（C4a/C4b） |
| §2 201 返回 32 位 hex run_id | PASS | 32 hex，model/task 回显一致 | 同上（C5） |
| §2 SSE 首 start 末 exit、未知 run 404、finished 可重放 | PASS | 全过；重放的固定首末与实时一致 | 同上（C6a–f / C7） |
| §2 history 最近 50 条 | PASS | 列表含刚跑完的 run 与最终 status/code | 同上（C8a/8b） |
| §2 stop 幂等 | PASS | 第二次 stop 返回 200 `{"stopped":false}` | 同上（C9） |
| §3.3 固定 token 缝 | PASS | 设 CUA_LLM_TOKEN 后：正确 token 通、错 token 401 | integration-probe-live-08.log（C10/C11） |
| §3 代理解析图片（多模态透传） | PASS | 已知载荷直连代理：base64 逐字节相同、无 stream、parts=[text,image_url]、文字 marker 保留 | 同上（C11a–e） |
| §3 上游空 content → 502 E_MODEL_EMPTY | PASS | 空上游：502 且 run 记录 `code=E_MODEL_EMPTY`；正常上游：200 + 非空（正反对照同测） | docs/verify/tmp/integration-probe-live-06.log |
| §3.4 llm/provider.go（Content 与 MultiContent 不再并存） | PASS | 见 §4 专项：openai 后端多模态请求真的发出且 body 里 content 是**数组** | docs/verify/tmp/provider-probe-01.log |
| §5 I5 优先 sidecar、回落 python -m cua.main | PASS | 回落分支：早期无 sidecar 时 notes 明确写回落；优先分支：bin/cua-sidecar.exe 存在后实测 run 期间**真的起了 cua-sidecar 进程**、notes 为空 | 见 §5 |
| §7 门禁 | PASS | go build/vet/test -count=1 全 0、gofmt -l 干净 | integration-baseline.log [5] |
| `#/cua` 前端（F1/F2/F3） | PASS | 13 项全过，含真实 UI dry-run 渲染（此前 #view-cua 嵌套 bug 已修复后复测） | docs/verify/frontend-probe.mjs |

## 2. C11 / C13：用我自己的桩独立复验（Lead 重点 1）

两项都不看实现者的测试，而是自己写桩、自己造正反例：

- **C13（空回复）正反对照**：同一个 app 分别接 `--mode normal`（非空）与 `--mode empty`（空），
  得到 200+非空 与 502+`E_MODEL_EMPTY` 两种结果。**先有反例再谈正例**：若代理对空回复也回 200，C13 就是 FAIL。
- **C11 图片透传**：run 期间上游桩确实收到带图载荷（C11a，来自本次 run 的真实调用）；
  再用**已知 1x1 JPEG** 直连代理（C11b–e），断言上游收到的 base64 与发送值**逐字节相同**、无 `stream` 字段、
  parts 顺序为 `["text","image_url"]`、文字 marker 未被吞。

> 自纠记录：C11b 我第一版探针把「子进程发给模型的图」与「前端 frame_image」当成同一张，得到假 FAIL。
> 二者是刻意不同的两个产物（前端是 640 长边 JPEG，模型输入是原始 PNG）。改成已知载荷直连后为真 PASS。

## 3. stop 是否收掉整棵进程树（Lead 重点 2）

**方法**：把子进程换成假程序（自己再起一个孙进程，各自落 PID 后长睡 600s），再 `POST /stop`，
前后各查一次两个 PID。只要有一个残留，重启 app 就会不断累积孤儿进程。

| 步骤 | grandchild | direct child |
|---|---|---|
| stop 前 | pid 49636 **alive=true** | pid 59348 **alive=true** |
| stop 后 | **alive=false** | **alive=false** |

`PASS`，且事后 `Get-Process fakespawn / verify-cua` 计数均为 **0**（无残留）。
证据：docs/verify/tmp/treekill-02.log（探针：docs/verify/tmp/treekill-probe.mjs，假子进程源码 docs/verify/tmp/fakespawn.go.txt）。

**一次失败与更正（必须记下）**：我先用 `CUA_PYTHON` 做同一个实验，第二次重跑却 FAIL —— 原因是本轮期间
cua-go 落盘了 `bin/cua-sidecar.exe`（16:31），按契约 §5 **sidecar 优先**，`CUA_PYTHON` 缝被绕过，
假程序根本没被启动（日志 treekill-01.log 里两个 PID 都是 null）。这是我探针的前置假设失效，**不是产品缺陷**；
改用 cua-go 新给的 `CUA_SIDECAR` 缝后复现并通过（treekill-02.log）。
这条同时说明：**进程树这一段我验的是当前 build 的真实路径**。

## 4. frame_image 内存上限 = 最近 10 条（Lead 重点 3）

先用 `--mock` 跑，但 mock 场景 5 步就结束（不足 10 条，测不出上限）——于是写了个返回**合法动作 JSON** 的上游桩
（`protocol:"cua/1"` + `action.ms>=100`，否则子进程报 E_SCHEMA 只走 1 步），把 run 拉到 15 步：

| 观察点 | 实测 |
|---|---|
| 实时 SSE 里的 frame_image | **16 条（>10，广播不被裁剪）** |
| 事后重放（已存快照） | **10 条** |
| 非图片事件 | 实时 82 = 重放 82（**全留**） |
| 保留的是哪 10 条 | 实时 [4..15] → 重放 **[6..15]**，即**最近 10 条** |

证据：docs/verify/tmp/memcap-02.log。该 run 以 `E_STALL`（画面连续 5 步无变化）自然结束，与上限无关。

## 5. §3.4 provider 修复 + ollama（RAG）未受影响（Lead 重点 4）

自己写 Go 探针直接调 `llm` 导出面（`docs/verify/tmp/provider-probe.go`，`//go:build ignore` 不进产物）：

| 检查 | 实测 | 说明 |
|---|---|---|
| P1 ollama 文本路径 | PASS 回复="收到" | **RAG 依赖的本地文本路径未变** |
| P2 ollama 图片路径 | PASS 回复="这张图是黑色的。" | 真 1x1 JPEG（160 字节）走多模态，不报错 |
| P3 openai 后端多模态（指向我的桩） | PASS 桩回复非空 | 桩侧 body：`content_types=[["text","image_url"]]`，顶层 `content` 是**数组** |

P3 正是 §3.4 的判据：修复前 `Content` 与 `MultiContent` 并存会让 go-openai 的 `MarshalJSON` 直接报错（表现为 502），
现在同一路径真的发出去了并拿到回复。

**自纠记录**：P2 第一版我用手写字节充当 JPEG，服务端回 400，一度看起来像真缺陷；换成可解码的真图后 PASS。
假绿要打假，**假红同样要打假**——否则会误报产品。

## 6. §5 sidecar 优先 / 回落（本轮的时点变化）

| 分支 | 实测 |
|---|---|
| 回落 `python -m cua.main` | 本轮早段 bin/ 下还没有 sidecar，status.notes 明确写"未找到 sidecar，回落 python -m cua.main" |
| 优先 `bin/cua-sidecar.exe` | 16:31 sidecar 落盘后：run 期间**实测到 cua-sidecar 进程**（sidecar_seen_max=1），status.notes 已为空 |

停止后 `leftover_sidecar=0 leftover_fakespawn=0 leftover_app=0`。

## 7. 负向对照（没有它，全绿不算证据）

| 对照 | 规模 | 结果 | 证据 |
|---|---|---|---|
| 集成层变异桩（SSE 12 判据 × 16 变异） | 16 | **16/16 被抓住**，干净桩 33/0（探针不乱叫） | docs/verify/tmp/integration-negative-control-03.log |
| 事件流变异 producer（E2/E8 判据 × 11 变异） | 11 | **11/11 被抓住** | docs/verify/tmp/events-negative-control-02.log |
| CUA 套件自带变异自检 | 11 | 11/11 | integration-baseline.log [5] |
| §3.1 空回复判据自检 | 11+2 | 11 条判据全过、2 条变异被抓 | cua/tests/it_model_empty.py |

与本轮新增向量直接相关的两条：

- `cua_task_mojibake`（SSE 里把 task 写成合法 JSON 的 U+FFFD 串）：**只有 C14b/C14c 能抓**，
  因为 Go 的 `encoding/json` 会把非法 UTF-8 静默替换成 U+FFFD，只看"能否解析出 JSON"永远抓不到。
- `cua_task_gbk_bytes`（SSE 里直接落 GBK 字节）：C14a 抓字节、C14b/C14c 抓语义。

## 8. C14 编码（含 emoji 向量）

中文能过 ≠ emoji 能过：emoji 在 GBK 里编不出来，修复前的表现是**子进程崩溃**（stdout 只剩一条 run_end）而不是乱码。
本轮两个方向都补上了，细节见 docs/verify/c14-encoding-record.md §8：

- 修复后 emoji 向量（15 码点）：严格 UTF-8、U+FFFD=0、task 逐码点相同；经 Go/SSE 端到端 C14b 为 **26 码点全等**（21304 字节严格解码）。
- `task_mojibake`、`gbk_strict_crash` 两个变异在 emoji 向量下**都被抓住**。
- **归因更正**：argv 走宽字符 API（GetCommandLineW），与代码页无关，**乱码完全由 stdout 的 GBK 落字节解释**；
  早期把 argv 也算作来源的说法是错的，已在 c14-encoding-record.md §9 更正。

## 9. 我没能验证的部分（明确列出，别把它们当已验）

1. **修复前的原始代码我无法亲手复跑**：修复已落地，我只能用"受控同型故障"（GBK 落字节 / strict 编码崩溃）复现，
   无法在真实代码上重现 0xd0@53 / 87 个 U+FFFD 那一组数字。早期数字引用的是 Lead/cua-py 的实测（已标注归属）。
2. **子进程真正发给模型的图片字节，端到端无法比对**：没有任何事件暴露模型输入图（前端 frame_image 是另一张图）。
   我验的是 ①run 的调用确实带图（桩侧看到）②代理这一段用已知载荷逐字节透传。
   "子进程字节 == 上游收到的字节"这条链路**没有被任何判据覆盖**。
3. **只验了本地链路**：上游一律是我自己的桩 / 本机 ollama，**没有对真实云端模型（deepseek-flash）做过端到端**。
   所以"真实云端多模态能不能通"不在我的证据范围内。
4. **没有做过任何真实 CUA 操作**（安全红线：会接管鼠标键盘）。所有 run 都是 dry-run/mock 语义，
   因此依赖真实输入的那部分行为（真实点击/输入的守卫、权限弹窗等）我没验，属离线用例覆盖范围。
5. **上限 10 只验到 16 条那一次**：run 因 E_STALL 在第 15 步结束，没有跑到 200 步量级；
   "长跑时上限行为一致"是从代码（与步数无关的裁剪）推出的，不是实测的。
6. **前端只在 Chromium（playwright-core + 固定 chrome 路径）里验过**，没在 Electron 壳里点过 `#/cua`。
7. **mock 帧是固定合成图**，所以"截图预算分档（真实分辨率下的长边裁剪）"在集成层验不了；那部分只由自检覆盖。

## 10. 复跑方式（可照抄）

```powershell
# 0) 先探端口：8300 被占就换（端口纪律见 integration-baseline.log）
go build -o $env:TEMP\verify-cua.exe .

# 1) 自检 / 套件 / 变异
python -X utf8 -m cua.main --selfcheck
python -X utf8 cua/tests/run_tests.py
python -X utf8 cua/tests/ct_negative_control.py

# 2) 事件流（含 emoji 向量）
$env:IT_TASK="中文任务🙂测试：含 emoji"
python -X utf8 docs/verify/events-jsonl-check.py --extra-args "--max-steps 1" --no-events-images
node docs/verify/events-negative-control.mjs 2>$null; python -X utf8 docs/verify/events-negative-control.py

# 3) 集成探测（自己的桩 + 固定 token）
node docs/verify/it-upstream-capture.mjs --port 8399 --mode normal --out docs/verify/tmp/upstream-capture-final.json
# 起 app：APP_PORT=8317 CUA_LLM_TOKEN=... CUA_LLM_UPSTREAM=http://127.0.0.1:8399/v1
node docs/verify/integration-probe.mjs --base http://127.0.0.1:8317 --proxy-token verify-fixed-token-2026
node docs/verify/integration-negative-control.mjs

# 4) 进程树 / 内存上限 / provider
$env:CUA_SIDECAR="$env:TEMP\fakespawn\fakespawn.exe"   # 注意：sidecar 存在时会盖过 CUA_PYTHON
node docs/verify/tmp/treekill-probe.mjs http://127.0.0.1:8317
node docs/verify/tmp/memcap-probe.mjs http://127.0.0.1:8317
go run docs/verify/tmp/provider-probe.go                # 需要 STUB_URL 才跑 P3

# 5) 门禁
go build ./... ; go vet ./... ; go test -count=1 ./... ; gofmt -l .
```

## 11. 声明

- 本报告只写我实际测到的结果；**没有测到的一律进 §9**，不用推测填空。
- 所有 PASS 都配套负向对照（§7）；没有对照的绿不写进判定表。
- 复验期间我未修改任何实现代码（写范围仅 docs/verify/** 与 docs/verify/tmp/**），未做 git commit。
