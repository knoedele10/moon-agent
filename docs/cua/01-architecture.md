# CUA 系统架构设计（cua v1）

> **状态**：设计文档（v1，2026-10-11）。实现由 task-5 落在 `cua/` 包下，本文档描述**应该长成什么样**与**为什么**。
> **唯一接口权威**：[03-action-protocol.md](03-action-protocol.md)（冻结）+ [action.schema.json](action.schema.json)（机器可读，两者不一致时以 Schema 为准）。
> **契约 v1 裁定（2026-10-11）**：终态字段以 Schema 为准——`action` 永远是必填的动作对象、不可为 `null`，`final` 是终态唯一权威来源；预算默认值 40 步 / 300 秒**只对云端 API 档**成立，本地档按后端分档。详见 §13 R1/R2 与契约 §2、§8.4。
> 本文档**不重新定义**任何字段；契约里已有的东西这里只引用章节号，不复制一份（复制会产生第二个真相源）。
>
> **证据标记约定**（全仓库文档约定）：
> - **实测**：本机跑过，给出命令与数值。
> - **设计如此**：有意做的设计决策，尚未跑过。
> - **待验证**：还没证据，可能错，标记出来防止被当成结论引用。
> 本文档中所有性能数字除非标注「实测」，一律视为「估算」或「待实测」。

---

## 0. 环境基线（实测，2026-10-11）

这一节是后面所有性能/预算结论的分母。换机器必须重测。

| 项 | 实测值 | 获取方式 |
|---|---|---|
| 操作系统 | Windows 11，10.0.26200 | `platform.platform()` |
| CPU | AMD Ryzen 9 8945HX | `Win32_Processor` |
| 内存 | 31.8 GB（当时空闲 10.7 GB） | `Win32_ComputerSystem` / `Win32_OperatingSystem` |
| GPU | NVIDIA GeForce RTX 5060 Laptop GPU，8151 MiB，驱动 582.05 | `nvidia-smi` |
| 主屏分辨率 | **2560 x 1600（物理像素）** | `GetSystemMetrics(0/1)`（进程 DPI aware 之后） |
| 显示器数量 | 1（虚拟桌面 = 主屏） | `GetSystemMetrics(80)` |
| 系统 DPI | 144 → **150% 缩放** | `GetDpiForSystem()` / `GetDeviceCaps(LOGPIXELSX)` |
| Python | 3.13.13 | `sys.version` |
| PyAutoGUI | 0.9.54，`FAILSAFE=True`，`PAUSE=0.1` | 直接读 `pyautogui` 常量 |
| Pillow / numpy | 12.2.0 / 2.2.0 | 模块 `__version__` |
| Ollama | `http://127.0.0.1:11434` 可达 | `/api/tags` |

Ollama 已装模型（实测 `/api/tags`）：

| 模型 | 体积 | 参数/量化 | 能力 | 在本项目中的角色 |
|---|---|---|---|---|
| `qwen2.5vl:7b` | 5.97 GB | 8.3B / Q4_K_M，ctx 128000 | completion + **vision** | 视觉推理 Agent 的候选主力 |
| `qwen2.5:7b` | 4.68 GB | 7.6B / Q4_K_M | completion, tools | 无视觉；纯文本规划/纠错备份 |
| `qwen3:4b` | 2.50 GB | 4.0B / Q4_K_M，ctx 262144 | completion, tools, thinking | 低成本文本规划 |
| `bge-m3:latest` | 1.16 GB | 566.70M / F16 | embedding | 知识库检索（与 CUA 无直接关系） |

---

## 1. 模块划分

### 1.1 模块总表

九个模块，分四层。**依赖方向单向向下**（见 1.2），任何反向依赖都算设计事故。

| # | 模块 | 层 | 职责（一句话） | 输入 | 输出 | 依赖 |
|---|---|---|---|---|---|---|
| M1 | `screenshot.py` | 感知 | 抓屏、缩放编码、坐标换算 | 区域参数（image 空间） | PNG 字节 + `{width,height,scale,origin,sha256}` | Pillow、pyautogui/pyscreeze |
| M2 | `observation.py` | 感知 | 组装 L1 Observation；画面差分与指纹 | 帧、上次 ActionResult、历史、预算 | Observation 字典（契约 §1） | M1、numpy |
| M3 | `agent.py` | 决策 | 调模型拿 L2 Step 原始文本；重试与超时 | Observation、系统提示词、历史 | 模型原始文本 + 用量统计 | backends/、M6(prompts) |
| M4 | `action_parser.py` | 解析 | 宽进提取 JSON → Schema 校验 → 语义校验 | 模型原始文本、当前 image 几何 | 规范化 Step + `meta.coord_rounded` | action.schema.json |
| M5 | `executor.py` | 执行 | 9 种动作 → pyautogui 调用；坐标换算；护栏 | 规范化 Step | L3 ActionResult（契约 §4） | pyautogui、M1 |
| M6 | `prompts.py` | 支撑 | 系统提示词与每步 user 消息模板 | — | 字符串常量 | 无 |
| M7 | `router.py` | 支撑 | 四级能力路由与探测（契约 §6） | 任务描述 | `route: {level, reason, probe_result}` | httpx/subprocess |
| M8 | `errors.py` | 支撑 | 错误码、可重试性、重试/重规划策略（契约 §5） | 异常与检测信号 | 带 `retryable` 的错误对象 | 无 |
| M9 | `loop.py` / `main.py` | 编排 | 主循环、预算、会话状态、日志落盘、CLI | 任务 + 配置 | run 记录、最终 `final` | 全部 |

补充模块（task-5 要求）：`config.py`（配置与默认值）、`backends/`（OpenAI 兼容多模态客户端 + mock）。

### 1.2 依赖方向（必须无环）

```text
main.py ──> loop.py ──┬──> observation.py ──> screenshot.py
                      ├──> agent.py ──────────> backends/ ──> (Ollama / OpenAI 兼容 HTTP)
                      ├──> action_parser.py ──> action.schema.json
                      ├──> executor.py ───────> screenshot.py
                      ├──> router.py
                      ├──> errors.py
                      └──> config.py / prompts.py
```

三条硬约束：

1. **executor 不读模型输出**。它只接受「已经通过 Schema 与语义校验的规范化 Step」。校验是解析层的活；让执行器对模型输出做二次猜测，等于把护栏撕开一个口子。
2. **screenshot 不知道动作的存在**。它只认「截哪块」和「换算到哪」。反向依赖会让差分逻辑钻进截图模块，最后没人能单独测截图。
3. **errors.py 不 import 任何业务模块**。它是被所有人依赖的叶子，一旦它反向依赖 `executor`，错误分类就会被执行细节绑死。

### 1.3 核心模块详述

**M1 截图模块 `screenshot.py`**

- 职责：全屏或局部抓屏；按目标宽度缩放；PNG 编码；算 `sha256`；实现契约 §1.1 的 `image ⇄ screen` 双向换算。
- 输入：`region=`（**image 空间**，可空）或 `full`；目标宽度 `target_width`（默认见 §10 建议值）。
- 输出：`{png_bytes, image:{width,height,scale,origin,sha256}, captured_at}`。
- 不做什么：不做 OCR、不做元素识别、不做画布内容理解（那是模型的事）。
- 关键不变量：`origin` 与 `scale` 必须和这一张 PNG 严格配对返回，任何「先截后改」的路径都可能让两者对不上——这是 CUA 最典型的静默点偏来源。

**M2 观测模块 `observation.py`**

- 职责：把帧 + 上次回执 + 压缩历史 + 预算组装成契约 §1 的 Observation。
- 输出：严格契约 §1 结构；`history_tail` 只放摘要（**不放历史截图**）。
- 附加：`screen_changed/change_ratio` 的计算入口（感知哈希 + 像素差分），供 `flags.popup_suspected` 与 §8 的卡死判定使用。

**M3 视觉推理 Agent `agent.py`**

- 职责：调用多模态模型，把 Observation 变成一段**文本**；负责超时、退避、用量统计。
- 输入：系统提示词（`prompts.py`）、user 消息（Observation 注入）、图片字节。
- 输出：原始文本 + `{model, prompt_tokens, output_tokens, latency_ms}`。
- 边界：**agent 不解析、不执行**。它返回的是「模型说了什么」，不是「要做什么」。这条边界保证「模型话术变了」只影响 M4，「动作语义变了」只影响 M5。
- 备选：模型名、温度、超时都来自 `config.py`，默认 `temperature=0`（同一画面重复采样应尽量稳定）。

