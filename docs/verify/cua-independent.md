# task-9 验收报告：CUA 框架与文档一致性（独立复验）

- 任务：task-9「独立复验 CUA 框架与文档一致性」
- 日期：2026-10-11
- 复验人：verifier（独立于 cua-arch / cua-code）
- 被判对象：cua/ 包（cua/1 冻结契约）+ docs/cua/01-architecture.md、02-agent-prompt.md、03-action-protocol.md、action.schema.json
- 独立性：一致性检查用**机器解析**（脚本 docs/verify/cua-consistency.py：从文档解析出事实、从代码取出同一事实、逐条比对），
  黑盒探测按契约文本先写断言再跑（脚本 docs/verify/cua-blackbox.py），不采信任何「实现者自述」的文字结论。

## 一、结论（先给结论）

| # | 复验项 | 结论 | 证据 |
| --- | --- | --- | --- |
| 1 | 复用 task-7 契约套件 | **PASS** 91 用例，0 失败 / 0 错误 / 0 跳过（exit 0） | task9-contract-suite.log |
| 2 | 套件有效性（负向对照） | **PASS** 11/11 注入违规被抓住（exit 0） | task9-negative-control.log |
| 3 | python -m cua.main --selfcheck（自己跑） | **PASS** 81 项通过 / 0 失败（exit 0） | cua-selfcheck.log |
| 4 | python -m cua.main --mock --dry-run（自己跑） | **PASS** 5 步闭环成功，设备操作 6 条全在 DryRunDevice（exit 0） | cua-dryrun.log |
| 5 | 六条黑盒行为探测 | **PASS** 8/8（含两条反例） | cua-blackbox-01.log |
| 6 | 文档↔代码一致性（41 项机械比对） | **PASS** 41/41，不一致 0 | cua-consistency-final.log |
| 7 | 9 种动作（文档 / Schema / 派发表 / 提示词 四处） | **PASS** 四处完全一致 | cua-consistency-final.log §A |
| 8 | 20 个错误码 + 逐码可重试性/上限 | **PASS** 键集合一致，每码 retryable/max_retries 一致 | 同上 §B |
| 9 | 键名白名单 68 项 | **PASS** 集合一致（大小写归一后） | 同上 §C |
| 10 | 破坏性词表 25 条（13 中 + 12 英） | **PASS** 集合一致 | 同上 §D |
| 11 | 坐标公式与越界（绝不 clamp） | **PASS** 6 点换算与公式一致、4 点越界全部 E_OUT_OF_BOUNDS | 同上 §E |
| 12 | 四级路由 L0~L3 | **PASS** 常量、文档、兜底行为、路由记录字段全部一致 | 同上 §F |
| 13 | 弹窗检测阈值与处置优先级 | **PASS** 3% / 80% / 0.25 + 三条行为用例 + esc≤2 转人工 | 同上 §G |
| 14 | 预算分档（40/300 只对云端档） | **PASS** cloud 40/300、local 40/1800、auto 按 base_url | 同上 §H |
| 15 | 安全不变量第 8 节七条 | **PASS** 七条都有代码级证据 | 同上 §I |
| 16 | 提示词与文档同源 | **PASS** 运行期提示词直接取自 docs/cua/02-agent-prompt.md（6097 字） | 同上 §J |

**总体判定：PASS，未发现契约不符。** 需要 Lead 知道的「记法差异」与「待验证项」在第四、五节单独列出，
不混进上面的结论表。

## 二、复现命令与证据文件

~~~powershell
python -X utf8 cua/tests/run_tests.py -v                  # 契约套件（91 项）
python -X utf8 cua/tests/ct_negative_control.py           # 套件有效性
python -X utf8 docs/verify/cua-consistency.py             # 文档↔代码 41 项机械比对
python -X utf8 docs/verify/cua-blackbox.py                # 六条黑盒行为探测
python -X utf8 -m cua.main --selfcheck                    # 框架自检（自己跑，不看 SELFCHECK.txt）
python -X utf8 -m cua.main --task "..." --mock --mock-scenario popup --dry-run --no-log
~~~

原始输出（UTF-8）都在 docs/verify/tmp/ 下：
task9-contract-suite.log、task9-negative-control.log、cua-consistency-final.log（另有 01~03 记录脚本自身修错过程）、
cua-blackbox-01.log、cua-selfcheck.log、cua-dryrun.log。

