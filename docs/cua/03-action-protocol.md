# CUA 动作协议 v1（冻结契约）

> **状态：已冻结（frozen interface contract）**。这是 blog-my 项目里视觉 CUA 的**唯一接口契约**：
> 视觉推理 Agent 的产出、动作解析器的输入、键鼠执行器的输入，全部以本文为准。
> 机器可读版本见 [action.schema.json](action.schema.json)（JSON Schema draft 2020-12）。
> **改动规则**：任何字段增删或语义变化都必须由项目负责人确认并升版本号（cua/1 → cua/2），
> 不允许「实现时顺手改一下」——并行开发中，契约漂移是最大的返工来源。

## 0. 三层数据

| 层 | 名字 | 方向 | 谁生产 | 谁消费 |
|---|---|---|---|---|
| L1 | **Observation**（观测） | 执行器 → 模型 | 截图模块 | 视觉推理 Agent |
| L2 | **Step**（决策步） | 模型 → 解析器 | 视觉推理 Agent | 动作解析模块 |
| L3 | **ActionResult**（执行回执） | 执行器 → 编排器 | 键鼠执行模块 | 编排器 / 下一轮 Observation |

闭环：**L1 → L2 → L3 → L1 → …**，直到 L2 里 `done=true` 或触发终止条件。

---

## 1. Observation（模型每步看到的东西）