**M4 动作解析模块 `action_parser.py`**

- 职责：按契约 §2.1 的顺序宽进提取 JSON（全文直解 → 剥代码围栏 → 平衡括号扫描 → 回喂重试 ≤2 次），再按 `action.schema.json` 校验，再做 Schema 管不到的**语义校验**：
  - 坐标是否落在当前 `image.width/height` 内（越界 → `E_OUT_OF_BOUNDS`，**不 clamp**）；
  - 键名是否在契约 §3.3 白名单内（→ `E_KEY_NOT_ALLOWED`）；
  - 数值区间（`dy`、`duration_ms`、`ms`、`repeat`、`interval_ms`…）；
  - **终态一致性**（契约 §2「终态字段的权威性」v1 裁定）：`action` 永远是必填的动作对象、不可为 `null`；若给的是 `action.type="done"`，其 `status`/`summary` 必须与顶层 `final` 一致，不一致记 `E_SCHEMA`；
  - 小数坐标四舍五入并在 `meta.coord_rounded` 留痕。
- 输出：规范化 Step（含默认值补齐：`button=left`、`clicks=1`、`modifiers=[]`、`unit=line`…）。
- 不做什么：**不猜测模型意图**。语义修正是重规划，不是解析。解析层擅自「帮模型改一个坐标」会让 §8 的失败归因彻底失效。

**M5 键鼠执行模块 `executor.py`**

- 职责：实现 9 种动作；执行契约 §1.1 的坐标换算；执行契约 §8 的七条安全不变量；产出 ActionResult（契约 §4）。
- 关键行为：
  1. 每步之间强制 `sleep ≥ 150ms`（契约 §8.3）。注意 `pyautogui.PAUSE` 实测默认 0.1s，它只作用于**相邻 pyautogui 调用**，不覆盖「动作后到截图前」的那段等待，两者不能互相顶替。
  2. 任何终止路径（正常 / 异常 / Ctrl+C / failsafe）都先对所有按下的修饰键补 `keyUp`（契约 §8.7）。
  3. 越界坐标**直接拒绝**，绝不 clamp（契约 §8.2）。
  4. 执行前后各截一帧算差分，填 `observed.screen_changed / change_ratio`。

### 1.4 支撑模块

| 模块 | 关键点 |
|---|---|
| 编排主循环 `loop.py` | 唯一的状态推进者；同时持有步数预算与墙钟预算（契约 §8.4）；每步产出 Step + ActionResult 并落盘；决定「重试 / 重规划 / 升级 / 终止」。 |
| 观测与历史 `observation.py` | 见 §4、§7；负责历史压缩，防止上下文爆炸。 |
| 护栏与安全 | 破坏性词表拦截（契约 §7）、`risk=high` 放行、危险组合键 allowlist（契约 §8.5）、Failsafe 常开（§8.1）。实现分散在 executor（物理层）与 loop（决策层），**两层都要拦**：executor 负责「不可能被执行」，loop 负责「根本不该发起」。 |
| 日志与回放 | 见 §11；每步落盘 Observation 摘要 + 原图 + 模型原始输出 + ActionResult。 |
| 能力路由 `router.py` | 见 §9；只在任务开始时与「连续失败后」触发，不在每步都探测（探测本身有成本）。 |

### 1.5 反职责（明确不做的事）

- 不做「按控件名点击」：契约 §3 的设计原则是只认坐标，控件定位是模型的职责。
- 不做 `exec/shell/删文件/关进程/注册表`：契约 §3 刻意排除，这类能力走独立白名单通道（契约 §6 L2）。
- 不做验证码识别与绕过：命中即转人工（契约 §3.9 `need_human`、§7 破坏性词表）。
- 不做「无人值守批量 GUI 操作」：v1 的定位是**人在旁边的补盲工具**，不是 RPA 流水线。

---

## 2. 分层架构图

**文字版（可独立读懂）**：最外层是任务与配置（人给的目标 + 预算 + 路由偏好）。往下是编排层，它是唯一有状态的层，负责循环与终止。编排层之下并排三条流水线：感知（截图 → 观测）、决策（提示词 → 模型 → 原始文本）、解析（提取 → Schema → 语义）。解析的产物是规范化动作，交给执行层，执行层把 ActionResult 交回编排层；编排层再驱动感知采下一帧，形成闭环。左侧一列横向贯穿的支撑件（错误策略、护栏、日志回放、能力路由）不属于任何单层，被所有层调用，但它们只能向上返回判定、不能改写流程。

```text
┌──────────────────────────────────────────────────────────────────────┐
│ 调用方：Electron 壳 / Go 后端 / CLI（--task / --serve）               │
└───────────────┬──────────────────────────────────────────────────────┘
                │ 任务描述 + 预算 + 路由偏好
┌───────────────▼──────────────────────────────────────────────────────┐
│ 编排层  loop.py / main.py                                            │
│   SessionState · 双重预算 · 重试/重规划决策 · 落盘                    │
└──┬────────────────┬───────────────────┬──────────────────┬───────────┘
   │ 采集请求        │ 决策请求           │ 校验请求          │ 执行请求
┌──▼─────────┐ ┌────▼──────────────┐ ┌──▼──────────────┐ ┌─▼──────────┐
│ 感知层      │ │ 决策层            │ │ 解析层           │ │ 执行层      │
│ screenshot │ │ prompts.py        │ │ JSON 提取(宽进)  │ │ 坐标换算    │
│ + observ.  │ │ + agent.py        │ │ Schema 校验      │ │ 9 种动作    │
│            │ │ + backends/       │ │ 语义校验+默认值   │ │ 护栏+键释放  │
└──┬─────────┘ └────┬──────────────┘ └──┬──────────────┘ └─┬──────────┘
   │ Observation    │ 模型原始文本        │ 规范化 Step       │ ActionResult
   └────────────────┴────────────────────┴──────────────────┘
                │ 全部经支撑件
┌───────────────▼──────────────────────────────────────────────────────┐
│ 支撑件（横向）：errors.py 错误策略 · router.py 四级路由               │
│               · 护栏（破坏性词表/risk/allowlist）· 日志与回放          │
└──────────────────────────────────────────────────────────────────────┘
                │
        ┌───────▼────────┐        ┌──────────────────────┐
        │ 桌面 / 目标应用 │        │ Ollama / 云多模态 API │
        └────────────────┘        └──────────────────────┘
```

**分层规则（设计如此）**：只允许「上一层的模块调用下一层」，不允许同层横向调用（`action_parser` 不得调用 `executor`）。唯一的例外是支撑件——它们可以被任何层调用，但不能回调业务层。

---

## 3. 端到端时序：截图 → 识别 → 推理 → 动作解析 → 执行 → 再截图

### 3.1 主循环伪代码（设计如此）

```text
state = init(task, budgets)
route = router.decide(task)              # 先决定要不要用 GUI（§9）
observation = capture_and_observe()      # L1
while True:
    raw   = agent.ask(observation)                  # 可能 E_MODEL_* → 重试/上报
    step  = action_parser.parse(raw, observation)   # 可能 E_BAD_JSON/E_SCHEMA/...
    guard.check(step)                               # 破坏性词表 / risk=high / 预算
    result = executor.run(step)                     # L3
    state.record(observation, step, result)         # 落盘
    if step.done: break                             # final: success|failed|need_human
    if state.budget_exhausted(): break              # E_STEP_BUDGET / E_WALL_BUDGET
    if errors.requires_replan(result): state.replan()   # E_STALL / E_LOOP
    observation = capture_and_observe()             # 回到 L1
```

注意：**「识别」不是一个独立步骤**。在本架构里，识别（控件在哪、文字是什么、弹窗该按哪个）全部发生在模型的推理里；本地代码不做元素识别。这正是「只认坐标不认控件名」的必然结果，也是 §9 强调「能走 API 就别走 GUI」的原因——把识别责任交给模型，等于把不确定性交给模型。

### 3.2 单步时序与失败点

