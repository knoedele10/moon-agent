# cua —— 视觉 CUA 最小可运行框架（协议 cua/1）

blog-my 项目里的 **Computer-Use Agent（视觉 GUI 操控）**最小可运行框架。
本包只实现契约，不改契约：

- 唯一接口契约：[docs/cua/03-action-protocol.md](../docs/cua/03-action-protocol.md)（冻结）
- 机器可读 Schema：[docs/cua/action.schema.json](../docs/cua/action.schema.json)（冲突时以 Schema 为准）
- 系统提示词来源：[docs/cua/02-agent-prompt.md](../docs/cua/02-agent-prompt.md)（第 1 节 text 代码块）

## 1. 快速开始（全部离线，无显示器、不动真实鼠标）

```powershell
# 全链路离线自检（解析 / 校验 / 坐标换算 / 越界 / 执行器 / 弹窗 / 路由 / 端到端）
python -m cua.main --selfcheck

# 脚本化假模型 + 只打印将执行的动作，不真按键、不截图
python -m cua.main --dry-run --mock

# 自带脚本文件（JSON 数组，每项是一段「模型原文」）
python -m cua.main --dry-run --mock --script cua/mock/demo.json
```

三条命令都不需要显示器、模型服务或数据库；`--mock`/`--selfcheck` 默认使用 **合成屏幕**
（`MockScreenshot`）与 **DryRunDevice**，真实 pyautogui 只被导入（用于断言 `FAILSAFE=True`），
从不调用任何输入函数。

### 接真实模型

```powershell
# Ollama（本机已装 qwen2.5vl:7b），默认 base_url 就是它
python -m cua.main --task "打开记事本，输入 hello 并保存" --budget-profile local

# 其他 OpenAI 兼容端点
python -m cua.main --task "..." --base-url https://api.openai.com/v1 --model gpt-4o-mini --budget-profile cloud
```

| 参数 | 作用 |
|---|---|
| `--task` | 任务描述（不传时用内置离线演示任务） |
| `--dry-run` | 只打印将执行的动作，不真按键；越界/白名单等语义检查照常执行 |
| `--mock` | 使用脚本化假模型（离线） |
| `--selfcheck` | 离线全链路自检，全部通过退出码 0，否则 1 |
| `--script` | `--mock` 的脚本文件 |
| `--budget-profile` | `cloud`（40 步/300s/截图长边 1472）| `local`（40 步/1800s/长边 1280）| `auto` |
| `--max-steps` / `--max-seconds` | 显式预算，优先级高于分档 |
| `--real-screen` | 即使是 `--mock` 也用真实截图（默认不用） |
| `--allow-high-risk` | 放行 `risk=high` 的动作（护栏，默认不放行） |
| `--no-log` | 不写 JSONL 日志 |

## 2. 模块结构与契约对应关系

| 模块 | 职责 | 契约位置 |
|---|---|---|
| `cua/screenshot.py` | 截图、`image -> screen` 坐标换算、帧差分/感知哈希、弹窗启发式 | 第 1、1.1、7 节 |
| `cua/observation.py` | L1 Observation 组装、压缩历史 `history_tail` | 第 1 节 |
| `cua/action_parser.py` | 宽进提取 JSON（全文直解 → 剥围栏 → 平衡括号）+ Schema 校验（自研子集，无新依赖） | 第 2、2.1 节 |
| `cua/executor.py` | 9 种动作落到 pyautogui；安全不变量；dry-run 设备 | 第 3、4、8 节 |
| `cua/errors.py` | 20 个错误码 + 重试/重规划策略表 | 第 5 节 |
| `cua/router.py` | L0 → L1 → L2 → L3 路由骨架 + capability probe | 第 6 节 |
| `cua/loop.py` | 主循环、双预算、stall/loop 检测、弹窗策略、JSONL 日志 | 第 4、5、7、8 节 |
| `cua/agent.py` + `cua/prompts.py` | 视觉推理 Agent：提示词 + 模型调用 + 解析重问 | 第 2 节 + 02 号文档 |
| `cua/backends/` | 模型客户端：OpenAI 兼容多模态 + mock | —— |
| `cua/main.py` | CLI、`--selfcheck`、`--dry-run`、`--mock` | —— |
| `cua/config.py` | 配置、键名白名单、危险组合键、破坏性词表、预算分档 | 第 3.3、7、8 节 |

`cua/tests/` 由独立验收方（verifier）编写，不属于本实现，本包不修改其中的任何文件。

## 3. 安装

Python 3.13（已在 3.13.13 + Windows 11 上实测）：

```powershell
python -m pip install -r cua/requirements.txt
```

**没有引入任何新依赖**：`pyautogui / Pillow / numpy / openai / httpx / pyperclip` 都是本机既有环境；
`jsonschema` 只作为「可选交叉校验」（装了就用它复核一遍，没装也不影响，校验由自研子集完成）。
运行不需要 MySQL、不需要 Go 后端、不需要网络。

