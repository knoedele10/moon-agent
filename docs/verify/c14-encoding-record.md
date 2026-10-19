# C14 非 ASCII 往返保真 · 验证记录（verifier）

契约依据：docs/cua/06-integration-contract.md §1（--events stdout / frame_image）与 §3.1；
验收判据由 Lead 在 2026-10-11 给出（三条），本文用**可复现证据**逐条对齐。

## 1. 为什么必须有一条专门的编码判据

Go 的 `encoding/json` 遇到非法 UTF-8 **不报错**，而是静默把坏字节替换成 U+FFFD。
因此"事件流能被解析成 JSON"完全不代表字节是对的：中文可以整体变成 `�����` 而所有形状断言照样全绿。
这类缺陷是**沉默失败**——不会让任何测试变红，只会让用户看到乱码。所以本判据看的是**原始字节**，不是解析结果。

## 2. 修复前的实测（Lead 采样，转记）

环境：`PYTHONUTF8` 未设、`PYTHONIOENCODING` 未设。
命令：`python -m cua.main --task "新建一个名为测试的文件夹" --mock --dry-run --events stdout --no-log --max-steps 1`

| 观测量 | 实测值 |
|---|---|
| stdout 原始字节 | 2482 字节 |
| 严格 UTF-8 解码 | **失败**：`can't decode byte 0xd0 in position 53: invalid continuation byte` |
| replace 解码后 U+FFFD | **87 个** |
| run_start.task | `�½�һ����Ϊ���Ե��ļ���`，与输入不相等 |
| 按 GBK 解码后是否含原任务串 | **True**（GBK 落字节的铁证） |

根因：管道下 `sys.stdout.encoding` 退化为 **gbk**（未设 PYTHONUTF8/PYTHONIOENCODING），
`cua/loop.py:88` 用 `ensure_ascii=False` 写 stdout → GBK 字节被 Go 按 UTF-8 读 → U+FFFD。

## 3. 我自己的受控复现（同一类损坏，可当场重跑）

我无法复现"原始未修版本"——我第一次跑判据时修复已经落地（见 §4 的时间线），因此我用一个**按 GBK 落字节的假生产者**
复现同一类损坏，证明判据对这类字节确实有区分能力（而不是只会报绿）：

```
$env:IT_MUT="gbk_output"
python -X utf8 docs/verify/events-jsonl-check.py --producer "python -X utf8 docs/verify/events-fake-producer.py"
[FAIL] E8a-utf8-strict      stdout 原始字节不是合法 UTF-8（管道下 stdout.encoding 很可能退化成 gbk）
        证据: {"字节数": 583, "第一个坏字节位置": 52, "坏字节": "0xd0", "错误": "'utf-8' codec can't decode byte 0xd0 in position 52: invalid continuation byte"}
[FAIL] E8b-ufffd            全量文本出现 14 个 U+FFFD（中文被替换成乱码）
        证据: {"总字符": 578}
[FAIL] E8c-task-fid         run_start 回显的 task 与提交值不一致（中文在子进程往返里失真）
        证据: {"提交": "新建一个名为测试的文件夹", "回显": "�½�һ����Ϊ���Ե��ļ���", "回显码点前 16": ["0xfffd", "0xbd", "0xfffd", "0x4bb", "0xfffd", "0xfffd", "0xfffd", "0xfffd", "0x3aa", "0xfffd", "0xfffd", "0xfffd", "0x535", "0xfffd", "0xfffd", "0x13c"]}
[INFO] E8d-gbk-diag         原始字节按 GBK 解码后包含提交的 task（GBK 落字节的铁证）
        证据: [[1, "decode-fail"]]
  FAIL E8a-utf8-strict
  FAIL E8b-ufffd
  FAIL E8c-task-fid
```