| # | 阶段 | 执行者 | 耗时量级 | 可能的失败点 | 对应错误码 |
|---|---|---|---|---|---|
| 1 | 抓屏 | screenshot.py | **实测 57.2 ms**（全屏 2560x1600）/ 36.0 ms（局部 800x500） | 锁屏、会话断开、显示器热插拔导致尺寸变化、远程桌面下拿到黑屏 | `E_INTERNAL`（尺寸异常时会先触发一次重截） |
| 2 | 缩放 + PNG 编码 + sha256 | screenshot.py | **实测 32.5–36.4 ms**（首次同尺寸首次编码 230.7 ms） | DPI 未对齐时图像与坐标空间不一致（见 §6） | 静默风险，不报错 |
| 3 | 组装 Observation | observation.py | `<1 ms`（估算） | `history_tail` 过长撑爆上下文；上一帧几何被误用 | `E_SCHEMA`（下游） |
| 4 | 组装 user 消息 + 历史压缩 | prompts.py | `<5 ms`（估算） | 压缩把关键线索压掉 → 模型重复错动作 | 表现为 `E_LOOP` |
| 5 | 多模态推理 | agent.py / backends | **实测 8.8 – 77.6 s**（qwen2.5vl:7b，见 §10） | 超时 / 4xx / 5xx / 模型拒答 / 输出被截断 | `E_MODEL_TIMEOUT` / `E_MODEL_HTTP` / `E_MODEL_REFUSAL` |
| 6 | JSON 提取（宽进） | action_parser.py | `<1 ms`（估算） | Markdown 围栏（**实测必现**）、前后自然语言、字符串内含花括号、被截断 | `E_BAD_JSON`（回喂重试 ≤2） |
| 7 | Schema + 语义校验 | action_parser.py | `<1 ms`（估算） | 缺字段、未知 `action.type`、参数越界、坐标越界、非法键名 | `E_SCHEMA` / `E_UNKNOWN_ACTION` / `E_ARG_RANGE` / `E_OUT_OF_BOUNDS` / `E_KEY_NOT_ALLOWED` |
| 8 | 护栏 | loop.py + executor.py | `<1 ms` | 可疑弹窗按钮命中破坏性词表；`risk=high` 未放行 | `E_NEED_HUMAN` |
| 9 | 坐标换算 | executor.py | `<1 ms` | 用了错误帧的 `origin/scale`（**最危险：不报错、点偏**） | 无错误码（静默故障） |
| 10 | 执行动作 | executor.py | 协议下限 150 ms 起；输入 2000 字文本按 `interval_ms=0` 计 `~0.2–2 s`（估算） | 焦点丢失、failsafe 触发、老旧控件吞字 | `E_EXECUTOR_FAILSAFE` / `E_NO_FOCUS` |
| 11 | 等待 / expect 轮询 | executor.py | `0 – timeout_ms`（Schema 上限 30000） | 界面慢、弹窗覆盖、动画永不停 | `E_EXPECT_TIMEOUT`（再确认一帧；仍不符 → `E_STALL`） |
| 12 | 再截图 + 差分/哈希 | observation.py | **实测 45.8 ms**（两帧）+ **82.3 ms**（差分）+ 5.05 ms（dHash） | 阈值不当：把光标闪烁当变化、把真实变化当没变 | `E_STALL` / `E_LOOP` |
| 13 | 落盘 | loop.py | `5–20 ms`（估算，含 1 张 PNG ~200 KB） | 磁盘满、权限 | `E_INTERNAL` |
| 14 | 预算检查 → 决策 | loop.py | `<1 ms` | — | `E_STEP_BUDGET` / `E_WALL_BUDGET` |

### 3.3 关于「耗时量级」的结论

单步耗时**几乎完全由第 5 步（模型推理）决定**：本地 7B 视觉模型实测 8.8–77.6 s/步，而全部本地环节（截图 + 编码 + 差分 + 换算 + 落盘）相加只有 **约 0.2–0.45 s/步**（实测分项之和）。因此：

- 优化顺序是 **① 换更快/更小的模型或云端 API ② 降低图像分辨率 ③ 减少步数**；去优化截图或 numpy 差分是白费力气。
- 状态探针（`wait`、`screenshot`）也要走一次模型推理，**等待本身不便宜**：一次 `wait` 的成本约等于 1 步推理。
- 这正是契约 §3.7 把 `wait` 限定在 `100–10000 ms` 且要求模型「只在明知界面在动时才用」的原因。

---

## 4. 数据流：Observation / Step / ActionResult

三层数据的字段定义**以契约 §0–§4 为准**，此处只描述关系与生命周期，不复制字段表。

```text
   ┌──────────────┐   L1 Observation（执行器/截图 → 模型）   ┌──────────────┐
   │  screenshot  │ ─────────────────────────────────────> │  模型推理     │
   │ observation  │  screen + image + last_action_result   │  agent.py     │
   └──────▲───────┘  + history_tail + flags                  └──────┬───────┘
          │                                                         │ L2 Step（模型 → 解析器）
          │                                                         ▼
          │                                                  ┌──────────────┐
          │                                                  │action_parser │
          │                                                  └──────┬───────┘
          │                                                         │ 规范化 Step
          │                                                         ▼
   ┌──────┴───────┐   L3 ActionResult（执行器 → 编排器）      ┌──────────────┐
   │  loop.py     │ <─────────────────────────────────────── │  executor    │
   │ （编排/历史） │   ok / error / observed.screen_changed   └──────────────┘
   └──────────────┘
```

生命周期（设计如此）：

| 数据 | 生产 | 消费 | 存活期 | 落盘 |
|---|---|---|---|---|
| Observation | 每步开始时新建 | 当步的模型调用 | **一步**（下一步重建；不增量修改） | 摘要 + PNG 落盘；PNG 保留 **最近 N=20 步**，更早的压成缩略图（估算值，待实测磁盘） |
| Step | 模型输出 → 规范化 | 执行器 + 护栏 | 一步 | 原样落盘（含模型原始文本） |
| ActionResult | 执行器 | 编排器（填下一步 Observation 的 `last_action_result`）+ 历史压缩 | 一步为「当前回执」，之后进入 `history_tail` 成为压缩摘要 | 原样落盘 |

三条不变量（设计如此）：

1. **跨帧坐标无意义**。动作里的坐标只对「生成它的那一帧的 `image`」成立（契约 §1.1）。loop 在把 Step 交给 executor 前必须确认 `step.image_id == observation.image_id`，不匹配直接拒绝——这一条是防止「异步重排导致点偏」的最后一道锁。
2. **ActionResult 必须回灌**。下一步的 `last_action_result` 不能为空（首步为 `null`）：模型必须知道上一步成功没有，否则它只能靠猜，而「靠猜」正是重复动作与死循环的来源。
3. **历史只给摘要**。完整截图历史绝不回灌（契约 §1 `history_tail`）。

---

## 5. 并发与线程模型

### 5.1 阻塞点

| 环节 | 阻塞性质 | 阻塞时长（实测/估算） | 能否与其它环节并行 |
|---|---|---|---|
| 截图 | 同步 GDI/GPU 调用，**独占桌面** | 36–57 ms | ❌ 与执行互斥 |
| 图像编码/差分 | CPU 密集，可释放 GIL（numpy/PIL） | 30–90 ms | ✅ 可与模型 HTTP 等待并行 |
| 模型调用 | 网络/推理 I/O，**大部分时间在等** | 8.8–77.6 s（实测） | ✅ 唯一值得并行的部分（预热、历史压缩） |
| 键鼠执行 | 同步 Win32 `SendInput`，**改变全局桌面状态** | ≥150 ms/步（协议下限） | ❌ 与截图互斥 |
| 落盘 | 磁盘 I/O | 5–20 ms（估算） | ✅ 可异步队列 |

### 5.2 为什么截图与执行不能并行（这是本节的核心）