实测汇总（原文）：

~~~
用例: run=91 failures=0 errors=0 skipped=0                 # SUITE_EXIT=0
结果：11/11 变异全部被抓住（套件对已列判据有区分能力）        # NEG_EXIT=0
检查项 41 ；一致 41 ；不一致 0                             # CONS_EXIT=0
---- 汇总：探测 8 项，通过 8，失败 0 ----                    # BLACKBOX_EXIT=0
  通过 81 项，失败 0 项                                     # SELFCHECK_EXIT=0
（dry-run 闭环）步数：5  重规划：0  弹窗事件：1  用时：1.3s  # DRYRUN_EXIT=0
---

## 三、六条黑盒探测（task-9 第 3 项的逐条回答）

| 要求 | 探测方式 | 结果 |
| --- | --- | --- |
| dry-run 下没有任何真实键鼠调用 | 走执行入口（dry_run=True）发一次 click，记录式假件统计输入调用 | **PASS** 真实输入调用 = []，结果 OK |
| 越界坐标被拒且未被 clamp | 1472x828 图上给 x=1972 | **PASS** E_OUT_OF_BOUNDS；同一坐标按公式算仍是 (1972,20)，未被夹回边界 |
| 围栏 + 前后自然语言仍可解析 | 喂「好的，我看清楚界面了。+ json 围栏 + 以上是这一步的决策。」 | **PASS** 解析出 action={"type":"click","x":640,"y":210} |
| 未知 action.type 报 E_UNKNOWN_ACTION | 喂 type=exec 与 type=shell（协议刻意不提供的命令执行类） | **PASS** 两者都判 E_UNKNOWN_ACTION（没有 exec/shell 逃逸口） |
| 失败终止后修饰键被释放 | 注入 FailSafe 让 ctrl+c 的 keyDown 后立刻失败 | **PASS** 仍按下 = {}，未配对 keyDown = [] |
| 弹窗命中破坏性词不点击、转人工 | 护栏喂「永久删除」按钮 + 反例「取消」 | **PASS** 命中 -> E_NEED_HUMAN 且零输入调用；「取消」放行（不误伤） |

原文摘录：

~~~
[PASS] dry_run_zero_input     入口 cua.executor.Executor.execute（dry_run=True）；真实输入调用=[]；结果=OK
[PASS] oob_rejected_not_clamped  x=1972 于 1472x828 -> E_OUT_OF_BOUNDS；同一坐标按公式算是 (1972, 20)
[PASS] fenced_json_parsed     解析入口 cua.action_parser.extract_json；得到 action={"type": "click", "x": 640, "y": 210}
[PASS] unknown_action_code    拒绝面 step 校验/执行面 -> E_UNKNOWN_ACTION
[PASS] no_shell_escape_hatch  shell 同样被拒 -> E_UNKNOWN_ACTION
[PASS] modifiers_released_on_failure  注入 FailSafe 后 ctrl+c -> OK；仍按下={}；未配对 keyDown=[]
[PASS] destructive_needs_human  词表命中 ['删除']；护栏判定 E_NEED_HUMAN（未执行点击；真实输入调用=[]）
[PASS] safe_button_not_forced_human  「取消」不命中词表，护栏判定 放行
~~~

补充（CLI 端，我自己跑的，不是看 SELFCHECK.txt）：

~~~
== 结论 ==
  通过 81 项，失败 0 项
  [PASS] dry-run 空转验证  -> 全程 DryRunDevice：15 条模拟操作，真实 pyautogui 未被调用
--- 设备操作记录（dry_run，未触碰真实鼠标/键盘）---
[ 1] {"button":"left","clicks":1,"op":"click","x":835,"y":275}
[ 2] {"key":"ctrl","op":"key_down"} ... [ 5] {"interval":0.0,"op":"write","text":"hello cua"}
[ 6] {"keys":["ctrl","s"],"op":"hotkey"}
~~~

## 四、文档↔代码逐条一致性（41 项，全部一致）

### 4.1 动作类型：四处一致
文档 §3.1~3.9、action.schema.json、cua/executor.py 派发表、docs/cua/02-agent-prompt.md 动作清单
解析出的集合完全相同：click / type / hotkey / drag / scroll / move / wait / screenshot / done（9 种，无第 10 种）。

