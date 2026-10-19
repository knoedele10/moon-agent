# task-7 验收报告：CUA 契约测试套件（verifier 独立编写）

- 任务：task-7「独立编写 CUA 契约测试套件」
- 交付物：`cua/tests/`（11 个文件）+ 本报告
- 日期：2026-10-11
- 契约版本：`cua/1`（含 Lead 2026-10-11 的 §2 终态权威性裁定、§8 预算分档说明）
- 编写纪律：期望值在**未读实现源码**前从 `docs/cua/03-action-protocol.md` 与 `docs/cua/action.schema.json` 冻结；
  之后仅调整适配层（入口解析 / 调用约定 / 结果归一），**未放宽任何判据、未删除任何用例**。

## 一、结论（先给结论）

| 判据类别 | 结果 |
| --- | --- |
| 套件自检（安全网 + 能力解析） | **PASS** 11/11 能力全部解析到实现入口，无 skip |
| 契约用例执行 | **PASS** 91 用例，failures=0，errors=0，**skipped=0**（退出码 0） |
| 套件有效性（负向对照） | **PASS** 11/11 注入的契约违规全部被抓住 |
| 契约裁定落地（`action` 永不为 null、`done`/`final` 一致性） | **PASS** 参考实现与实现同判 `E_SCHEMA` |
| 未覆盖面（本报告的 WARN，不影响 task-7 的「编写套件」目标） | 见第六节 |

原始证据：
- `docs/verify/task-7-run-08.log`（套件全量运行，UTF-8）
- `docs/verify/task-7-negative-control.log`（负向对照，UTF-8）
- 中间迭代日志 `task-7-run-01..07.log`（保留，便于复核修了什么）

复现命令（仓库根目录）：

```powershell
python cua/tests/run_tests.py -v
python cua/tests/ct_negative_control.py
```

实测输出（2026-10-11）：

```
用例: run=91 failures=0 errors=0 skipped=0          # run_tests.py -v，退出码 0
结果：11/11 变异全部被抓住（套件对已列判据有区分能力）   # ct_negative_control.py，退出码 0
```

各模块用例数（`TestLoader.countTestCases()` 实测）：

| 模块 | 用例数 | 覆盖 |
| --- | --- | --- |
| ct_test_00_capability | 3 | 安全网 + 能力表 |
| ct_test_01_json | 10 | §2.1 JSON 提取 |
| ct_test_02_schema | 14 | §2/§3 Schema 严出 |
| ct_test_03_coords | 7 | §1.1 坐标换算 |
| ct_test_04_bounds | 14 | §1.1/§8.2 越界拒绝 |
| ct_test_05_keys | 8 | §3.3 键名白名单 |
| ct_test_06_errors | 12 | §5 错误码分类 |
| ct_test_07_destructive | 9 | §7 破坏性词表硬约束 |
| ct_test_08_modifier_release | 6 | §8 安全不变量 |
| ct_test_09_dryrun_mock | 8 | §6/§8 dry-run/mock |

## 二、任务书要求的 9 项覆盖 —— 逐项到位

| 任务书要求 | 落地位置 | 证据形态 |
| --- | --- | --- |
| JSON 提取（纯 JSON / 围栏 / 夹带文字 / 字符串内花括号 / 转义引号） | `ct_test_01_json` | oracle 对照 + 实现入口 `cua.action_parser.extract_json` |
| Schema 校验（缺必填 / `done=true` 无 `final` / 未知 `action.type` / 参数越界） | `ct_test_02_schema`（43 个非法夹具） | 每个夹具单点故障，逐条断言错误码 |
| 坐标换算（全屏 + 局部 `region` 的 origin/scale，正算 + 逆算，小数取整） | `ct_test_03_coords` | 含文档黄金用例 `(640,210)@scale 0.7667 -> (835,274)` |
| 越界拒绝（`E_OUT_OF_BOUNDS`、绝不 clamp） | `ct_test_04_bounds` | 含 `x == image.width` 边界、执行器端到端四类动作 |
| 键名白名单（`E_KEY_NOT_ALLOWED`） | `ct_test_05_keys` | 68 项合法键全通过 + 15 项非法键全拒绝 |
| 错误分类（可重试 vs `E_STALL`/`E_LOOP` 必须重规划） | `ct_test_06_errors` | 20 个错误码全覆盖 + 策略表/判定函数一致性 |
| 破坏性词表（命中即不点击、转人工） | `ct_test_07_destructive` | 30 条命中文本 + 8 条安全文本 + 零键鼠调用断言 |
| 失败终止释放修饰键 | `ct_test_08_modifier_release` | 记录式假件 `pressed` 终态为空 + `keyDown/keyUp` 配对 |
| `--dry-run`/`--mock` 零真实输入 | `ct_test_09_dryrun_mock` | CLI 标志 + 无不可拦截后端 + 执行层零调用 |

## 三、判据分档与实测

### 3.1 硬判据（文档唯一无歧义）—— 全部 PASS