1. **唯一桌面、唯一指针**。pyautogui 操作的是进程外的全局状态（光标位置、焦点、按键状态）。截图和执行同时发生，等价于「一边拍照一边改布景」：截到的是半执行的中间态，差分信号随之失真，`E_STALL`/`E_LOOP`` 的判定基础直接崩掉。
2. **中间态会被模型当真**。截图若落在 `mousedown` 与 `mouseup` 之间，模型可能看到「按钮已按下」的悬停态，从而做出与真实状态不符的下一步决策——这类错误不会当场报错，只会在三步之后以「界面不对」的形式爆出来，排障成本最高。
3. **差分信号要求帧间口径一致**。`observed.screen_changed` 是「执行前后两帧」的比较（契约 §4）。如果第三处并发截图插进来，比较就变成「三帧乱序」。
4. **pyautogui/pyscreeze 不是线程安全的**。它们共享模块级状态（`PAUSE`、`FAILSAFE`、内部句柄），并发调用会产生不可复现的偶发失败。

**模型调用是唯一允许并行的部分**：可以在一次推理进行时预热下一个模型客户端、压缩历史、把上一步 PNG 写盘。但必须满足「同一时刻只有一个决策步在飞」，即**步内可并行、步间严格串行**。

### 5.3 线程模型（设计如此）

| 线程 | 职责 | 约束 |
|---|---|---|
| 主线程 | 主循环（截图 → 推理 → 解析 → 执行） | 串行推进 `step`；唯一允许调用 pyautogui 的线程 |
| 模型客户端线程 | HTTP 请求与超时/取消 | 仅做 I/O；不触碰桌面 |
| 落盘线程 | 异步写 PNG 与 jsonl | 只写 `runs/<run_id>/`，不参与决策 |
| 取消/中断线程 | 监听用户中断与 failsafe | 只置标志 + 释放按键，不推进状态 |

**中断安全**（设计如此）：任何线程发现终止条件（用户取消 / failsafe / 预算耗尽）只能置一个原子标志并让主线程在**安全检查点**（每步开始、动作执行前、动作执行后）退出；不允许从其它线程直接调用 pyautogui 去「打断」正在执行的动作——那会留下按下的键。

---

## 6. 坐标与缩放处理

协议 §1.1 已经定死公式，此处**只引用不重写**：

```text
screen_x = origin.x + round(x / scale)      # scale = image.width / 捕获区域宽
screen_y = origin.y + round(y / scale)
```

三条由此推出的硬规则：

1. **模型坐标只属于当前帧**。换帧（包括局部 `screenshot` 之后）意味着 `origin/scale` 全变，旧坐标立即作废。
2. **越界拒绝，不 clamp**（契约 §1.1 / §8.2）。clamp 会把「模型看错了」变成「点在合法但错误的位置」，静默故障比报错贵得多。
3. **`dpi_scale` 只做日志，不参与换算**（契约 §1）。重复乘缩放是经典点偏来源。

### Windows DPI 缩放的坑（本机实测）

本机实测数据（`ctypes` 直接读 Win32 度量）：

| 场景 | `GetSystemMetrics(0/1)` | 说明 |
|---|---|---|
| 进程默认（DPI unaware） | **1707 x 1067** | 逻辑坐标，系统替你做虚拟化缩放 |
| 进程 DPI aware 后 | **2560 x 1600** | 物理像素 |
| `GetDpiForSystem()` / `GetDeviceCaps(LOGPIXELSX)` | **144**（= 150% 缩放） | 1707 × 1.5 ≈ 2560 |
| `import pyautogui` 之前 / 之后 | **1707x1067 → 2560x1600** | **实测：仅仅 import 就翻转了进程 DPI 感知** |
| `pyautogui.size()` / `ImageGrab.grab()` / `pyautogui.screenshot()` | 三者一致，均 2560 x 1600 | 都在同一套物理坐标系下 |

从这个实测能得到四条工程结论：

1. **`import pyautogui` 会改变进程的 DPI 感知状态**（本机实测：1707x1067 → 2560x1600）。因此「截图 API 拿到的尺寸」和「GetSystemMetrics 拿到的尺寸」是否一致，取决于**导入顺序**。这是最容易踩、又最难复现的坑：同一个函数在测试里对、在服务里错，只因为 `import` 顺序不同。
2. **换算公式的分子分母必须来自同一套坐标**。因为 pyautogui 截图与 pyautogui 的点击在同一套（物理）坐标系下，所以 `scale = image.width / 捕获区域宽` 里两个量都必须取自截图结果，**不能**混入 Win32 逻辑度量或 `dpi_scale`。协议 §1 说「重复乘缩放会点偏」，实测含义就是这个。
3. **多显示器/缩放变更要复检**。本机只有 1 块屏（实测 `GetSystemMetrics(80)=1`），但用户的机器可能是 125%/150% 混合缩放的多屏。会话开始时必须把「屏幕几何 + DPI + 显示器数量」写进 `screen`（契约 §1）并落盘；一旦检测到变化（尺寸改变），当前 Step 作废、重截、并记一条日志。多显示器下的行为**待验证**（本机无第二块屏，无法实测）。
4. **不要给 Python 子进程加 DPI 虚拟化**。任何在进程外层做「隐藏 DPI 缩放」的兼容层，都会让截图坐标与点击坐标分属两套空间。保持单进程单口径。

---

## 7. 状态管理

### 7.1 会话状态（设计如此）

```text
SessionState = {
  run_id, task, started_at,
  route:        {level, reason, probe_result},   # §9
  step,         budgets: {steps_left, seconds_left},
  history:      [ 压缩摘要 … ],                   # 喂给模型的
  full_log:     [ 完整 Step/ActionResult … ],     # 只落盘，不回灌
  image_id:     当前 Observation 的帧标识,
  last_stable:  {step, frame_hash, description},  # 回退锚点（E_LOOP 用）
  key_down:     {ctrl, alt, shift, win},          # 执行器维护的真实按住集合
  counters:     {same_action_hash: n, no_change_streak: n, retry: {code: n}},
  terminal:     {final, action_type} | null,      # 终态（契约 §2 v1 裁定）
}
```

状态只活在编排层。感知/执行/解析模块**无状态**（截图模块只依赖入参，执行器只持有 `key_down` 这一份必要的物理状态）。这样任何一步都能从落盘记录重建，回放才有可能（见 §11）。

**终态只有一个权威来源**（契约 §2「终态字段的权威性」v1 裁定，2026-10-11）：`final.status`/`final.summary` 是终态的唯一权威；`action` 永远是必填的动作对象、**任何情况下都不可为 `null`**（含 `done=true`）；推荐收尾写 `action.type="done"`，此时其 `status`/`summary` 必须与 `final` 一致（不一致记 `E_SCHEMA`）；但**不强制** `action.type` 必须是 `done`——`done=true` 搭配任意合法动作 + `final` 同样合法（Schema 不禁止）。编排层因此只在 `terminal` 里记一份 `final`，并在收尾时做一次一致性校验，不必为「`action=null` 该不该收尾」再写一条分支。

### 7.2 历史压缩策略（三级，设计如此）

| 级别 | 触发 | 内容 | 目标 |
|---|---|---|---|
| L-近 | 永远 | 最近 6–10 步：`{step, action, ok, screen_changed}`（契约 §1 `history_tail` 的形式） | 让模型知道自己刚做了什么、成功没有 |
| L-里程碑 | 每 10 步 | 把更早的步骤折叠成「已完成的事实」列表（例：已打开设置页；已输入标题） | 保住任务进度，扔掉过程噪音 |
| L-摘要 | 上下文接近阈值 | 用文本模型（`qwen3:4b` 可作为候选）把里程碑再压成 3–5 句 | 长任务兜底 |

两条禁令：**不压缩未成功的步**（失败的步恰恰是最重要的线索）、**不回灌历史截图**（一张 1472 宽 PNG 实测 234,879 B / 1817 token，灌 10 张就是 1.8 万 token 且毫无收益）。

### 7.3 幂等与去重（设计如此）

- **动作指纹** `fp = normalize(type, 关键参数)`：坐标量化到 8 px 网格、文本取 SHA-256 前 8 位、忽略 `thought`。
- **画面指纹** `fh = dHash(帧)`（实测 5.05 ms，64 bit）。
- 规则：
  1. `(fp, fh)` 连续出现 **3 次** → `E_LOOP`，强制中断当前策略并回退到 `last_stable`（契约 §5）。
  2. `screen_changed=false` 连续 **N=3** 步（且动作非 `wait`/`screenshot`）→ `E_STALL`，触发重规划（契约 §5，阈值 N 待实测标定）。
  3. **执行前短路**：若上一步的 `expect` 已经被观测满足（例如点击后界面已切过去，模型却又发了一次同样的点击），loop 可以不执行该动作而直接进入下一步观测，并在日志记 `skipped_duplicate=true`。这条是防止双重提交（双击变四次点击）的关键。
- **幂等边界（重要）**：CUA 的动作在语义上**不是幂等的**。「发送消息」「提交表单」这类动作重放会产生真实副作用。因此规则 3 只允许跳过「与上一步完全同指纹且画面未变」的动作，**不允许**任何形式的自动重放。

---

## 8. 错误处理机制（专章）

统一入口：所有失败都构造契约 §5 的错误对象（`code / message / retryable / hint`）。**可重试 ≠ 应该重试**：契约 §5 明确区分「同一动作再来一次」（retryable）与「这条路走不通，换策略」（`E_STALL`/`E_LOOP` 强制重规划）。把卡死当超时反复重试，是这类 agent 烧完预算却什么都没干成的最常见原因。

### 8.1 三类故障总表

| 故障类 | 检测信号 | 判定阈值 | 恢复动作 | 升级路径 | 错误码 |
|---|---|---|---|---|---|
| **界面识别失败**（模型没看懂/看错了） | ① 输出无法解析成 JSON；② 解析成功但语义不合法；③ 动作执行成功但预期未达成（`screen_changed=false` 或 `expect` 不符） | ①② 按契约 §5 的重试额度（`E_BAD_JSON`/`E_SCHEMA` ≤2 次，`E_ARG_RANGE`/`E_OUT_OF_BOUNDS`/`E_KEY_NOT_ALLOWED` ≤1 次）；③ 同类失败连续 2 次 | ①回喂解析错误 + 「只输出一个 JSON 对象」；②回喂失败字段与合法区间；③**换观测口径**：先 `move` hover 再截、或对目标区域 `screenshot` 局部放大重截 | 额度用尽 → 记 `E_STALL` 强制重规划；再失败 → `E_NEED_HUMAN` | `E_BAD_JSON` `E_SCHEMA``E_UNKNOWN_ACTION``E_ARG_RANGE``E_OUT_OF_BOUNDS``E_KEY_NOT_ALLOWED``E_NO_FOCUS``E_EXPECT_TIMEOUT` |
| **弹窗干扰** | 连续两帧差分的 bbox：面积占比 **3%–80%**、填充率（变化像素/bbox 面积）**> 0.25**、bbox 不贴满全屏（契约 §7）；附加：中央高对比新块 / 背景压暗 | 上述三个几何条件同时成立 → `flags.popup_suspected=true`；连续 2 步仍疑似 → 升级 | ①把疑似区域**裁剪放大**作为下一步观测主体；②模型选按钮时**默认选非破坏性**（取消/关闭/稍后/X）；③判断不了 → 按一次 `esc`，仍不消失 → 再按一次 `esc` | 两次 `esc` 无效 → `E_NEED_HUMAN`；**命中破坏性词表 → 不点击，直接 `E_NEED_HUMAN`** | `E_POPUP_BLOCKING`（≤2 次）/ `E_NEED_HUMAN` |
| **操作超时** | ① 模型响应超时；② 上游 HTTP 4xx/5xx；③ 等待 `expect` 超时；④ 墙钟预算耗尽 | ① 按客户端超时（建议 60 s，`E_MODEL_TIMEOUT` ≤2 次）；② 5xx 可重试、4xx 不重试；③ `expect.timeout_ms`（Schema 上限 30000）；④ 墙钟预算：云端档默认 300 s、本地 VLM 档建议 ≥1800 s（契约 §8.4 v1 裁定） | ①指数退避重试；②5xx 退避重试、4xx 立即上报；③再截一帧确认，仍不符记 `E_STALL`；④从最近稳定点优雅退出并返回已完成进度 | `E_MODEL_TIMEOUT` 连续失败 → `E_NEED_HUMAN`；`E_WALL_BUDGET`/`E_STEP_BUDGET` → 正常终结并汇报进度 | `E_MODEL_TIMEOUT` `E_MODEL_HTTP` `E_MODEL_REFUSAL` `E_EXPECT_TIMEOUT` `E_STALL` `E_WALL_BUDGET` `E_STEP_BUDGET` |

### 8.2 「界面识别失败」的两个子类要分开处理

- **表达失败**（模型想对了但没写对）：JSON 层或 Schema 层报错。**处理方式是回喂错误信息**，重试额度见契约 §5。这类失败通常是提示词/格式约束问题，不是视觉问题——所以修复动作应该发生在 `prompts.py`，而不是逼模型「再看仔细一点」。
- **认知失败**（模型看错了/想错了）：格式完全合法，动作也执行成功，但界面没往预期方向走。**唯一的检测信号是 `observed.screen_changed` 与 `expect`**（契约 §4/§2）。处理方式是换观测口径（hover 看 tooltip、局部放大、滚动露出更多内容、切窗口），而不是重试同一个动作。
- 实测补充：本地 `qwen2.5vl:7b` 在 3 组不同提示词下都稳定输出 **Markdown 代码围栏**（```json … ````），即使在提示词里明写「不要用 Markdown 代码块」。**结论：解析器的第 2 步（剥围栏）不是可选优化，而是必需路径**；且不得因为「有围栏」判 `E_BAD_JSON`。同时说明：把格式正确性押在提示词上是不可靠的，**解析器才是格式的最终保障**。