## 4. 关键设计决策（显式登记）

1. **坐标换算**（协议 1.1，实测）：`screen = origin + round(x / scale)`。
   `round` 用 `int(floor(v+0.5))`，即 `.5` 一律向上，避免 Python 银行家舍入带来的不确定性。
   协议第 4 节样例已实测通过：image 1472x828、scale=0.7667、origin=(0,0) 下 `(640,210) -> (835,274)`。
2. **越界绝不 clamp**（实测）：`x<0 / x>width-1 / y<0 / y>height-1` 一律 `E_OUT_OF_BOUNDS`，
   错误消息回显实际值与当前 `image.width/height`；越界动作不会产生任何设备操作（自检用 RecordingDevice 断言）。
   `screenshot.region` 的越界判定由**执行器自己**完成（像素区间 `[x, x+width)`，允许 `x+width == image.width` 贴边），
   不依赖某个提供者恰好做了校验——不同 provider 对越界区域的反应不同（报错 / 静默裁剪 / 抛底层异常），
   这曾经把一条真实缺陷变成了 `E_INTERNAL`（2026-10-11 由 verifier 套件发现并修复）。
3. **E_ARG_RANGE 与 E_SCHEMA 的边界**（设计如此，Lead 已确认口径）：
   `action` 对象内的 `minimum/maximum` 越界（如 `dy=99`、`wait.ms=50`、`region.width=10`）记 `E_ARG_RANGE`；
   其余结构性问题（缺字段、类型/枚举/长度/元素个数不符、`done=true` 缺 `final`）记 `E_SCHEMA`。
4. **FAILSAFE 常开**（实测）：`pyautogui.FAILSAFE=True` 在设备构造时强制设置，**不是配置项**（协议 8.1）；
   `FailSafeException` 映射为 `E_EXECUTOR_FAILSAFE`，先释放所有按键再停手。
5. **步间 sleep 不少于 150ms**（实测计时）：每步在 `finally` 里补足，与动作本身耗时无关。
6. **任何终止路径释放修饰键**（实测）：`key_down` 全部登记在 `_held`，`finally` 补 `key_up`；
   拖拽中断也保证 `mouse_up`（否则 Windows 上会留下卡住的 Ctrl）。
7. **E_STALL / E_LOOP 是重规划而不是重试**（实测）：stall 默认连续 5 步变化像素占比 < 0.5%；
   loop 默认同一「动作指纹 + 画面 sha256」第 3 次命中。两者都触发换策略提示并把重规划次数计入预算（默认 2 次），
   用尽才终止；画面变了 loop 计数自然分离。
8. **弹窗启发式用两级阈值**（设计如此）：弱变化（通道差 > 8）算 `change_ratio`（协议第 4 节口径），
   强变化（> 45）用来定「模态级突变」的 bbox。只用一级会导致**整屏压暗把 bbox 撑满全屏**，
   于是「bbox 面积比 3%~80%」这条规则永远不成立。附加信号 `center_block` / `background_darkened` 只做增强，不单独触发。
   处置顺序按协议第 7 节：先裁剪放大可疑区域 → 优先非破坏性按钮 → 判断不了按 esc（最多 2 次）→ 仍无效转 `E_NEED_HUMAN`。
9. **破坏性词表硬约束**（实测）：仅在「疑似弹窗」上下文中生效（协议 7.4 的措辞是「可疑弹窗按钮文字」），
   命中删除/卸载/格式化/delete/format 等词直接 `E_NEED_HUMAN`，不点击。
   未接入 OCR：扫描的是模型给出的文本字段（`thought`、`action` 内字符串、`final.summary`）——**待验证**。
10. **打字优先走剪贴板**（task-25 实测修订，2026-10-11）：**所有非空文本都先写剪贴板再 `Ctrl+V`**
    （ASCII 也是）。原因是 `pyautogui.write` 逐字符 `press`，真实 Windows 上会丢键——实测在记事本输入
    `hello from CUA` 落成 `hellofromCUA`（窗口标题 `*hellofromCUA - Notepad`，两个空格系统性丢失）；
    粘贴是原子操作，不丢字符且更快。仅当剪贴板不可用（写后读不回）或 `type_use_clipboard_for_unicode=False`
    时才退化为逐字符（换行 -> enter，非 ASCII 跳过并记 `type_skipped_chars`）。
    注意：输入会占用系统剪贴板；空文本记 `type_path=empty`。
11. **预算按后端分档**（2026-10-11 裁定）：`cloud` 40 步 / 300s / 截图长边 1472；
    `local` 40 步 / **1800s** / 长边 **1280**（本地 VLM 实测 9~78 秒/步，300s 只够 4~12 步）。
    默认保持 cloud 档；`--budget-profile auto` 按 base_url 是否本机自动判定；
    显式 `--max-steps/--max-seconds` 覆盖分档。