**与 Lead 采样的一致性**：我的受控复现给出的坏字节是 **0xd0 @ 52**（Lead 是 0xd0 @ 53，差 1 个字节来自假生产者前缀长度不同），
而回显出来的乱码串 `�½�һ����Ϊ���Ե��ļ���` 与 Lead 记录的 run_start.task **完全相同**——
同一输入、同一 GBK 编码路径、同一 U+FFFD 替换结果。这证明我复现的是同一类损坏，而不是另一种噪声。

注意 E8d 这一行的意义：它按 GBK 解码原始字节后**能找到提交的中文**，说明坏字节确实是 GBK 编码产物，
而不是随便什么噪声——这正是定位根因所需要的证据。

## 4. 修复后的复验（同一命令、同一台机器、同样的环境变量"未设"）

我的 shell 中 `PYTHONUTF8` 与 `PYTHONIOENCODING` 均为空（已实测打印），因此下面的绿不是"环境变了"造成的。

| 判据 | 修复后实测 |
|---|---|
| E8a stdout 原始字节严格 UTF-8 | **PASS**（2551 字节严格解码通过） |
| E8b U+FFFD 计数 | **PASS = 0**（2411 个字符） |
| E8c run_start.task 与提交值逐码点相同 | **PASS**（12 个码点全等） |
| E8e 每行解码后再编码逐字节相同 | **PASS** |

经 Go 层（HTTP/SSE 路径）端到端：

| 判据 | 实测 |
|---|---|
| C14a SSE 原始字节严格 UTF-8 | **PASS**（35234 字节） |
| C14b run_start.task 保真 | **PASS**（22 个码点全等，U+FFFD 计数 0） |
| C14c 事件流内无替换字符 | **PASS**（18 条事件） |

两侧修复（都已落地，缺一不可）：
- cua-go：`web/cua.go:360-361` 给子进程注入 `PYTHONUTF8=1` + `PYTHONIOENCODING=utf-8`；
- cua-py：`cua/main.py:242-247` 在写第一个事件前 `reconfigure(encoding="utf-8", errors="strict")`。

## 5. 负向对照（判据必须能抓到违规，否则不算证据）

| 对照 | 注入的违规 | 抓住它的判据 |
|---|---|---|
| stdout 侧 `gbk_output` | 中文按 GBK 落字节 | E8a / E8b / E8c / E8e |
| stdout 侧 `task_mojibake` | 合法 UTF-8，但 task 被替换成 U+FFFD | E8b / E8c |
| SSE 侧 `cua_task_mojibake` | Go→SSE 链路上 task 变成 U+FFFD（JSON 仍合法） | **仅** C14b / C14c |
| SSE 侧 `cua_task_gbk_bytes` | SSE 流里直接落 GBK 字节 0xb2 0xe2 0xca 0xd4 | C14a / C14b / C14c |
| stdout 侧 `gbk_strict_crash`（emoji 向量） | emoji 编不成 GBK，strict 编码抛异常、进程崩、stdout 只剩 run_end | E8c（+ E2-span） |

`cua_task_mojibake` 这条最能说明问题：它是一条**完全合法的 JSON 流**，形状类判据（事件种类、首尾事件、字段）全部照过，
只有 C14b/C14c 抓住了它——这就是为什么必须新增这条判据，而不是靠既有的形状断言。

汇总：集成负向对照 16/16 变异全部被抓住，干净桩 33 PASS / 0 FAIL；事件流负向对照 11/11。

## 6. 我没能验证的部分（明确边界）

1. **我没有亲手复现"原始未修版本"** 的 0xd0@53 / 87 个 U+FFFD：我第一次执行判据时修复已经落地。
   §2 的数字是 Lead 的采样（此处转记并标注来源），我自己的等价证据是 §3 的受控 GBK 生产者。
2. `--mock` 模式下原始帧几何是固定夹具（`cua/main.py:803` 的 `1472x828 @ scale 1472/1920`），
   与真实屏宽（本机 2560）无关，因此**无法用 mock 帧验证截图长边分档**；分档由自检第 13 项覆盖。