- §1.1 坐标公式：`screen = origin + round_half_up(image / scale)`。实测 `cua.screenshot.image_to_screen`
  与文档黄金用例、8 组参数化用例一致；逆算 `screen_to_image` 与其互逆。
- §1.1/§8.2 越界：`x == image.width`、负数、远越界共 7 点全部被拒，错误码 `E_OUT_OF_BOUNDS`；
  合法点全部放行且换算值等于公式值（**没有 clamp**）。执行器端到端复核 click/scroll/drag/screenshot region 四类。
- §3.3 白名单：68 项合法键零误拒；`f13/F0/escape/return/meta/del/control/windows/"enter "/" enter"/"ctrl+c"/""/é/clear`
  全部拒绝，错误码 `E_KEY_NOT_ALLOWED`。
- §5 错误码：20 个冻结码的可重试性与重试上限逐条比对（`E_BAD_JSON=2`、`E_SCHEMA=2`、`E_ARG_RANGE=1`、
  `E_OUT_OF_BOUNDS=1`、`E_KEY_NOT_ALLOWED=1`、`E_MODEL_TIMEOUT=2`、`E_EXPECT_TIMEOUT=1`、`E_POPUP_BLOCKING=2`），
  `E_STALL`/`E_LOOP` 必须重规划、不可重试；`E_MODEL_HTTP` 4xx 不可重试 / 5xx 可重试。
- §7 硬约束：30 条命中文本（含大小写混写 `aLlOw`）全部被护栏拒绝且**零键鼠调用**；8 条安全文本不被强制转人工。
- §8 安全：FAILSAFE 导入后为真、源码无 `FAILSAFE=False`；FAILSAFE 触发 → `E_EXECUTOR_FAILSAFE`；
  失败终止后 `pressed` 为空集、无未配对 `keyDown`。

### 3.2 歧义判据（断言落在允许集合内，并记录实际值）—— PASS

| 歧义点 | 允许集合 | 实现实际取值 |
| --- | --- | --- |
| `.5` 的四舍五入方向（「四舍五入」可读作 round-half-up 或银行家舍入） | `{round_half_up, round_half_even}` | 实现采用 round_half_up（与文档示例一致） |
| 顶层数组 `[1,2,3]` 是否算合法 JSON | `{提取成功, E_BAD_JSON}` | 提取成功并返回列表（`ExtractResult.value=[1,2,3]`） |
| 键名大小写宽容度（文档只写 a-z / F1-F12） | `{接受, 拒绝}` | 大小写不敏感（`CTRL` 与 `ctrl` 同判） |
| 小数坐标：md §2.1 宽容 vs schema `integer` | 两读都记录 | 解析层接受并把 `100.4 -> 100`、`200.6 -> 201`；schema 判其非法（见 3.3） |

### 3.3 冲突判据（§9「schema 优先」+ Lead 裁定）—— PASS

- `action = null`（无论 `done` 真假）：schema 的 `action.oneOf` 不接受 null。
  Lead 2026-10-11 裁定「action 永远是必填动作对象」，实现与参考实现同判 **`E_SCHEMA`**。
- `done=true` 但缺 `final`：两读都非法，实测 `E_SCHEMA`。
- **新增裁定**：`done=true` 且给了 `action.type=done` 时，其 `status`/`summary` 必须与 `final` 一致。
  实测「status 不一致」「summary 不一致」均判 `E_SCHEMA`，「完全一致」判合法；`done=true` 配非 done 动作 + `final` 仍判合法。
- 小数坐标：md §2.1 要求宽容，schema 要求 `integer`。本套件对**同一夹具同时记录两种结论**：
  解析层接受（宽容）、`jsonschema` 判非法（`False`）—— 这是文档自身的差异点，不是实现缺陷。

## 四、负向对照：证明套件不是「空绿」

一个全绿的套件本身不是证据。`ct_negative_control.py` 在内存里注入 11 个已知违规（不改磁盘文件），
要求对应用例必须变红：

| 注入的违规 | 目标模块 | 结果 |
| --- | --- | --- |
| `extract_json` 对坏 JSON 也成功 | ct_test_01_json | 已抓住 |
| `validate_step_dict` 全放行 | ct_test_02_schema | 已抓住 |
| `check_image_bounds` 永不拒绝（=放任 clamp） | ct_test_04_bounds | 已抓住 |
| `KEY_WHITELIST` 混入 `f13` | ct_test_05_keys | 已抓住 |
| `destructive_hits` 恒为空 | ct_test_07_destructive | 已抓住 |
| `destructive_guard` 恒放行 | ct_test_07_destructive | 已抓住 |
| dry-run 设备记录后仍真发输入 | ct_test_09_dryrun_mock | 已抓住 |
| `key_up` 变成 no-op（不释放修饰键） | ct_test_08_modifier_release | 已抓住 |
| 三个错误策略面全改成「都可重试」 | ct_test_06_errors | 已抓住 |
| `is_replan` 恒为真 | ct_test_06_errors | 已抓住 |
| 关闭 FAILSAFE | ct_test_08_modifier_release | 已抓住 |