### 4.2 错误码：20 个，键集合与逐码策略都一致
- 文档 §5 表格 20 行 ↔ cua/errors.py 的 20 个常量 ↔ ERROR_TABLE 的 20 个键，**三处集合相同**（仅文档有 / 仅代码有都是空）。
- 逐码比对 retryable 与 max_retries：13 个「否」、7 个「是，最多 N 次」逐条相符（E_BAD_JSON=2、E_SCHEMA=2、E_ARG_RANGE=1、
  E_OUT_OF_BOUNDS=1、E_KEY_NOT_ALLOWED=1、E_NO_FOCUS=1、E_MODEL_TIMEOUT=2、E_EXPECT_TIMEOUT=1、E_POPUP_BLOCKING=2）。
- E_MODEL_HTTP 文档写「5xx 是 / 4xx 否」：代码内部存 None（表示取决于上下文），is_retryable(code, 500) = True、
  is_retryable(code, 404) = False —— **语义与文档一致**，比塞一个静态 bool 更贴文档。
- E_STALL / E_LOOP：is_replan 为真且不可重试，与文档「必须重规划」一致。

### 4.3 键名白名单：68 项一致
文档文本展开出 68 项（a-z、0-9、F1-F12 + 24 个具名键），代码 KEY_WHITELIST 也是 68 项，**大小写归一后集合完全相同**。

### 4.4 破坏性词表：25 条一致
文档 §7 与 cua/config.py DESTRUCTIVE_WORDS 都是 13 中文 + 12 英文 = 25，对称差为空；
docs/cua/02-agent-prompt.md 里也逐字写出了同一套词（提示词与词表同源，不是两处维护）。

### 4.5 坐标公式与越界
- 文档 §1.1 写明 screen = origin + round(image / scale)；代码 cua/screenshot.py 是
  frame.origin[0] + round_half_up(x / scale)。
- 用 task-7 从文档独立实现的参考公式做 6 点数值比对：完全一致；4 个越界点（x=width、y=height、负数、极大值）
  全部 E_OUT_OF_BOUNDS，且越界点按公式算出的值仍是原值（1972），**没有夹回边界**。

### 4.6 四级路由
ROUTE_LEVELS = L0/L1/L2/L3 与文档 §6 的四级一致；无探针时兜底到 L3，reason 里明确写「按协议第 6 节落到 GUI 视觉操控兜底」；
路由记录含 level / reason / probe_result 三个字段。

### 4.7 弹窗策略
- 阈值：文档 §7 的 3%~80% 面积比、填充率 > 0.25 ↔ 配置 popup_area_min_ratio=0.03 / max=0.80 / fill_min_ratio=0.25。
- 行为：我自己合成三种样张喂 diff_frames —— 30% 面积模态框 -> 判为疑似弹窗（面积比 0.270、填充率 1.00）；
  90% 贴满全屏的突变 -> 不判弹窗（0.847，避免整屏变化误判）；单像素变化 -> 不判弹窗（0.000002）。
- 处置优先级：裁剪放大（popup_auto_crop=True）-> 非破坏性选择 -> esc（最多 2 次）-> E_NEED_HUMAN，与文档 §7 一致。

### 4.8 预算分档（Lead 2026-10-11 裁定 R2）
- Config() 默认 cloud：40 步 / 300s / 截图长边 1472；local 档：40 步 / 1800s / 长边 1280。
- resolve_budget_profile("auto", 本地地址) -> local；远端地址 -> cloud；显式 --max-steps/--max-seconds 覆盖分档。
- 文档 §8.4 已写明「只对云端 API 后端成立」并给出 1800s。
- 因此 task-7 的套件**不把 300s 当通用不变量**：这是本次协作里两处裁定的一致性落点之一。

### 4.9 安全不变量（第 8 节七条）
| 条款 | 代码级证据 |
| --- | --- |
| 8.1 FAILSAFE 永久开启 | executor 显式 pyautogui.FAILSAFE = True，全仓库无 FAILSAFE=False |
| 8.2 越界不 clamp | check_image_bounds 直接抛 E_OUT_OF_BOUNDS；越界点换算仍是原值 |
| 8.3 步间 >=150ms | executor 的 min_step_sleep 逻辑（dry-run 记录里 min_sleep_s=0.15） |
| 8.5 危险组合键默认关闭 | 存在危险组合键开关（默认关闭） |
| 8.6 risk=high 需放行 | execute 结果 meta 带 guardrail_required / risk，高风险需放行 |
| 8.7 终止释放修饰键 | executor.execute 的 finally 与 loop.run 的 finally 都调用 release_all_modifiers（覆盖正常/异常/Ctrl+C） |