### 8.3 「操作超时」的阈值设计理由

- 超时阈值必须**按最慢的合理单步**设，而不是按平均值。本机实测 VLM 单步 8.8–77.6 s（同一模型、不同分辨率/是否驻留），**波动接近 9 倍**。客户端超时若设 30 s，会在最需要它成功的那一步（第一次全屏推理、模型刚卸载后）稳定失败。
- 4xx 与 5xx 必须区别对待（契约 §5）：4xx 多为 key/额度/模型名问题，重试无意义；实测探活：请求不存在的模型，Ollama 返回 **HTTP 404 + `{"error":"model 'no-such-model:latest' not found"}</BT>**——这类错误重试 3 次只是把「配置错」拖成「看起来像网络抖动」。
- 重试必须**指数退避 + 总量上限**，且重试期间不得跨过墙钟预算。

### 8.4 错误码一一对应表（契约 §5 全部 20 个）

| 错误码 | 本架构中的触发位置 | 可重试 | 本架构的默认处置 |
|---|---|---|---|
| `E_BAD_JSON` | `action_parser` 提取阶段 | 是，≤2 | 回喂解析错误 + 要求单 JSON 对象 |
| `E_SCHEMA` | `action_parser` Schema 校验；含终态一致性（`action.type="done"` 的 `status`/`summary` 与 `final` 不一致，契约 §2 v1 裁定） | 是，≤2 | 回喂失败字段名与期望类型；终态不一致时回喂「以 `final` 为准」并要求两者一致 |
| `E_UNKNOWN_ACTION` | `action_parser` 动作分派 | 否 | 回喂 9 种动作清单后重规划 |
| `E_ARG_RANGE` | `action_parser` 区间校验 | 是，≤1 | 回喂合法区间；连续 2 次 → `E_STALL` |
| `E_OUT_OF_BOUNDS` | `action_parser` / `executor`（双保险） | 是，≤1 | 回喂当前 `image.width/height`；**绝不 clamp** |
| `E_KEY_NOT_ALLOWED` | `action_parser` 键名白名单 | 是，≤1 | 回喂白名单子集 |
| `E_NO_FOCUS` | `executor` 输入前焦点检查 | 是，≤1 | 提示模型用 `target` 先点击再输入 |
| `E_MODEL_TIMEOUT` | `agent` HTTP 层 | 是，≤2 | 指数退避；连续失败 → `E_NEED_HUMAN` |
| `E_MODEL_HTTP` | `agent` 状态码判定 | 5xx 是 / 4xx 否 | 5xx 退避重试；4xx 立即上报（key/额度/模型名） |
| `E_MODEL_REFUSAL` | `agent` 响应语义判定 | 否 | 记 `need_human`，换说法反复试探属于禁止行为 |
| `E_EXECUTOR_FAILSAFE` | `executor` 捕获 pyautogui 异常 | 否 | **立即停手** + 释放全部按键 + 交还控制权 |
| `E_EXPECT_TIMEOUT` | `executor` expect 轮询 | 是，≤1 | 再截一帧确认；仍不符 → `E_STALL` |
| `E_POPUP_BLOCKING` | `observation` 弹窗判定 | 是，≤2 | 走 §8.1 弹窗处置链 |
| `E_STALL` | `loop` 连续性判定 | 否 | **强制重规划**：换观测口径/换策略 |
| `E_LOOP` | `loop` 指纹判定 | 否 | 中断当前策略，回退 `last_stable` |
| `E_STEP_BUDGET` | `loop` 预算检查 | 否 | 优雅退出 + 返回已完成进度 |
| `E_WALL_BUDGET` | `loop` 预算检查 | 否 | 同上 |
| `E_CANCEL` | `loop` 中断标志 | 否 | 释放按键后停手 |
| `E_NEED_HUMAN` | 护栏 / 验证码 / UAC / 支付 / 破坏性词表 | 否 | 暂停并明确提示用户接管 |
| `E_INTERNAL` | 任意未预期异常 | 否 | 完整堆栈落盘 + 释放按键 + 停手 |

### 8.5 重试 / 重规划 / 升级 的决策规则（设计如此）

```text
失败发生
 ├─ 错误对象 retryable=true 且该 code 的重试额度未用尽？
 │    ├─ 是 → 回喂 hint + 重试同一动作（同一帧几何，不重新截图）
 │    └─ 否 → 进入下一级
 ├─ 属于「认知失败」（screen_changed=false / expect 不符 / E_STALL / E_LOOP）？
 │    ├─ 是 → 重规划：换观测口径（hover/局部放大/滚动）×1 → 换策略（换入口/换顺序）×1
 │    └─ 否 → 进入下一级
 └─ 升级：E_NEED_HUMAN / E_STEP_BUDGET / E_WALL_BUDGET
      · 到人：需要判断力、凭据、授权、不可逆确认
      · 到预算：带着已完成进度优雅退出，绝不「再试最后一次」