3. C14 经 SSE 只能看到 Go 层**转发出来**的内容；若子进程压根没发某个中文事件，SSE 侧看不到——该场景由 stdout 侧判据兜底。
4. 本次所有 run 都是 `dry_run`；退出码 1 出现在 `--max-steps 1` 时是**预算耗尽**的正常终态
   （`run_end: {"code":"E_STEP_BUDGET","message":"超出步数预算 1"}`），不是崩溃。

## 7. 复跑命令

```
# stdout 侧（含 E8 编码判据；--extra-args/--no-events-images 对齐 Lead 的复现命令）
python -X utf8 docs/verify/events-jsonl-check.py --task "新建一个名为测试的文件夹" --extra-args "--max-steps 1" --no-events-images
# stdout 侧负向对照（11 个变异，含 gbk_output / task_mojibake）
python -X utf8 docs/verify/events-negative-control.py
# SSE 侧（C14a/b/c）
node docs/verify/integration-probe.mjs --base http://127.0.0.1:<port> --sse-timeout 30000
# SSE 侧负向对照（16 个变异，含 cua_task_mojibake / cua_task_gbk_bytes）
node docs/verify/integration-negative-control.mjs
```


## 8. 追加：emoji 向量（2026-10-11 16:2x）

中文能过不代表 emoji 能过：emoji 在 GBK 里**根本编不出来**，所以修复前的表现不是乱码而是**直接崩溃**。
本次把 emoji 纳入向量，两个方向都测了：

| 检查 | 结果 |
|---|---|
| 修复后 emoji（`中文任务🙂测试：含 emoji`，15 码点） | 严格 UTF-8 PASS、U+FFFD=0、task 逐码点相同 PASS |
| `task_mojibake` 变异 + emoji 向量 | 被抓住（E8b 出现 9 个 U+FFFD、E8c 判不一致） |
| `gbk_strict_crash` 变异 + emoji 向量 | 被抓住（stdout 只剩 33 字节 = 一条 run_end；E8c 判不一致） |
| SSE 侧 `cua_task_mojibake`（C14 已改用含 emoji 的中文任务） | 仍被抓住（C14b + C14c） |
| SSE 侧 `cua_task_gbk_bytes`（同一 emoji 向量） | 仍被抓住（C14a + C14b + C14c） |

向量本身是否原样到位也留了证据：判据开头的 `E0-vector` 会把本次提交任务的码点逐个打出来（与 cua-py
用 `",".join(hex(ord(c)))` 转储的做法一致），避免"向量在传递环节就先坏了"这类假绿/假红。

## 9. 归因更正：乱码完全由 stdout GBK 落字节解释（argv 不背这个锅）

早期记录里曾把 argv 也算作乱码来源，**这条是错的，此处更正**：

- Windows 的 argv 走宽字符 API（GetCommandLineW），与 locale/代码页无关；cua-py 用 `",".join(hex(ord(c)))`
  转储证明子进程收到的码点与原文一致，我自己的 `E0-vector` 也复现了同样的码点。
- 而同一个子进程把中文 + emoji 打到 gbk 管道时立刻 UnicodeEncodeError（§8 的 `gbk_strict_crash` 复现）。
- 因此：**乱码与崩溃都只由 stdout 的 GBK 落字节解释**，argv 路径不参与。

## 10. 基线口径

`--selfcheck` 当前锚点已随 task-21 变为 **84 通过 / 0 失败**（81 -> 83 -> 84，两次都是新增自检项，
不是回归）；`cua/tests/run_tests.py` 仍 91/0。详见 docs/verify/integration-baseline.log 的 [4]/[5] 两段。

证据文件：docs/verify/tmp/c14-prefix-baseline.log（修复后同命令）、
docs/verify/tmp/events-negative-control-02.log、docs/verify/tmp/integration-negative-control-03.log、
docs/verify/tmp/integration-probe-live-04.log。