```json
{
  "protocol": "cua/1",
  "step": 12,
  "task": "把 README.md 的标题改成 Hello",
  "screen": { "index": 0, "width": 1920, "height": 1080, "dpi_scale": 1.0 },
  "image": {
    "width": 1472, "height": 828,
    "scale": 0.7667,
    "origin": { "x": 0, "y": 0 },
    "media_type": "image/png",
    "sha256": "…"
  },
  "last_action_result": { "…": "见第 4 节，第一步为 null" },
  "history_tail": [
    { "step": 10, "action": { "type": "click", "x": 640, "y": 210 }, "ok": true, "screen_changed": true },
    { "step": 11, "action": { "type": "type", "text": "Hello" }, "ok": true, "screen_changed": true }
  ],
  "flags": { "popup_suspected": false, "failsafe_armed": true, "steps_left": 28, "seconds_left": 214.5 },
  "notes": "（可选）编排器给模型的人类提示，例如「上次同类操作失败过一次」"
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `step` | 当前步号，从 1 开始。用于让模型知道自己走到哪、还剩多少预算。 |
| `screen` | 真实屏幕（多显示器时为**虚拟桌面合并坐标系**）的几何信息。`dpi_scale` 是 Windows 缩放（125% → 1.25），**只做日志**，不参与坐标换算——因为 pyautogui 与截图 API 都在同一套坐标下工作，重复乘缩放会点偏。 |
| `image` | 模型实际看到的那张图。**`image.scale` 与 `image.origin` 是坐标换算的唯一依据。** |
| `last_action_result` | 上一步的回执（第 4 节）。模型据此判断「我上一步到底成功没有」，而不是靠猜。 |
| `history_tail` | 最近 6~10 步的压缩历史。**只给压缩摘要**：把完整截图历史喂回去会瞬间烧光上下文。 |
| `flags` | 编排器状态：是否疑似弹窗、failsafe 是否武装、剩余步数与时间预算。 |

### 1.1 坐标空间（最容易出错的地方，单独定死）

- **模型看到的所有坐标都在 `image` 的像素空间里**，左上角为 `(0,0)`，右/下为正。
- 动作里的坐标**永远是当前这一帧 Observation 的 image 空间**，不是屏幕空间，也不是上一帧的。
- 执行器负责换算：

  ```text
  screen_x = origin.x + round(x / scale)
  screen_y = origin.y + round(y / scale)
  ```

- `scale = image.width / 捕获区域宽`。全屏捕获时 `origin = (0,0)`；局部捕获（`screenshot.region`）时
  `origin` = 该区域左上角在虚拟屏幕坐标中的位置。
- **超出 `image` 边界的坐标一律拒绝（`E_OUT_OF_BOUNDS`），绝不 clamp。** clamp 会把一个「模型看错了」的
  错误变成一个「点在了错误但合法的位置」的静默故障，后者更难查。

---

## 2. Step（模型唯一被允许的输出）

```json
{
  "protocol": "cua/1",
  "thought": "地址栏已经在焦点上，直接输入网址",
  "action": { "type": "type", "text": "https://example.com", "submit": true },
  "expect": { "what": "页面开始加载", "timeout_ms": 8000, "must_change": true },
  "risk": "low",
  "done": false,
  "final": null
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `protocol` | 是 | 固定 `"cua/1"`。缺省或不等 → `E_SCHEMA`。 |
| `thought` | 是 | 不超过 1200 字，一句话说清「我看到什么 → 所以做什么」。**强制写出来**是因为它对排障的价值远大于那点 token：出错时能直接看出是看错了还是想错了。 |
| `action` | 是 | 见第 3 节。**必须是一个动作对象，任何情况下都不可为 `null`**（包括 `done=true` 时）。 |
| `expect` | 否 | 预期结果 + 等待上限。给了它，执行器才能区分「操作失败」和「操作成功但界面没变」。 |
| `risk` | 否 | `low` / `medium` / `high`。`high` 默认需要护栏放行（第 8 节）。 |
| `done` | 是 | 任务是否结束。 |
| `final` | `done=true` 时必填 | `{status: success|failed|need_human, summary, answer?}`。**终态的唯一权威来源**。 |

> **终态字段的权威性（v1 裁定，2026-10-11）**：本契约早期草稿写过「`done=true` 时 `action` 可为 `null`」，
> 与 `action.schema.json`（`action` 是 9 选 1 的对象，不接受 `null`）冲突，而第 9 节又规定「不一致时以 Schema 为准」。
> **裁定：以 Schema 为准 —— `action` 永远是必填的动作对象，不可为 `null`。**
> 收尾的推荐写法是 `action.type = "done"`（第 3.9 节）并同时给出顶层 `final`；其中：
>
> - `final.status` / `final.summary` 是**唯一权威**的终态信息；
> - 若同时给了 `action.type="done"`，其 `status` / `summary` 必须与 `final` 一致，不一致记 `E_SCHEMA`；
> - **不强制** `action.type` 必须是 `"done"`：`done=true` 搭配任意合法动作 + `final` 同样合法（Schema 不禁止）。
>
> 理由：让「怎么结束」只有一种检查口径。若允许 `action=null`，解析器就要为「null 该不该触发收尾」多写一条分支，
> 而模型完全可以用合法的 `{"type":"done"}` 表达同一件事——多一条分支只多一处 bug。

### 2.1 解析规则（宽进严出）

模型的输出**不保证是纯 JSON**（可能裹在 Markdown 代码块里，或前后带一句话）。解析器按下面顺序提取，
任何一步失败都记 `E_BAD_JSON`：

1. 直接 `json.loads` 全文；
2. 否则剥离 Markdown 代码围栏后再试；
3. 否则用「第一个 `{` 到与之配对的 `}`」扫描出第一个平衡对象（正确跳过字符串内的花括号与转义）；
4. 都不是 → 回喂错误信息，要求「只输出一个 JSON 对象」，最多重试 2 次。

**宽容点（防御性解析，与 Schema 的严格性不冲突）**：Schema 是给「守规矩的模型」的契约；
解析器对**数值坐标**接受整数或小数，小数四舍五入取整并在 `meta` 里记录 `coord_rounded: true`。
未知字段一律**忽略**（向前兼容），但未知的 `action.type` **一定报错** `E_UNKNOWN_ACTION`。

---

## 3. 动作指令规范（9 种，穷举）

> 设计原则：**只认坐标，不认控件名**。协议里没有「点击叫『确定』的按钮」这种语义动作——
> 控件定位是视觉模型的职责，协议层保持平台无关，换个操作系统或换一套 UI 框架都不用改协议。

### 3.1 click — 单击 / 双击 / 右键

```json
{ "type": "click", "x": 640, "y": 210, "button": "left", "clicks": 1, "modifiers": [] }
```

| 字段 | 必填 | 取值 | 默认 |
|---|---|---|---|
| `x` `y` | 是 | image 空间坐标 | — |
| `button` | 否 | `left` / `right` / `middle` | `left` |
| `clicks` | 否 | `1` / `2`（2 = 双击） | `1` |
| `modifiers` | 否 | `ctrl` / `alt` / `shift` / `win` 的子集，最多 3 个 | `[]` |

### 3.2 type — 输入文本

```json
{ "type": "type", "text": "Hello", "clear_first": false, "submit": false, "interval_ms": 0 }
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `text` | 是 | 不超过 2000 字，允许换行。**不支持键位转义**（`{ENTER}` 之类）——要按键请用 `hotkey`，两条路径混在一起会让注入语义变得不可预测。 |
| `clear_first` | 否 | 先 `ctrl+a` 再输入，默认 `false`。 |
| `submit` | 否 | 输入后按一次 `enter`，默认 `false`。 |
| `target` | 否 | 默认 `"focused"`（直接打字）；给 `{"x":..,"y":..}` 时先点一下再打字，用于「界面上没有现成焦点」的情况。 |
| `interval_ms` | 否 | 逐字间隔，0~500，默认 0。某些老旧控件吞字时才需要放慢。 |

### 3.3 hotkey — 组合键 / 单键

```json
{ "type": "hotkey", "keys": ["ctrl", "shift", "s"], "repeat": 1 }
```

- `keys`：1~3 个键，按顺序同时按下。**单键也走这个类型**（`{"keys":["enter"]}`），不另设 `press`——
  少一个类型就少一处分支 bug。
- 键名白名单：`a-z`、`0-9`、`F1-F12`、`ctrl` `alt` `shift` `win` `cmd`、`enter` `tab` `esc` `space`
  `backspace` `delete` `up` `down` `left` `right` `home` `end` `pageup` `pagedown` `insert`。
  白名单之外 → `E_KEY_NOT_ALLOWED`。
- `repeat`：1~20，默认 1。

### 3.4 drag — 拖拽 / 框选 / 拉滑杆

```json
{ "type": "drag", "from": {"x":100,"y":200}, "to": {"x":800,"y":640}, "duration_ms": 600, "button": "left" }
```

- `duration_ms`：100~5000，默认 600。**必须给时长**：瞬时拖拽在不少 UI 上会被当成点击。
- 可选 `modifiers`，同 `click`。

### 3.5 scroll — 滚动

```json
{ "type": "scroll", "x": 960, "y": 540, "dy": -3, "dx": 0, "unit": "line" }
```

- `x` `y`：**光标锚点，必填**。滚动作用在「指针下方的那个控件」上，不给锚点就没法确定滚哪个区域。
- `dx` `dy`：-20~20，正数 = 向右 / 向下。
- `unit`：`line`（默认）/ `page`。

### 3.6 move — 悬停

```json
{ "type": "move", "x": 300, "y": 400, "duration_ms": 200 }
```

用于触发 tooltip、悬停菜单、hover 态按钮——这类元素的文本只有指针停上去才出现，是**截图里看不到、
必须先 hover 再截一次**才能读到的信息。

### 3.7 wait — 显式等待

```json
{ "type": "wait", "ms": 1500, "reason": "等待页面加载" }
```

`ms`：100~10000。用于「明知界面在动、现在截图只会截到中间态」的场合。

### 3.8 screenshot — 重新截（可只截局部）

```json
{ "type": "screenshot", "region": null }
```

`region` 为 `null` → 全屏重截；给 `{x,y,width,height}`（image 空间）→ 只截该区域并放大，用于
**小字/密表格看不清**时「先放大再决策」。执行后下一次 Observation 的 `image.origin` / `image.scale`
会随之改变，模型的后续坐标自动落在新的局部空间里（换算规则见 1.1），不需要模型自己记偏移。

### 3.9 done — 结束

```json
{ "type": "done", "status": "success", "summary": "已把 README 标题改为 Hello", "artifacts": [] }
```

`status`：`success` / `failed` / `need_human`。`need_human` 用于验证码、密码、UAC 提权、支付确认等
**必须由人来做**的环节——这不是失败，是正确地交还控制权。

> **协议刻意不定义**：`exec` / `shell` / 删文件 / 关进程 / 注册表。这些属于「L2 系统命令」，
> 走独立的**白名单**通道（见第 6 节），不进本协议。一个能被截图内容影响的模型不应该同时握着
> 任意命令执行权。

---

## 4. ActionResult（执行回执）

```json
{
  "ok": true,
  "step": 12,
  "action": { "type": "click", "x": 640, "y": 210 },
  "started_at": 1730000000.123,
  "duration_ms": 143,
  "error": null,
  "observed": { "screen_changed": true, "change_ratio": 0.037, "cursor": { "x": 835, "y": 274 } },
  "meta": { "screen_point": { "x": 835, "y": 274 }, "coord_rounded": false }
}
```

失败时：

```json
{
  "ok": false,
  "error": { "code": "E_OUT_OF_BOUNDS", "message": "x=1600 超出图像宽度 1472", "retryable": false, "hint": "坐标必须落在 image.width/image.height 内" }
}
```

`observed.screen_changed` 由「执行前后两帧缩略图的感知哈希差」得出，是判断「这一下到底有没有生效」的
主要客观依据。`change_ratio` = 变化像素占比。

---

## 5. 错误码表（冻结）

| 错误码 | 触发条件 | 可重试 | 默认处置 |
|---|---|---|---|
| `E_BAD_JSON` | 模型输出无法解析成 JSON | 是，最多 2 次 | 回喂解析错误 + 要求「只输出一个 JSON 对象」 |
| `E_SCHEMA` | JSON 不符合本协议 | 是，最多 2 次 | 回喂 schema 校验失败字段 |
| `E_UNKNOWN_ACTION` | `action.type` 未注册 | 否 | 回喂支持的动作类型清单 |
| `E_ARG_RANGE` | 参数越界（如 `dy=99`） | 是，最多 1 次 | 回喂合法区间 |
| `E_OUT_OF_BOUNDS` | 坐标超出当前 image | 是，最多 1 次 | 回喂当前 `image.width/height` |
| `E_KEY_NOT_ALLOWED` | 键名不在白名单 | 是，最多 1 次 | 回喂白名单 |
| `E_NO_FOCUS` | 要打字但目标不可编辑 | 是，最多 1 次 | 先点 `target` 再输入 |
| `E_MODEL_TIMEOUT` | 模型响应超时 | 是，最多 2 次 | 指数退避重试；连续失败 → `E_NEED_HUMAN` |
| `E_MODEL_HTTP` | 上游 4xx / 5xx | 5xx 是 / 4xx 否 | 4xx 多为 key、额度或模型名问题，重试无意义，直接上报 |
| `E_MODEL_REFUSAL` | 模型拒绝执行 | 否 | 记 `need_human`，不换个说法反复试探 |
| `E_MODEL_EMPTY` | 模型返回空 `content`（如 token 预算被内部 reasoning 吃光） | 否 | **不得**当成合法回复送进动作解析器；直接报人工——重试同一请求无意义，应先修 token 预算 |
| `E_EXECUTOR_FAILSAFE` | 鼠标被甩到左上角 | 否 | **立即停手**，释放所有按键，交还控制权 |
| `E_EXPECT_TIMEOUT` | 等待 `expect` 超时 | 是，最多 1 次 | 再截一帧确认；仍不符预期记 `E_STALL` |
| `E_POPUP_BLOCKING` | 检测到疑似弹窗遮挡 | 是，最多 2 次 | 走第 7 节弹窗策略 |
| `E_STALL` | 连续 N 步画面几乎无变化 | 否 | 判定「卡住了」，强制**重规划**（换策略，而不是重试同一动作） |
| `E_LOOP` | 同一（动作，画面哈希）重复 3 次以上 | 否 | 强制中断当前策略，回退到上一个稳定状态 |
| `E_STEP_BUDGET` | 超出步数预算（默认 40） | 否 | 优雅退出，返回已完成的进度 |
| `E_WALL_BUDGET` | 超出时间预算（默认 300s） | 否 | 同上 |
| `E_CANCEL` | 用户手动中断 | 否 | 释放按键、停手 |
| `E_NEED_HUMAN` | 需人工介入（验证码 / 密码 / UAC / 支付） | 否 | 暂停并提示用户接管 |
| `E_INTERNAL` | 未预期异常 | 否 | 记录完整堆栈，停手 |

> **v1 修订（2026-10-11，Lead 裁定）**：新增第 21 个错误码 `E_MODEL_EMPTY`。
> 起因：接入真实云端视觉模型后实测到——同一张图同一问题，`max_tokens=80` 返回空字符串、
> `max_tokens=4000` 正常（单次 completion_tokens 实测 163/532/2189，说明模型内部有 reasoning 消耗预算）。
> 空 content 若流进解析器，现场会表现为「模型返回结构异常」（`E_INTERNAL`），把真正成因掩盖掉。
> 故单列一码、**不可重试**（重试同一请求无意义），并同步把云端 `max_tokens` 抬高。
> 本表由 20 条增至 21 条；`action.schema.json` 不枚举错误码，无需同步。
>
> **v1 二次修订（2026-10-11，Lead 实测）**：下限由 4096 上调为 **16384**。
> 起因：真实端到端跑 deepseek-flash 时，run 在 **step 12 死于 `E_MODEL_EMPTY`**——而轨迹显示它当时
> 正在正常推进（记事本已打开、文字已输入）。高保真复测（CUA 真实系统提示词 + 真实 1472×920 截图，
> prompt=5299 tok）：**同一 prompt 三次调用 completion_tokens = 414 / 195 / 1339，波动极大**，
> 4096 在长推理步会被吃光。实测 16384 下同一请求正常返回（completion=1339，content 非空）。
> 二次修订后真实端到端 **28 步 done**，全程未再出现 `E_MODEL_EMPTY`。

**重试 vs 重规划的区别（关键）**：可重试错误（`retryable=true`）是「同一动作再来一次」；
`E_STALL` / `E_LOOP` 是**语义上认定这条路走不通**，必须换策略重规划。把卡住当成超时去反复重试，
是这类 agent 烧完预算还什么都没干成的最常见原因。

---

## 6. 边界：优先 API，没有 API 才用 GUI 视觉操控

四级能力路由，**从上往下命中第一个可用的就停**：

| 级别 | 手段 | 典型例子 | 成本 / 稳定性 |
|---|---|---|---|
| **L0** | 结构化接口：REST / GraphQL / SDK / CLI / 本地 socket | 调 GitHub API 建 issue；`git` 命令 | 最快最稳，首选 |
| **L1** | 浏览器 DOM 层：CDP / Playwright（本仓库的 agent-browser、browser skill） | 网页表单填写、抓取 | 稳，不受分辨率影响 |
| **L2** | 操作系统命令（**白名单**约束） | 打开文件、复制目录 | 快，但权限语义要管住 |
| **L3** | **GUI 视觉操控**（本协议） | 第三方闭源桌面软件、只有图形界面的老旧内部系统 | 最慢最脆，**兜底** |

判定规则：

1. **有能力探测（capability probe）**：不是「文档说有 API」就算数，要实际探一次（探到 401 也说明接口存在；
   探到 DNS 失败说明不存在）。探测结果要落日志。
2. **用了 API 也要校验结果**，不能因为「走的是 API」就跳过验证——L0 也会静默失败。
3. **必须走 GUI 的情形**：目标软件完全没有可编程接口；跨应用的手工流程；一次性或低频操作
   （为它写 API 集成的成本远高于点几下）；登录态与验证码天然在 API 之外；操作对象是第三方闭源桌面软件。
4. **一律转人工**（不允许 agent 自主决定）：涉及资金支付、权限授权、不可逆删除、法律签署。
5. 每次路由都要记录 `route: {level, reason, probe_result}`，让「为什么用了慢路径」事后可审计。

> 一句话：**GUI 视觉操控是最后手段，不是默认手段。** 本项目的 CUA 定位是「补上 API 覆盖不到的盲区」，
> 而不是「用鼠标重写一遍 API 调用」。

---

## 7. 弹窗干扰策略

**检测口径**（不追求「认出这是弹窗」，而是识别「画面结构发生了模态级突变」）：

1. 对连续两帧做差分，得到变化区域的外接矩形 bbox；
2. 若 bbox 面积占画面 **3%~80%**、且其形状接近矩形（填充率 = 变化像素 / bbox 面积，大于 0.25，
   且 bbox 不贴满全屏）→ 标记 `flags.popup_suspected = true`；
3. 附加信号：画面中央区域出现高对比度新块、背景区域被压暗（模态框的典型视觉特征）。

**处置优先级**：

1. 把疑似弹窗区域**单独裁剪放大**，作为下一步 Observation 的主体，让模型看清按钮文字；
2. 模型选择按钮，但**默认选非破坏性那个**（取消 / 关闭 / 稍后 / Cancel / Close / X）；
3. 判断不了 → 按一次 `esc`；仍不消失 → 再试一次 `esc`；再无效 → `E_NEED_HUMAN`；
4. **硬约束**：可疑弹窗按钮文字若命中破坏性词表 → **不点击，直接 `E_NEED_HUMAN`**。

破坏性词表（命中即转人工，中英文均含）：

> 删除、卸载、格式化、清空、重置、覆盖、支付、付款、确认购买、允许、授权、安装、同意、
> Delete、Uninstall、Format、Erase、Reset、Overwrite、Pay、Purchase、Allow、Grant、Install、Agree

**为什么这么保守**：在这类系统里，「误点一个按钮」的期望损失远大于「少点一个按钮」。宁可停下来问人。

---

## 8. 安全不变量（执行器必须无条件成立）

1. `pyautogui.FAILSAFE = True` **永久开启**，且不作为可配置项暴露给模型。
2. 坐标越界一律拒绝（`E_OUT_OF_BOUNDS`），**不 clamp、不取模、不翻转到另一块屏**。
3. 每步之间强制 `sleep` 至少 150ms，防止把 UI 事件队列打爆导致更诡异的失败。
4. 步数预算（默认 40）与墙钟预算（默认 300s）**双重**约束，任一超限即终止。**这两个默认值只对云端 API 后端成立**：本地视觉模型实测单步 9–78 秒（qwen2.5vl:7b，2026-10-11 实测，含首次加载与长短 prompt 的抖动），300 秒只够 4–12 步。本地后端的墙钟默认值应显著上调（建议不低于 1800 秒），并把截图长边收到 896–1280 以压住单步延迟。二者都是可配置默认值、不属于接口语义，按后端分档配置即可。
5. 危险组合键（含 `win` 的系统级组合、`alt+f4`、`ctrl+alt+delete`）默认关闭，需显式 allowlist 打开。
6. `risk=high` 的动作默认**需要护栏放行**；放行策略由调用方配置，协议层只负责标记。
7. 因任何错误终止时，必须先**释放所有仍按下的键**（对所有修饰键补 `keyUp`），否则 Windows 上会留下
   一个「Ctrl 卡住」的系统，接下来用户的所有点击都变成多选——这是这类 agent 最恶心的事故模式。

---

## 9. 版本与扩展

- 当前版本 `cua/1`。
- 新增动作类型：要么升到 `cua/2`，要么用 `x-` 前缀的实验动作，且必须在配置里显式打开才生效。
- 未知**字段**忽略（向前兼容）；未知 `type` 报错（向后不兼容）。
- 本协议与机器可读 Schema 必须同步改，两者不一致时**以 Schema 为准**（因为它是可执行校验的）。

---

## 10. 与 blog-my 现有代码的对应关系

| 协议元素 | 实现位置 |
|---|---|
| Observation 组装 | `cua/observation.py` |
| 截图与坐标换算 | `cua/screenshot.py` |
| 视觉推理 Agent（提示词 + 模型调用） | `cua/agent.py` + [02-agent-prompt.md](02-agent-prompt.md) |
| Step 解析 / Schema 校验 | `cua/action_parser.py` |
| 动作到键鼠执行 | `cua/executor.py` |
| 错误码与重试、重规划策略 | `cua/errors.py` |
| API-first 路由 | `cua/router.py` |
| 主循环与预算 | `cua/loop.py`、`cua/main.py` |