```

**为什么重试时不重新截图**：同一动作重试的前提是「环境没变」。重新截图会让模型看到新帧、生成新坐标，于是「重试」实际变成了「重规划」，两种策略被混在一起后，失败归因就无从谈起（也违反契约 §5 对二者的区分）。

---

## 9. API-first 边界（专章）

契约 §6 的四级路由在这里展开成可执行的决策树。**GUI 视觉操控是最后手段，不是默认手段**——本项目的 CUA 定位是补上 API 覆盖不到的盲区，而不是用鼠标重写一遍 API 调用。

### 9.1 决策树

```text
任务来了
 │
 ├─ 目标对象是浏览器里的网页？ ── 是 ─> 探测 L1（CDP/Playwright 可用？）
 │      ├─ 可用 ──> 走 L1（DOM 定位，不受分辨率影响）
 │      └─ 不可用 ──> 落到 L3
 │
 ├─ 有官方 REST/GraphQL/SDK/CLI？ ── 有 ─> 探测 L0（真的调一次）
 │      ├─ 成功 ──> 走 L0，**并对结果做校验**（L0 也会静默失败）
 │      ├─ 401/403 ──> 接口存在但需要凭据：有凭据→补上重试；无凭据→转人工
 │      └─ DNS/连接失败 ──> 接口不存在 ──> 继续往下
 │
 ├─ 是本地文件/进程层面的确定性操作？ ── 是 ─> 走 L2（**白名单**命令）
 │
 └─ 以上都不行 ──> L3（本协议）：
        目标软件无接口 / 跨应用手工流程 / 一次性低频操作 /
        登录态与验证码天然在 API 之外 / 第三方闭源桌面软件
```

### 9.2 每级判据、探测方法与结果校验

| 级别 | 判据（什么算「有这个能力」） | 探测方法 | 探测结果的解释 | 用后必须做的结果校验 |
|---|---|---|---|---|
| **L0** 结构化接口 | 存在可调用的 REST/GraphQL/SDK/CLI/socket 入口 | 发一次最小合法请求（读接口用 list/get；写接口用 dry-run 或探测权限的只读端点） | **探到 401/403 说明接口存在**（只是缺凭据）；探到 DNS 失败/连接拒绝说明不存在；探到 404 需结合路径再判一次 | 断言返回值包含预期字段；写操作后读回一次确认状态真的变了 |
| **L1** 浏览器 DOM | 目标是网页且浏览器可被 CDP/Playwright 接管 | 尝试建立 CDP/Playwright 连接并 `querySelector` 到目标元素 | 连接成功但元素找不到 ≠ 能力不存在（可能是没登录/没进对页面），要区分「能力缺失」与「状态不对」 | 操作后读取 `textContent/value` 校验，而不是看截图像不像 |
| **L2** OS 命令 | 操作用一条白名单命令能确定性地完成 | 用 `--dry-run` 或只读命令探一次（如 `Test-Path`、`--version`） | 命令不存在/被白名单拒绝 → 不可用 | 命令退出码 + 后置状态断言（文件存在、进程列表变化） |
| **L3** GUI 视觉 | 上述全部不可用，或目标是闭源桌面软件 | 直接进入本协议主循环；**不需要探测**（探不到才是常态） | — | 每一步都靠 `observed.screen_changed` 与 `expect` 校验（契约 §4） |

### 9.3 路由记录

每次路由都要落一条（契约 §6.5）：

```json
{ "level": "L3", "reason": "目标为闭源桌面客户端，L0 无接口（探测 DNS 失败），L1 不适用（非网页）", "probe_result": { "l0": "dns_fail", "l1": "n/a", "l2": "not_applicable" } }
```

路由只在两个时刻触发：**任务开始时**，以及**连续失败后**（`E_STALL` 重规划时考虑「是不是该退到 API 路径」）。不在每步探测——探测本身有网络与状态成本。

### 9.4 一律转人工（不允许 agent 自主决定）

契约 §6.4 定死四类，本架构不再放宽：**资金支付、权限授权、不可逆删除、法律签署**。落地到执行层是两条：
1. 任何命中破坏性词表（契约 §7）的按钮，**不点击**，直接 `E_NEED_HUMAN`；
2. 需要凭据的环节（登录密码、验证码、UAC 提权、支付确认）由 `done.status="need_human"` 交还控制权——**这不是失败，是正确地交还控制权**（契约 §3.9）。

---

## 10. 性能与预算

### 10.1 本机实测分项（2026-10-11，Windows 11 / RTX 5060 Laptop 8 GB / 2560x1600）

| 环节 | 配置 | 实测值 | 备注 |
|---|---|---|---|
| 全屏截图 | 2560x1600 | **57.2 ms** | `pyautogui.screenshot()` |
| 局部截图 | region 800x500 | **36.0 ms** | 局部比全屏略快 |
| 连续两帧截图 | 2560x1600 | **45.8 ms** | 差分需要两帧 |
| numpy 全屏差分（逐通道最大差 > 12） | 2560x1600 int16 | **82.3 ms** | 含 bbox 与变化率计算 |
| dHash（9x8 灰度） | 全屏 | **5.05 ms** | 64 bit |
| 缩放 + PNG 编码 | →1472 宽 | **230.7 ms**（同尺寸首次） | 后续同尺寸 32.5–36.4 ms，首次含编码器初始化 |
| 缩放 + PNG 编码 | →1280 / →896 宽 | **32.5 / 36.4 ms** | — |
| PNG 体积 | 1472x920 | **234,879 B** | 影响上传与落盘 |
| PNG 体积 | 1280x800 | **196,831 B** | — |
| PNG 体积 | 896x560 | **120,503 B** | — |
| 显存占用（推理后） | — | **7.1–7.5 GB / 8.15 GB** | `nvidia-smi`；`ollama ps` 报 `size_vram = 5.22 GB`，即约 0.95 GB 仍在 CPU 侧（部分卸载） |
| 模型加载 | qwen2.5vl:7b | **5.7–9.3 s**（`load_duration`） | 每次未被 keep-alive 命中的调用都要重付 |

### 10.2 VLM 单步推理实测（qwen2.5vl:7b，同一张真实桌面截图，n=1~2）

| 图像宽 | scale（2560→w） | prompt token | prompt_eval | 输出 token | 生成耗时 | **端到端墙钟** |
|---|---|---|---|---|---|---|
| 1472 | 0.575 | 1817 | 52.3–65.3 s | 69 | 2.6–2.7 s | **72.8 / 77.6 s** |
| 1280 | 0.500 | 1406 | 30.8 s | 65 | 1.6 s | **35.9 s**（刚加载后）/ **8.8 s**（已驻留） |
| 896 | 0.350 | 1138 | 22.1 s | 57 | 2.1 s | **24.3 s** |

同一配置两次相差 4 倍（35.9 s vs 8.8 s）——**延迟抖动极大，不是测量误差，而是「模型是否驻留 + 是否部分卸载到 CPU」的真实差异**。工程含义：**任何超时预算都必须按最坏情况设计**，平均值没有意义。

Token 与像素面积的关系**不是线性的**（1472 宽 1817 tok、1280 宽 1406 tok、896 宽 1138 tok）：视觉编码按 patch 网格对齐且有下限，因此**不要用线性公式外推**，要按上表实测选分辨率。

### 10.3 预算估算（标注性质）

| 项 | 值 | 性质 |
|---|---|---|
| 单步本地环节（截图+编码+差分+换算+落盘） | **约 0.2–0.45 s** | 实测分项之和 |
| 单步总耗时（本地 7B VLM） | **约 9–78 s，典型 24–40 s** | 实测区间 |
| 步数预算 40 步（契约 §5，**只对云端 API 档成立**） | 本地模型 40 × 24–78 s = **16–52 分钟** | 由实测推算（估算） |
| 墙钟预算 300 s（契约 §5，**只对云端 API 档成立**） | 本地模型下约 **4–12 步**就触发 `E_WALL_BUDGET` | 由实测推算（估算） |
| 本地档墙钟预算 **≥1800 s**（契约 §8.4 v1 裁定） | 按实测 9–78 s/步，可容纳约 **23–200 步**；若同时受 40 步步数上限约束，最坏约 **52 分钟** | 由实测推算（估算） |
| 系统提示词 token | **实测 3,092 token**（正文 5,955 字符，qwen2.5:7b 分词器，`/api/generate` 的 `prompt_eval_count`） | **实测**；文本前缀 prefill 1,415 ms（约 2.2k tok/s，无图） |
| 每步 prompt token | 图像 1138–1817 + 系统提示词 **3092** + 历史 ~200 ≈ **4.4–5.1k tok/步** | 图像与提示词实测，历史部分估算 |
| 40 步累计输入 token | **约 176–204k tok** | 由实测推算（估算） |
| 每步输出 token | 实测 57–69（未压缩的 `thought` 可能到 `~150`） | 部分实测 |
| 提示词 token 与图像 token 之比 | **3092 : 1817 ≈ 1.7 : 1**（提示词比截图还贵） | 实测 —— 含义：**每步都在重发一遍系统提示词**，长会话应先做前缀缓存或精简提示词（缓存行为**待验证**） |
| Python 进程常驻内存 | `~150–250 MB`（含 Pillow/numpy 与一张 2560x1600 RGB 数组 ≈ 12.3 MB ×N 帧） | 估算（未实测 RSS） |
| 落盘/步 | `~0.2–0.3 MB`（PNG 为主） | 由 PNG 实测推算 |
| 显存 | **7.1–7.5 GB / 8.15 GB**（几乎打满，已部分卸载到 CPU） | 实测 |

**结论（已由 Lead 裁定，契约已更新 2026-10-11）**：契约 §8 第 4 条现已写明——**40 步 / 300 秒的默认值只对云端 API 后端成立**；本地视觉模型实测单步 9–78 秒（qwen2.5vl:7b，2026-10-11 实测），因此本地档墙钟默认值应显著上调（**建议不低于 1800 秒**）、截图长边收到 **896–1280**。两者都是可配置默认值、不属于接口语义，**按后端分档配置，不升契约版本**。落地方案（`config.py` 按后端选档）：

| 后端 | 建议步数预算 | 建议墙钟预算 | 建议图像宽 | 依据 |
|---|---|---|---|---|
| 云多模态 API（低延迟） | 40（契约默认） | 300 s（契约默认） | 1280–1472 | 待实测 |
| 本地 `qwen2.5vl:7b`（本机） | 40 | **≥1800 s**（契约 §8.4 v1 裁定） | **896–1280**（1472 宽实测 73–78 s/步，性价比差） | 实测 |
| 本地 + 只做演示 | 8–12 | 600 s | 896 | 由实测推算（估算） |

**分辨率取舍**：1472 宽 → 896 宽，实测端到端从 72.8–77.6 s 降到 24.3 s（约 3 倍加速），代价是小字更难认。因此推荐**默认 1280 宽，遇到小字/密表格时用契约 §3.8 的 `screenshot.region` 局部放大**——把「高分辨率」花在真正需要的那一小块上，而不是每步都为整屏付费。

---

## 11. 可观测性与回放

### 11.1 每步落盘什么（设计如此）

```text
runs/<run_id>/
  run.json                运行元信息：任务、起止、route、预算、环境基线（屏幕/DPI/显示器数）、模型与版本
  run.jsonl               每步一行（追加写，崩溃也不丢历史）
  route.json              路由决策与探测结果（契约 §6.5）
  steps/0001/
    observation.json      Observation（含 image 几何；image 字段不含 base64）
    frame.png             该步模型真正看到的那张图（唯一的「真相源」）
    model_raw.txt         模型原始输出，**原样**（含围栏、含自然语言）
    step.json             规范化 Step（含补齐后的默认值与 meta.coord_rounded）
    result.json           ActionResult（含 observed.screen_changed / change_ratio / meta.screen_point）
    diff.png              （可选）执行前后差分可视化，用于人工核对 E_STALL/E_POPUP 判定
