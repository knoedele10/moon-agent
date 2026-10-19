# CUA 契约测试套件（verifier 独立编写）

只依据 **契约文档**（`docs/cua/03-action-protocol.md`、`docs/cua/action.schema.json`）编写的黑盒契约测试。
期望值在阅读实现源码**之前**冻结；之后只允许调整「适配层」（怎么调到实现的入口），不允许放宽任何判据。

- 依赖：Python 标准库 `unittest`（本仓库未安装 pytest）+ 已安装的 Pillow / jsonschema。
- 安全前提：套件在导入任何 `cua.*` 之前把**记录式假 pyautogui** 装进 `sys.modules`，
  因此测试在物理上不可能移动真实鼠标/键盘。

## 怎么跑

```powershell
python cua/tests/run_tests.py            # 全部用例
python cua/tests/run_tests.py -v         # 详细（含 skip 原因）
python cua/tests/run_tests.py -k coords  # 只跑文件名含 coords 的模块
python cua/tests/ct_negative_control.py  # 负向对照：注入契约违规，套件必须变红
```

退出码：`0` 全通过；`1` 有失败/错误；**`2` = 没有失败但存在 skip**（说明有未验证项，不能当通过看）。

## 文件结构

| 文件 | 作用 |
| --- | --- |
| `ct_oracle.py` | **参考实现（oracle）**：verifier 按文档独立写的契约判定，用作对照基线 |
| `ct_fake_pyautogui.py` | 记录式 pyautogui 假件：安全网 + 调用记录 + 故障注入（`fail_next`/`fail_after`） |
| `ct_adapter.py` | 适配层：把「文档里的能力」解析到实现真实入口（含调用约定归一） |
| `ct_cli_probe.py` | 在装有假件的子进程里跑 `cua.main`，用于 CLI 面取证 |
| `ct_test_00..09_*.py` | 契约用例（见下表） |
| `run_tests.py` | 套件入口（globs `ct_test_*.py`） |
| `ct_negative_control.py` | 套件自身的有效性证明（变异测试） |

## 判据分档（报告里必须区分）

1. **硬判据**：文档唯一无歧义的文本（键名白名单、坐标公式、破坏性词表硬约束、错误码分类、FAILSAFE 永久开启）。
2. **歧义判据**：文档允许多种合理解读，断言「落在允许集合内」，并把实际取值记录在案
   （例：`.5` 的四舍五入方向、`top-level array`、键名大小写宽容度）。
3. **冲突判据**：md 与 schema 不一致处 —— 按协议 §9「schema 优先」判，并把两种结果都记录下来。
   2026-10-11 Lead 裁定：`action` 永不为 null（含 `done=true`）；`final` 是终态唯一权威；
   若给了 `action.type=done`，其 `status`/`summary` 必须与 `final` 一致，不一致记 `E_SCHEMA`。

夹具一律**单点故障**（一个用例只违反一条契约），避免未文档化的错误优先级影响结论。

## 覆盖矩阵

| 模块 | 用例数 | 覆盖的契约面 |
| --- | --- | --- |
| `ct_test_00_capability` | 3 | 安全网自检 + 能力解析表（把「哪些入口已解析」写进日志） |
| `ct_test_01_json` | 10 | §2.1 JSON 提取：纯 JSON / 代码围栏 / 前后夹带文字 / 字符串里的花括号 / 转义引号 / 坏 JSON 报 `E_BAD_JSON` |
| `ct_test_02_schema` | 14 | §2/§3 Schema 严出：缺必填、类型错、枚举错、`done=true` 缺 `final`、`action=null`、`done`/`final` 一致性、未知 `action.type`、参数越界 |
| `ct_test_03_coords` | 7 | §1.1 坐标换算：全屏 / 局部 `region` 的 origin+scale、正算+逆算、小数四舍五入 |
| `ct_test_04_bounds` | 14 | §1.1/§8.2 越界：`x==width` 也算越界、必须 `E_OUT_OF_BOUNDS`、**绝不 clamp**，含执行器端到端（click/scroll/drag/region） |
| `ct_test_05_keys` | 8 | §3.3 键名白名单（68 项）与白名单外键名 `E_KEY_NOT_ALLOWED`（解析层或执行层均算，见下） |
| `ct_test_06_errors` | 12 | §5 错误码分类：可重试 vs 不可重试、`E_STALL`/`E_LOOP` 必须重规划、重试上限、`E_MODEL_HTTP` 4xx/5xx 语境、策略表与判定函数一致性 |
| `ct_test_07_destructive` | 9 | §7 破坏性词表（13 中 + 12 英）与硬约束：命中即不点击、转人工 |
| `ct_test_08_modifier_release` | 6 | §8 安全不变量：FAILSAFE 永久开启、任何失败终止都补 `keyUp`、`E_EXECUTOR_FAILSAFE` |
| `ct_test_09_dryrun_mock` | 8 | §6/§8 `--dry-run`/`--mock`：CLI 标志暴露、无不可拦截输入后端、执行层零真实输入 |

## 「拒绝发生在哪一层」的判法

契约只要求「非法输入被拒 + 给出正确错误码」，**没有**规定必须由解析器还是执行层拒绝；
`§10` 的模块分工里，键名白名单天然属于执行层。因此适配层提供**综合校验面**
（`A.reject_step`）：先问 Step 校验器，不拒再问执行器；判据（被拒 + 错误码正确）不变。
同理，越界只能由认识图像尺寸的执行层把关（解析器不认识 image 尺寸）。

## 负向对照（套件有效性证明）

`ct_negative_control.py` 在**内存里**给实现打上 11 个已知契约违规（不动磁盘文件），
再跑对应用例，要求它们**必须失败**；全部抓住才说明判据不是空的。

```
extract_json 永远成功 / schema 全放行 / 越界不拒绝 / 白名单放宽 / 词表判定失效 /
护栏永远放行 / dry-run 泄漏真实输入 / 不释放修饰键 / 全部错误都可重试 /
全部错误都重规划 / 关闭 FAILSAFE
```

> 该工具抓到过一个真实盲区：早期版本的 `dry_run_executor` 调用执行器时没传帧，
> 动作根本没执行，于是「零真实输入」断言**假通过**。修好后它的断言也补了
> 「动作必须真的被执行过」这一条，否则「零输入」不构成证据。

## 适配层的能力解析表

在装有假件的进程里打印（`ct_test_00_capability` 会输出到测试日志）：

| 能力 | 解析到的入口（当前实现） |
| --- | --- |
| `extract_json` | `cua.action_parser.extract_json` |
| `step_validate` | `cua.action_parser.validate_step_dict` |
| `image_to_screen` / `screen_to_image` | `cua.screenshot.image_to_screen` / `screen_to_image` |
| `bounds_check` | `cua.screenshot.check_image_bounds` |
| `bounds_enforced_convert` | `cua.screenshot.to_screen_point`（先检后换） |
| `key_whitelist` | `cua.executor.KEY_WHITELIST` |
| `error_policy` | `cua.errors.ERROR_TABLE` + `policy`/`is_retryable`/`is_replan`/`max_retries` |
| `destructive_words` | `cua.executor.destructive_hits` |
| `click_guard` | `cua.executor.destructive_guard` |
| `execute_action` | `cua.executor.Executor.execute(step, frame)` |
| `dry_run_executor` | `cua.executor.Executor.execute`（`dry_run=True`） |

解析不到的能力 → `SkipTest`（计 skip，退出码 2），而不是判失败；解析到但行为违反契约 → FAIL。
失败信息里会写明用的是哪个入口，便于定位。