**这个工具抓到了本次验收里唯一一个真实盲区**（见第五节 V1），说明它不是形式主义。

## 五、验收过程中发现的问题

### V1（verifier 自身缺陷，已修复）：dry-run 的「零输入」断言曾经是空断言
- 现象：第一版 `dry_run_executor` 调用执行器时只传了 action、没传帧，执行器直接抛
  `TypeError: missing 1 required positional argument: 'frame'`；动作**根本没执行**，
  于是「零真实输入调用」断言恒真。
- 修复：dry-run 入口与普通执行入口共用同一条调用路径（构造帧 + 归一结果），
  并新增「dry-run 下动作必须真的被执行（结果非拒绝）」断言 —— 否则「零输入」不构成证据。
- 证据：`docs/verify/task-7-run-07.log`（修复前后对照）与负向对照 `dry_run_sends_input` 由「未抓住」变「已抓住」。
- 结论：**判据不变，证据强度补齐**。这条记在报告里，是因为「实现自证 + 套件全绿」很容易掩盖这类空断言。

### V2（实现的可观察行为，非契约违规，供 Lead 决策）：`validate_step_dict` 会**原地改写**调用方的 Step
- 实测：直接调用实现入口时，传入 `{"x":100.4,"y":200.6}`，调用后**调用方自己的 dict** 变成 `{"x":100,"y":201}`：
  ```powershell
  python -c "import sys;sys.path.insert(0,'cua/tests');sys.path.insert(0,'.');import ct_fake_pyautogui;from cua.action_parser import validate_step_dict;s={'protocol':'cua/1','thought':'x','action':{'type':'click','x':100.4,'y':200.6},'done':False};print(validate_step_dict(s));print(s)"
  # 实测输出：{'...': 'x': 100, 'y': 201} —— 原始 s 也被改成了 100/201
  ```
- 诚实说明：这条是**在给适配层加隔离之前**观察到的。修隔离后，经适配层的调用不再把改写泄漏回夹具：
  ```powershell
  python -c "import sys;sys.path.insert(0,'cua/tests');sys.path.insert(0,'.');import ct_adapter as A;s={'protocol':'cua/1','thought':'x','action':{'type':'click','x':100.4,'y':200.6},'done':False};print(A.verdict(A.step_validator()(s)));print(s)"
  # 实测输出：OK + 原始 100.4/200.6（未被污染）
  ```
  即：**突变仍在实现里**，是套件侧用 `copy.deepcopy` 做的隔离，不是实现被改了。
- 影响：契约没有禁止原地改写；但主循环若在校验后重新序列化 Step（写日志/回喂模型/落库），
  会看到被改写过的坐标（改写幂等、方向正确，属「宽容点」的一部分）。
- 建议：不要求修，记录在案即可。

### V3（文档与实现的分工，已按契约判为 PASS）
- 键名白名单**不在解析层**，而在执行层（`cua.executor`）。契约只要求「被拒 + `E_KEY_NOT_ALLOWED`」，
  §10 的模块分工把白名单天然划给执行层 —— 因此套件用「综合校验面」取证（解析器 → 执行器），判据不变。
- 越界同理：解析器不认识图像尺寸，`image_to_screen` 文档注明「调用方必须先做边界检查」，
  真正的闸门是 `check_image_bounds`/`to_screen_point`（执行器唯一入口）。套件在执行层端到端复验。

### V4（实现暴露三套错误策略面，当前一致）
`cua.errors` 同时提供 `ERROR_TABLE`（数据）、`policy()`（对象）、`is_retryable()`/`is_replan()`/`max_retries()`（判定函数）。
套件除逐条判据外，另加「策略表 vs 判定函数结论一致」用例，防止「表对、函数错」被漏掉。实测一致。

## 六、我没能验证的部分（WARN，明确列出）

1. **主循环级 dry-run 端到端**：需要真实模型 + 截图注入点才能验「`--dry-run` 跑完整循环也不碰键鼠」。
   本套件只覆盖 CLI 标志暴露 + 执行层零输入（两句都有实测证据），主循环级标记为**待验证**。
2. **40 步 / 300 秒预算**：Lead 已澄清 300 秒只适用云端 API 档，本地档由 config 分档。
   本套件**不把 40/300 当通用不变量**，未对其断言（属于 task-9 的框架/文档一致性范围）。
3. **四级路由与 `E_STALL`/`E_LOOP` 的真实触发**：需要真实模型与多步轨迹，属 task-9 范围。
4. **多显示器虚拟桌面坐标系**：实现自述为「已知限制（pyautogui 只报主屏几何）」，本套件未验（无多屏环境）。
5. **`DestructiveGuardWithoutDedicatedEntry` 分支**：当前实现有专用护栏入口，该回退分支未被触发（设计如此）。

## 七、给 Lead 的一句话

task-7 的契约套件已可交付：**91 用例全绿、0 skip，并且用 11 个注入违规证明它不是空绿**；
唯一发现的实现侧可观察行为（V2 原地改写 Step）不违反契约，建议记录不修。