```

**必须原样落盘「模型原始输出」**：`model_raw.txt` 是区分「模型说错了」与「解析器解析错了」的唯一证据。只落规范化 Step 会把这两类问题永久混在一起。

### 11.2 如何复现一次失败（设计如此，4 步）

1. 打开 `runs/<run_id>/steps/00NN/model_raw.txt`，确认模型到底输出了什么（围栏？截断？）。
2. 用 `frame.png` 重新喂模型（`temperature=0`），比对 `model_raw.txt`：一致 → 问题在解析/执行；不一致 → 问题在采样或模型版本变化。
3. 用 `observation.json` 里的 `image.origin/scale` 与 `result.json` 里的 `meta.screen_point` 手算一次换算，验证是否点偏。
4. 在 dry-run 模式重放：`python -m cua.main --replay <run_id> --from-step NN --dry-run`，逐步对照 `step.json`（**不真按键**）。

### 11.3 脱敏与保留

- PNG 会包含用户屏幕内容（含密码输入框、聊天记录）。默认**只保留最近 N 步的 PNG**（估算 N=20，待按磁盘实测调），且 `runs/` 目录默认不进版本库。
- 会话结束后可一键清理 `runs/<run_id>`；`run.jsonl` 与 `model_raw.txt` 体积小，可长期保留用于回归。

---

## 12. 与 blog-my 的集成点

现状（实测/已核实）：`electron/main.js` 已经用「spawn Go 子进程 → 正则匹配 `READY http://…` → 加载到 BrowserWindow」的模式管理后端生命周期（第 17–73、152 行附近），退出时用 `taskkill /T /F` 清进程树；Go 侧 `main.go` 在 `APP_PORT=0` 时让 OS 分配端口并打印该 READY 行；聊天接口在 `/api` 组下受 `authRequired` 保护；`rag/` 已经通过 `http://127.0.0.1:11434` 调 `bge-m3:latest`（嵌入）与 `qwen2.5vl:7b`（图片描述），模型名可配（`KB_EMBED_MODEL` / `KB_VISION_MODEL`）。

### 12.1 三种集成方案

| 方案 | 形态 | 优点 | 缺点 | 结论 |
|---|---|---|---|---|
| **A. Python 子进程（sidecar）** | Electron 像启动 Go 一样 `spawn(python, ["-m","cua.main","--serve","--port","0"])`，同样 READY 握手；Go 后端把 `/api/cua/*` 转发/代理给它，前端只同源调 Go | ①复用现有生命周期与 READY 解析模式，改动最小；②鉴权仍在 Go（`authRequired`），Python 侧不碰用户库、不碰 cookie；③CUA 崩溃不影响主后端；④Python 侧可独立单测与 dry-run | 多一个进程；Windows 需要进程树清理；需要端口与 token 约定 | **推荐** |
| **B. Go 内嵌调用 Python** | Go 用 `os/exec` 每次起一个一次性 Python 进程 | 不用管端口 | 每步起进程（解释器启动 ~0.1–0.3 s，估算）拖慢循环；状态无法跨步保存；错误传递容易变成字符串拼接 | 不推荐 |
| **C. 纯 CLI 一次性任务** | `python -m cua.main --task "…"`，人在终端看输出 | 最简单，适合验收与演示 | 无 UI 集成、无并发控制、无法从网页端取消 | 仅作为验收/演示形态保留（task-5 已有 `--dry-run --mock --selfcheck`） |

### 12.2 推荐方案的接口与边界（设计如此）

- **控制面**：Python 侧提供最小 HTTP（或 stdio JSON 行）接口：启动任务、查询状态、取消任务、拉取 steps 事件流。Go 后端只做**转发 + 鉴权**，不解析 CUA 语义（保持契约在 Python 侧单一实现）。
- **安全边界**：CUA 子进程**必须**在交互式用户会话中、以当前用户身份运行（它要操作真实桌面）；Electron 的 `sandbox/contextIsolation` 只管渲染进程，与它无关；默认 `--dry-run`，需要真实键鼠时必须显式开启。
- **依赖一致性**：CUA 与 `rag/` 共用同一个 Ollama 服务与模型名配置（`qwen2.5vl:7b`）。两边同时跑视觉推理会抢显存——实测本机在推理后已占用 7.1–7.5 GB / 8.15 GB，**并发的第二路视觉推理会显著变慢甚至失败**（待验证）。故建议：知识库图片入库（`rag`）与 CUA 任务不要同时进行，或在 `config` 里给出互斥开关。
- **与 RAG 的分工**：RAG 提供「领域知识」（要填什么内容），CUA 提供「操作能力」（往哪里填）。二者互补但**不共享进程**：RAG 的产出是文本，CUA 的输入是截图——把知识检索塞进 CUA 主循环会显著推高每步延迟。推荐由调用方先 RAG 出内容，再启动 CUA 任务并把它写进 `task` 描述。