12. **终态字段裁定**（2026-10-11）：`action` 恒为必填动作对象，**不可为 null**（`action=null` 记 `E_SCHEMA`）；
    `done=true` 必须给 `final`；`final.status/summary` 是终态唯一权威来源；
    若同时给了 `action.type="done"`，其 `status/summary` 必须与 `final` 完全一致，否则 `E_SCHEMA`；
    `done=true` 配任意合法动作也合法——该动作会先执行，再按 `final` 收尾。
13. **JSON 校验不依赖第三方**（实测）：`cua/action_parser.py` 内置 draft 2020-12 所需子集
    （type / required / properties / additionalProperties / items / minItems / maxItems / minLength / maxLength / minimum / maximum / enum / const / oneOf / anyOf / allOf / if-then / ref）。
    自检会把同一批样例同时喂给自研子集与 `jsonschema`，断言「合法/非法」判定一致（本机 25 个样例一致）。

## 5. 日志与排障

默认写 `cua/logs/cua.jsonl`（每行一个事件，UTF-8 JSON）：run_start / frame / observation / agent /
model_retry / step / action_result / executor_retry / expect_timeout / popup_detected / popup_esc /
replan / terminal_action_result / run_end。

```powershell
Get-Content cua/logs/cua.jsonl | Select-String '"kind":"action_result"' | Select-Object -Last 5
```

模型每步的原始输出没有单独落盘（只落解析后的 step、以及 reask 事件）。需要排查「模型为什么这么想」时，
看 step 事件里的 `thought` 与 agent 事件里的 `model_reask`。

## 6. 自测与证据

- `cua/SELFCHECK.txt`：`python -m cua.main --selfcheck` 与
  `python -m cua.main --dry-run --mock --no-log` 两条命令的**完整输出 + 退出码**（生成命令写在文件开头）。
- 自检覆盖：环境与依赖、契约一致性（9 种动作 / 20 个错误码与文档逐条对齐 / 策略表）、
  宽进提取 6 例、Schema 校验（9 合法 + 16 非法 + 未知字段 + 小数坐标 + 终态裁定）、
  与 jsonschema 交叉校验、坐标换算与逆换算、越界拒绝（含「不产生设备操作」）、
  执行器 9 动作 / 修饰键 / FAILSAFE / 白名单 / 危险组合键 / 打字路径 / 滚动映射 / 150ms 计时 / 局部放大、
  错误与重试策略、弹窗/卡死/循环检测、破坏性词表、四级路由、端到端 13 个场景、预算分档。
- 最近一次自检：**81 项通过 / 0 项失败**（最终数字以 SELFCHECK.txt 为准）。

独立验收（不属于本包，由 verifier 另写）：

[[F]]powershell
python cua/tests/run_tests.py       # 0 = 全通过；1 = 有失败；2 = 有跳过（未验证项）
[[F]]

- 最近一次独立套件结果：**90 用例 / 0 失败 / 0 错误 / 0 跳过，退出码 0**（2026-10-11，本机）。
- 套件发现并已修复的真实缺陷：`screenshot.region` 越界未由执行器把关，遇到不做校验的 provider 时会退化成 `E_INTERNAL`。

## 7. 已知限制与「待验证」

| 事项 | 状态 |
|---|---|
| 本地 VLM（Ollama `qwen2.5vl:7b`）端到端真实操控 | **待验证**：本仓库自检全程离线，未对真实桌面跑过 |
| 多显示器「虚拟桌面合并坐标系」 | **待验证**：pyautogui 在 Windows 上报的是主屏几何，合并坐标系未实测 |
| 弹窗按钮文字识别 | **待验证**：未接 OCR，只扫模型给出的文本字段 |
| `unit="page"` 的滚动折算 | **设计如此**：1 页 = 5 行（`config.scroll_page_lines`），不是协议规定值 |
| `risk=high` 的放行策略 | **设计如此**：协议层只标记，本实现的调用方用 `--allow-high-risk` 开关放行 |
| `type` 动作的 `target` 只支持 `"focused"` 或 `{x,y}` | **设计如此**：其他取值记 `E_ARG_RANGE` |
| 系统提示词与 02 号文档的一致性 | **实测**：启动时逐字读取 02 号文档第 1 节的 text 代码块；文档缺失时用 `cua/prompts.py` 占位 |

## 8. 契约版本与改动纪律

- 当前协议 `cua/1`；本包不做「实现时顺手改一下契约」。
- 协议与 Schema 冲突时以 **Schema 为准**（协议第 9 节）。2026-10-11 Lead 已就地澄清两处（终态字段、预算分档），
  版本号仍是 `cua/1`，本包已同步。
- 如果发现本实现与契约不符：先看 `cua/SELFCHECK.txt` 的对应断言，再核对 Schema，最后才考虑改代码。