### 4.10 终态权威性（Lead 裁定 R1）
文档 §2 写明 action 永不为 null（含 done=true）、final 是唯一权威、若给 action.type=done 则必须与 final 一致；
代码行为与我 task-7 的独立参考实现同判：action=null、done 动作与 final 不一致 -> E_SCHEMA；完全一致 -> 合法；
done=true 配非 done 动作 + final 也合法（不强制 action.type 必须是 done），与裁定逐字相符。

### 4.11 提示词同源
cua/prompts.py 的 system_prompt() 直接从 docs/cua/02-agent-prompt.md 抽取提示词块
（实测来源 = ...\docs\cua\02-agent-prompt.md，长度 6097）；该文本含 9 种动作名、cua/1、键名白名单、
E_KEY_NOT_ALLOWED、need_human 与中英文破坏性词表。代码里另有一份占位提示词，但**运行期未启用**（不是双真相源）。

## 五、记法差异（不是契约不符，但值得登记）

1. **round vs round_half_up**：文档写 round(x/scale)，实现用自定义 round_half_up（四舍五入），
   源码 docstring 明说「协议第 1.1 节写的是 round(...)，本函数就是它的确定性实现」。
   Python 内置 round 是银行家舍入、对 .5 方向不稳定；实现选了一个确定方向。
   我 task-7 把「.5 的四舍五入方向」判为**歧义判据**（允许 round_half_up / round_half_even），实测取值 = round_half_up。
   建议（不阻塞）：在文档里把 round 明确写成「四舍五入（.5 向上）」即可彻底消歧。
2. **F1-F12 的大小写**：文档写 F1-F12，代码存 f1..f12。/api/kb/* 无关；CUA 侧执行前统一小写，
   task-7 已实测 CTRL 与 ctrl 同判（大小写不敏感），属记法差异。
3. **路由记录字段是超集**：文档要求 route 记录 level/reason/probe_result，实现多带一个 candidates（诊断用）。
   文档用的是「记录这三个」而不是「只能有这三个」，属超集，不影响可审计性。
4. **E_ARG_RANGE 与 E_SCHEMA 的边界**：实现自检 §12 把它登记为已知边界（action 内 minimum/maximum 越界 -> E_ARG_RANGE，
   其余结构不符 -> E_SCHEMA）。文档未逐字段写这条分界；task-7 的套件按单点故障夹具避开了它，
   所以本次**没有把它判成不符**，只登记。

## 六、我没能验证的部分（明确列出）

1. **多显示器虚拟桌面合并坐标系**：pyautogui 只报主屏几何，实现自述为已知限制（01-architecture 亦标注）。本机单屏，无法实测。
2. **本地视觉模型真实端到端（Ollama qwen2.5vl:7b）**：框架自检全程用 MockScreenshot + DryRunDevice，
   没有调用真实 VLM；我也没跑（真实 VLM 单步 9~78s，属「设计如此/待验证」）。
3. **弹窗按钮文字识别**：依赖模型给出的文本字段，未接入 OCR（自检 §12 已登记）。我验证的是「给到文字后护栏如何判」，
   不是「从像素里读文字」。
4. **真实模型下的 40 步 / 300s / 1800s 预算行为**：我只验证了分档取值与 auto 判定，没有用真实模型跑满预算。
5. **E_STALL / E_LOOP 的真实触发**：需要真实模型多步轨迹；task-7 只验证了策略映射（必须重规划）。
6. **主循环级 dry-run 端到端**：task-7 只覆盖 CLI 标志 + 执行层零输入；CLI 级我只跑了 --mock --dry-run 的闭环（5 步），
   没有用真实模型跑 dry-run。

## 七、给 Lead 的一句话

task-9 **PASS，零契约不符**：91 项契约用例 + 11/11 负向对照 + 自己跑的 selfcheck(81/0) 与 dry-run 闭环 +
8/8 黑盒探测 + 41/41 文档↔代码机械比对全绿；两处裁定（action 永不为 null / 40-300 只对云端档）在文档和代码两侧都落地。
需要登记的不是缺陷而是三处记法差异：round 写成 round_half_up、F1-F12 存成小写、路由记录多一个 candidates 字段。