### 12.3 与现有 `/api` 契约的关系

CUA 的新接口**不修改**现有 `docs/api-contract.md` 里的任何既有条目；新增的 `/api/cua/*` 若落地，需要单独增补到该文件（由相应任务的负责人执行，本文档不越界修改）。

---

## 13. 风险、未决问题与对契约的反馈

| # | 项 | 性质 | 说明 / 处置 |
|---|---|---|---|
| R1 | 契约默认墙钟 300 s 与本地 VLM 实测 9–78 s/步曾不自洽（现已裁定） | **已裁定（2026-10-11）** | Lead 裁定：**40 步 / 300 秒只对云端 API 档成立**；本地档墙钟默认值应显著上调（**建议不低于 1800 秒**）、截图长边收到 **896–1280**；二者是可配置默认值、不属接口语义，**按后端分档配置，不升契约版本**。**契约已更新**：§8 第 4 条已补该说明。本文档 §10.3 已同步两档预算表。 |
| R2 | 契约 §2 早期草稿说 `done=true` 时 `action` 可为 `null`，与 `action.schema.json`（`action` 是 9 选 1 的对象、不接受 `null`）冲突，而契约 §9 规定「不一致时以 Schema 为准」 | **已裁定（2026-10-11）** | Lead 裁定「**以 Schema 为准**」：`action` 永远是必填的动作对象、**任何情况下都不可为 `null`**（含 `done=true`）；`final.status`/`final.summary` 是终态**唯一权威来源**；**推荐**收尾写 `action.type="done"` 并同时给顶层 `final`，若给了则其 `status`/`summary` 必须与 `final` 一致（不一致记 `E_SCHEMA`）；**不强制** `action.type` 必须是 `"done"`（`done=true` 搭配任意合法动作 + `final` 同样合法）。**契约已更新**：§2 新增「终态字段的权威性（v1 裁定，2026-10-11）」引用块，`action.schema.json` 的 `action` 与 `final` 已补 description。 |
| R3 | 本地 VLM 定位准确性未经评测 | 待验证 | 本次只验证了**输出格式可用**（能给出合法 JSON），**没有**验证它点得准不准（需要构造带标注的界面截图做坐标误差评估）。 |
| R4 | 多显示器 / 混合 DPI 行为 | 待验证 | 本机仅 1 屏，无法实测。设计上已按契约 §1 的「虚拟桌面合并坐标」处理。 |
| R5 | `E_STALL` 的 N、弹窗 bbox 阈值 3%/80%/0.25 | 部分实测 | 实测样例：光标类分散变化 bbox 面积占比 0.2838（落在 3%–80% 区间内），但**填充率仅 0.001**，远低于 0.25 → 判定为非弹窗。说明「填充率」这一条是**必要的**，否则该帧会被误判成弹窗。样本 n=1，阈值仍需多场景标定。 |
| R6 | 解析器必须容忍 Markdown 围栏 | 实测 | 3 组提示词下模型均输出 ```json` 围栏（含明示禁止的情况）。已在 §8.2 写入处置。 |
| R7 | 提示词工程不能替代解析器 | 设计如此 | 见 R6；格式的最终保障在解析器，不在提示词。 |
| R8 | [02-agent-prompt.md](02-agent-prompt.md) 中 R2 的表述曾停留在「契约内部冲突的处置」 | **已同步（task-12，2026-10-11）** | 02 已按契约 §2「终态字段的权威性（v1 裁定）」改写：文档头、系统提示词 done 条目与输出前自检、示例 3 解说、§4 设计说明第 17 条、§5 契约对照表、英文版提示词共 7 处；同步后旧口径正则残留 0 处。 |

---

## 附录 A. 契约一致性核对表

本文档与 02 提示词中出现的所有取值域，逐项对齐冻结契约：

| 契约条目 | 契约值 | 本文档中的引用 | 是否一致 |
|---|---|---|---|
| 动作类型（§3，9 种，穷举） | `click / type / hotkey / drag / scroll / move / wait / screenshot / done` | §1.3 M5、§3.2、§8.1 | ✅ |
| 坐标换算（§1.1） | `screen = origin + round(image / scale)`，越界拒绝不 clamp | §6（引用不重写）、§1.3 M4/M5 | ✅ |
| 三层数据（§0） | Observation / Step / ActionResult，闭环 L1→L2→L3→L1 | §4 | ✅ |
| 错误码（§5，20 个） | 见 §8.4 全表 | §8.4 一一对应 | ✅ |
| 重试 vs 重规划（§5 末） | 可重试=同一动作再来一次；`E_STALL`/`E_LOOP`=换策略 | §8.5、§8.4 | ✅ |
| 四级路由（§6） | L0 REST/GraphQL/SDK/CLI → L1 DOM/CDP → L2 OS 命令白名单 → L3 GUI 视觉 | §9.1、§9.2 | ✅ |
| 必须转人工（§6.4） | 资金支付、权限授权、不可逆删除、法律签署 | §9.4 | ✅ |
| 弹窗判据（§7） | bbox 面积 3%–80%、填充率 > 0.25、不贴满全屏；`esc`×2；破坏性词表→转人工 | §8.1、R5 | ✅ |
| 安全不变量（§8，7 条） | FAILSAFE 常开、越界拒绝、每步 sleep ≥150 ms、双重预算（40 步 / 300 s **云端档**；本地档按契约 §8.4 v1 裁定分档）、危险组合键默认关、risk=high 放行、终止必释放按键 | §1.3 M5、§5.3、§8、§10.3 | ✅ |
| 解析顺序（§2.1） | 全文直解 → 剥围栏 → 平衡括号扫描 → 回喂重试 ≤2 | §1.3 M4、§3.2、§8.2 | ✅ |
| 字段语义与必填 | `protocol/thought/action/done` 必填；`done=true` 必填 `final`；`action` 永不为 `null`，`final` 是终态唯一权威 | §1.3 M4、§7.1、§13 R2 | ✅（v1 裁定后已与 Schema 对齐，2026-10-11） |
| 版本与扩展（§9） | 未知`字段`忽略、未知 `type` 报错、Schema 优先 | §1.3 M4、R2 | ✅ |

## 附录 B. 本文档实测数据的原始获取方式

| 数据 | 命令 / 方法 |
|---|---|
| 屏幕几何、显示器数、DPI | `ctypes.windll.user32.GetSystemMetrics(0/1/78/79/80)` + `GetDpiForSystem()` + `gdi32.GetDeviceCaps(hdc,88/117/118)`，分别在 `import pyautogui` 前后各测一次 |
| pyautogui 常量 | `pyautogui.PAUSE / FAILSAFE / MINIMUM_DURATION / KEYBOARD_KEYS` |
| 截图、编码、差分、dHash 耗时 | Python 计时脚本，对 `pyautogui.screenshot()` 与 numpy/PIL 逐项 `time.time()`，单次采样 |
| VLM 时延与 token | `POST http://127.0.0.1:11434/api/generate`（`stream=false`, `temperature=0`），读 `load_duration / prompt_eval_count / prompt_eval_duration / eval_count / eval_duration / total_duration` |
| 系统提示词 token | 把 02 文档 §1 的提示词正文原样 POST 到 `/api/generate`（`qwen2.5:7b`, `num_predict=1`），读 `prompt_eval_count` = 3092、`prompt_eval_duration` = 1415 ms |
| 显存与模型驻留 | `nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total` 与 `GET /api/ps` 采样 |
| Ollama 4xx 行为 | 请求不存在的模型 → 实测 `HTTP 404 {"error":"model '…' not found"}` |
| GPU / 内存 / CPU | `nvidia-smi`、`Win32_ComputerSystem`、`Win32_OperatingSystem`、`Win32_Processor` |
| 仓库集成现状 | 直接读 `electron/main.js`、`main.go`、`web/web.go`、`rag/vision.go`、`rag/embed.go`、`package.json`、`scripts/dev.js` |

> 实测数据均为**单次采样**（VLM 部分 n=2），用于确定量级与取舍方向，不作为性能承诺。换机器、换模型、换分辨率都必须重测。
