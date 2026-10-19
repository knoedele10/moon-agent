# docs/cua/ 文档索引

本目录是 blog-my 项目**视觉 CUA（Computer-Use Agent）**子系统的文档入口。
CUA 的定位是"补上 API 覆盖不到的盲区"：能调 API / 浏览器 DOM / 白名单命令的地方优先走那些通道，
只有面对第三方闭源桌面软件、老旧内部系统这类**只有像素没有接口**的场景，才动用截图—视觉推理—键鼠执行。

> 本页已于 **2026-10-11 按最终状态回填**（task-10）。此前一版写于代码尚未落地时，其中「撰写中 / 未落地」的标注已作废，以本页为准。

> **可信度标记约定**（全目录统一）：
> | 标记 | 含义 |
> |---|---|
> | ✅ | 已实测：有可复现证据（命令输出 / 日志 / 单测 / 截图） |
> | 🔶 | 设计如此：契约、代码或文档已这样定义，端到端效果未必验证过 |
> | ⏳ | 待验证：尚未跑通或尚未取得证据 |
> | 📐 | 估算：量级推算，非实测，不得当指标引用 |

---

## 从哪读起（推荐顺序）

| 你的目的 | 先读 | 再读 |
|---|---|---|
| 5 分钟了解这个项目 | [05-project-intro.md](05-project-intro.md) | — |
| 要写 / 改 CUA 实现 | [03-action-protocol.md](03-action-protocol.md)（**冻结契约，必读**） | [action.schema.json](action.schema.json) → [01-architecture.md](01-architecture.md) → [../../cua/](../../cua/) 代码 |
| 要改视觉推理提示词 | [02-agent-prompt.md](02-agent-prompt.md) | [03-action-protocol.md](03-action-protocol.md) 第 2、3 节 |
| 要了解 CUA 自身怎么切模块 | [01-architecture.md](01-architecture.md)（§1 模块总表、§12 集成点） | [05-project-intro.md](05-project-intro.md) 第 5、9 节 |
| **要接 / 改集成层**（Electron ↔ Go ↔ Python） | [06-integration-contract.md](06-integration-contract.md)（**冻结契约**） | [04-integration.md](04-integration.md) → [../api-contract.md](../api-contract.md) 第 10 节 |
| 要跑离线自检 / 契约测试 | 下文「四、环境与运行」 | [../../cua/README.md](../../cua/README.md) |
| 要看验收证据 | [../day23/notes.md](../day23/notes.md) | [../day23/verify-log.txt](../day23/verify-log.txt) → `../verify/`（独立复验日志） |

一句话顺序：**05（是什么）→ 03（动作契约是什么）→ 01（怎么切模块）→ 02（提示词怎么写）→ action.schema.json（机器校验）→ cua/ 代码与测试**；要把两端接起来，再读 **06（集成契约）→ 04（集成实现）**。

---

## 一、docs/cua/ 文件索引

| 文件 | 行数 | 状态 | 一句话说明 |
|---|---|---|---|
| [01-architecture.md](01-architecture.md) | 675 | ✅ 已就绪（cua-arch） | 系统架构 v1：模块总表与依赖方向、分层图、端到端时序、三层数据流、并发与线程模型、坐标与 Windows DPI、状态与历史压缩、错误处理专章（20 个错误码一一对应）、API-first 边界、性能与预算（本机实测分项）、与 blog-my 的三种集成方案、风险与对契约的反馈 R1–R8；附录 A 为契约一致性核对表 |
| [02-agent-prompt.md](02-agent-prompt.md) | 534 | ✅ 已就绪（cua-arch） | 视觉推理 Agent 系统提示词 v1：可直接复制的提示词正文、每步 user 消息模板与注入规则、3 个 few-shot（正常一步 / `E_NO_FOCUS` 恢复 / 破坏性词表转人工）、逐条设计说明、与契约的逐项对照、未验证与已知限制、英文版提示词、与实现文件的对应关系 |
| [03-action-protocol.md](03-action-protocol.md) | 392 | ✅ **已冻结**（v `cua/1`） | 三层数据（Observation / Step / ActionResult）、9 种动作、**21 条错误码**（v1 新增 `E_MODEL_EMPTY`，不可重试）、四级能力路由、弹窗策略、7 条安全不变量；**动作层唯一接口契约**。经 Lead v1 裁定修订（§2 终态字段、§8 预算分档），见下方说明 |
| [action.schema.json](action.schema.json) | 447 | ✅ **已冻结** | 上述协议的 JSON Schema（draft 2020-12）机器可读版本；两者冲突时**以 Schema 为准**。v1 裁定后 `action` 与 `final` 已补 description |
| [04-integration.md](04-integration.md) | 452 | ✅ 已就绪（cua-doc） | **集成实现说明**：架构总览（ASCII）、I1 事件流（`--events stdout`、12 个 kind、`frame_image`、UTF-8 落字节修复与归因更正）、五个 HTTP 接口逐字段、模型代理与测试缝、前端 tab、启动方式（开发态 ✅ / 打包态 ⏳）、事件内存上限、已知限制 L1–L8、本次验收证据 |
| [05-project-intro.md](05-project-intro.md) | 441 | ✅ 已就绪（cua-doc） | 大创申报材料：项目名称建议、简介、背景痛点、技术原理、模块概览、创新点、可行性、应用场景、进度、指标（已达成 / 待验证分开）、伦理安全；已按最终状态回填（RAG 142 PASS / 0 FAIL、CUA 12 模块等） |
| [06-integration-contract.md](06-integration-contract.md) | 191 | ✅ **已冻结**（集成契约 v1，Lead 拥有） | 把 Electron 壳 / Go 后端 / Python CUA **装成一个应用**的契约：I1 事件流、I2 `/api/cua/*`、I3 模型代理、I4 前端 tab、I5 打包；含 §2.3 内存上限、§3.1 max_tokens 与空回复、§3.2 视觉模型可用性、§3.3 测试缝、§3.4 多模态修复五处补充 |

> **契约 v1 裁定（2026-10-11，版本仍为 `cua/1`，不升版）**：本次协作中冻结契约被修订两处，均在文档内留痕：
> 1. **§2 新增「终态字段的权威性（v1 裁定，2026-10-11）」** —— `action` 永远是必填的动作对象、**任何情况下都不可为 `null`**（含 `done=true`）；`final.status` / `final.summary` 是终态**唯一权威**；**推荐**收尾写 `action.type="done"` 并同时给顶层 `final`，若给了则二者 `status`/`summary` 必须一致（不一致记 `E_SCHEMA`）；**不强制** `action.type` 必须为 `"done"`。
> 2. **§8 第 4 条补预算分档说明** —— 默认「40 步 / 300 秒」**只对云端 API 档成立**；本地视觉模型实测单步 9–78 秒（`qwen2.5vl:7b`，2026-10-11 实测），本地档墙钟默认值**建议不低于 1800 秒**、截图长边收到 **896–1280**。二者都是可配置默认值、不属于接口语义，按后端分档配置即可。
>
> 裁定依据与影响见 [01-architecture.md](01-architecture.md) §13 的 **R1 / R2**，以及 §10.3 的两档预算表；02 文档也已按裁定同步（同文件 R8）。

> 编号已补齐：04（集成实现）与 06（集成契约）均已落地，不再有空槽。

---

## 二、cua/ 代码索引（2026-10-11 最终状态）

`cua/` 是 Python 侧的 CUA 运行时，**只实现协议、不改协议**；契约有歧义时按协议第 9 节「以 Schema 为准」。
集成层代码（Go 侧网关与前端 tab）不在 `cua/` 里，见下文 **2.4**。
下表行数为**含空行的实际行数**（与文件当前内容一致）。除另有标注外，状态均为 ✅ 已落地。

### 2.1 核心模块

| 文件 | 行数 | 职责 |
|---|---|---|
| [../../cua/__init__.py](../../cua/__init__.py) | 15 | 包入口：版本号与 `PROTOCOL_VERSION = "cua/1"`；不做任何副作用（不导入 pyautogui、不截屏、不动鼠标） |
| [../../cua/config.py](../../cua/config.py) | 353 | 运行配置与冻结常量：键名白名单（契约 §3.3）、预算分档（cloud / local / auto，契约 §8 第 4 条 v1 裁定）、破坏性词表（§7）、安全不变量开关（§8）、事件流开关与截图参数（`event_image_max_edge = 640` / `event_image_quality = 60`，集成契约 §1）、**输入层开关 `type_use_clipboard_for_unicode`**（task-25：非空文本一律走剪贴板，ASCII 也是；置 False 才退回逐字符 `write`） |
| [../../cua/errors.py](../../cua/errors.py) | 244 | **21 条**冻结错误码（含 `E_MODEL_EMPTY`：空 content 不可重试）、`CuaError`、**重试 vs 重规划**策略分离（`E_STALL` / `E_LOOP` 必须换策略而非重试） |
| [../../cua/screenshot.py](../../cua/screenshot.py) | 667 | 截图、image↔screen 坐标换算（越界拒绝、**不 clamp**）、前后帧差分（`change_ratio`）、弹窗启发式；含完全离线的 `MockScreenshot` |
| [../../cua/action_parser.py](../../cua/action_parser.py) | 596 | 契约 §2「宽进严出」：三级降级提取 JSON（全文直解 → 剥 Markdown 围栏 → 平衡花括号扫描）+ 自研 Schema 校验子集（不引入新依赖）、坐标小数归一化与 `coord_rounded` 标记 |
| [../../cua/observation.py](../../cua/observation.py) | 86 | Observation 组装：截图、屏幕几何、上一步回执、压缩历史、剩余预算 |
| [../../cua/prompts.py](../../cua/prompts.py) | 142 | 系统提示词常量，来源为 [02-agent-prompt.md](02-agent-prompt.md) 第 1 节 |
| [../../cua/agent.py](../../cua/agent.py) | 93 | 视觉推理 Agent：把 Observation 交给后端模型、取回 Step |
| [../../cua/executor.py](../../cua/executor.py) | 715 | 9 种动作到键鼠的映射、执行回执、`DryRunDevice`、破坏性点击守卫、终止时释放所有按键；task-25：`type` 走**剪贴板主路径**（不可用时退化为逐字符 `write`） |
| [../../cua/router.py](../../cua/router.py) | 194 | 四级能力路由（L0 API → L3 GUI 视觉）与 `route` 审计记录 |
| [../../cua/loop.py](../../cua/loop.py) | 784 | 主循环：双预算、停滞 / 循环判定、取消与优雅退出；含 **stdout 事件流**（`--events stdout` 的 `EVENT_STREAM_KINDS` 白名单与 `EventStreamLogger`，集成契约 §1） |
| [../../cua/main.py](../../cua/main.py) | 1748 | CLI 入口：`--task / --dry-run / --mock / --script / --selfcheck / --budget-profile / --events stdout / --events-images`；`_force_utf8_stdio()` 在写首个事件前把 stdout/stderr 钉成 UTF-8 |

### 2.2 模型后端与支撑文件

| 文件 | 行数 | 职责 |
|---|---|---|
| [../../cua/backends/base.py](../../cua/backends/base.py) | 18 | 后端接口定义（模型调用的统一抽象） |
| [../../cua/backends/mock.py](../../cua/backends/mock.py) | 60 | 脚本化假模型：离线跑全链路，不需要模型服务 |
| [../../cua/backends/openai_compat.py](../../cua/backends/openai_compat.py) | 220 | OpenAI 兼容 HTTP 后端（本机 Ollama 也走这条）；含 `require_content` / `status_error_code` —— 空 content → `E_MODEL_EMPTY`、上游状态码 → `E_MODEL_HTTP`（verifier 的 `it_model_empty.py` 只调这两个入口） |
| [../../cua/backends/__init__.py](../../cua/backends/__init__.py) | 16 | 后端注册 |
| [../../cua/README.md](../../cua/README.md) | 176 | 使用说明：三条离线命令、接真实模型、参数表（含 `--budget-profile` 三档） |
| [../../cua/SELFCHECK.txt](../../cua/SELFCHECK.txt) | 183 | 自检输出留档（文字记录，非实时；本人已于 2026-10-11 复跑，见第四节） |
| [../../cua/requirements.txt](../../cua/requirements.txt) | 10 | Python 依赖清单 |
| [../../cua/mock/demo.json](../../cua/mock/demo.json) | 6 | `--mock --script` 用的脚本样例 |

### 2.3 契约测试（verifier 独立编写，标准库 unittest；本仓库未装 pytest）

| 文件 | 行数 | 职责 |
|---|---|---|
| [../../cua/tests/run_tests.py](../../cua/tests/run_tests.py) | 64 | 套件入口；退出码 0 = 全通过，1 = 有失败，2 = 无失败但有 skip（有未验证项） |
| [../../cua/tests/ct_adapter.py](../../cua/tests/ct_adapter.py) | 1226 | 实现适配层：把实现的能力入口解析成统一接口，找不到就记 skip 并附原因 |
| [../../cua/tests/ct_oracle.py](../../cua/tests/ct_oracle.py) | 501 | 契约 Oracle：从契约 / Schema 推出期望值，与实现行为对照 |
| [../../cua/tests/ct_fake_pyautogui.py](../../cua/tests/ct_fake_pyautogui.py) | 286 | 假 pyautogui：拦截全部键鼠调用，测试不碰真实设备 |
| [../../cua/tests/ct_negative_control.py](../../cua/tests/ct_negative_control.py) | 248 | 负向对照：验证测试本身具备失败能力（防止"永远通过"的假绿） |
| [../../cua/tests/ct_cli_probe.py](../../cua/tests/ct_cli_probe.py) | 47 | CLI 能力探测（子进程探 `--help` 暴露的开关） |
| [../../cua/tests/ct_test_00_capability.py](../../cua/tests/ct_test_00_capability.py) | 34 | 能力表：实现是否提供契约要求的入口 |
| [../../cua/tests/ct_test_01_json.py](../../cua/tests/ct_test_01_json.py) | 150 | 三级降级 JSON 提取（含 Markdown 围栏、字符串内花括号） |
| [../../cua/tests/ct_test_02_schema.py](../../cua/tests/ct_test_02_schema.py) | 261 | Step / Schema 校验与错误码映射 |
| [../../cua/tests/ct_test_03_coords.py](../../cua/tests/ct_test_03_coords.py) | 126 | 坐标换算（scale / origin / 四舍五入） |
| [../../cua/tests/ct_test_04_bounds.py](../../cua/tests/ct_test_04_bounds.py) | 183 | 越界判定：必须拒绝、不得 clamp |
| [../../cua/tests/ct_test_05_keys.py](../../cua/tests/ct_test_05_keys.py) | 125 | 键名白名单 |
| [../../cua/tests/ct_test_06_errors.py](../../cua/tests/ct_test_06_errors.py) | 162 | 错误码与重试 / 重规划策略 |
| [../../cua/tests/ct_test_07_destructive.py](../../cua/tests/ct_test_07_destructive.py) | 121 | 破坏性词表与点击守卫 |
| [../../cua/tests/ct_test_08_modifier_release.py](../../cua/tests/ct_test_08_modifier_release.py) | 97 | 终止时释放修饰键、`FAILSAFE=True` 常开 |
| [../../cua/tests/ct_test_09_dryrun_mock.py](../../cua/tests/ct_test_09_dryrun_mock.py) | 95 | dry-run 与 mock 路径不产生键鼠调用 |
| [../../cua/tests/README.md](../../cua/tests/README.md) | 100 | 套件自述：文件结构、判据分档（硬 / 歧义 / 冲突）、覆盖矩阵、怎么跑与退出码语义 |
| [../../cua/tests/it_model_empty.py](../../cua/tests/it_model_empty.py) | 168 | 独立复验：契约 §3.1「空 content 必须报 `E_MODEL_EMPTY`，不得当正常回复放行」+ 自带变异自检（判据 18 组 / 问题 0 项） |

> ⏳ **仍未验证的边界**（与 [01-architecture.md](01-architecture.md) §13 R3/R4 一致，不要在对外材料里当结论用）：
> **多显示器 / 混合 DPI** 未实测（本机单屏）；**本地 VLM 的真实端到端**（真实截图 + 真实模型）未跑（自检全程 `MockScreenshot` + `DryRunDevice`，本地实测单步 9–78 秒）；
> **坐标定位的系统性评测**仍缺——目前只有**一次**实测（`deepseek-flash`，合成截图 1472×920，三点偏差 **2.2 / 5.0 / 24.2 px**，均命中目标，见集成契约 §3.2），换分辨率 / 换模型就没有证据。

### 2.4 集成层代码（Go 网关 + 前端 tab + 打包）

`cua/` 之外新增的集成代码；契约见 [06-integration-contract.md](06-integration-contract.md)，实现说明见 [04-integration.md](04-integration.md)。

| 文件 | 行数 | 职责 |
|---|---|---|
| [../../web/cua.go](../../web/cua.go) | 948 | 进程管家 + 事件转发：单 run 闸门、JSONL→SSE 广播（原样透传）、内存历史（最近 50 条）、`frame_image` 历史裁剪到 10 条、杀进程树、环境探测 |
| [../../web/cua_llm.go](../../web/cua_llm.go) | 357 | 模型代理：每 run Bearer token 校验、OpenAI `messages` → `llm.Message`（data URI 图片 → `Images`）、`max_tokens` 下限 **16384**（2026-10-11 二次裁定）、空 content → `E_MODEL_EMPTY`、上游原因透传 |
| [../../web/cua_test.go](../../web/cua_test.go) | 642 | Go 侧集成单测 **14 个**（已实测全绿，2026-10-11 19:40，`ok blog-my/web 0.236s`）：data URI 解析、消息转换、错误映射、视觉白名单、400/409 边界、status 恒 200、代理鉴权、mock 后端正常路径、空 content 报错、上游原因透传、图片历史上限、空列表仍是数组、max_tokens 下限、sidecar 路径解析 |
| [../../templates/static/js/cua.js](../../templates/static/js/cua.js) | 1036 | 前端「电脑操控」tab（`#/cua`）：状态 / 起停 / 轨迹与截图渲染；原生 `EventSource`，无新依赖、无构建步骤。行数为 2026-10-11 19:40 采样（cua-ui 的 task-22 历史列表已完成）；此后若前端继续改动请刷新此数 |
| [../../packaging/cua.spec](../../packaging/cua.spec) | 93 | PyInstaller 单文件 spec → `bin/cua-sidecar.exe`（✅ 产物已生成：48,711,704 字节；task-26 重建，含 task-25 输入层修复）；`datas` 把 `docs/cua` 一并打进包 |
| [../../packaging/cua_sidecar_entry.py](../../packaging/cua_sidecar_entry.py) | 31 | sidecar 入口：吃掉 `-m cua.main` 前缀，让**打包产物与源码态跑同一份自检** |
| [../../electron/main.js](../../electron/main.js) | 267 | Electron 主进程壳：起 Go 二进制、开窗口，退出时 `taskkill /T /F` 收进程树（task-23 修掉「安装后 app 起不来后端」） |
| [../../scripts/build-go.js](../../scripts/build-go.js) | 14 | `npm run build:go` / `npm run dist` 中构建 Go 二进制到 `bin/` |
| 安装包（无源文件行数） | — | ✅ 产物 `dist/blog-my Setup 0.1.0.exe`（**185,244,782 字节**，NSIS）+ `dist/win-unpacked/`；⚠️ **"装上能跑"仍缺仓库内证据**：task-23 修过「安装后 app 起不来后端」，但没有"安装 → 启动 Electron → 跑一次"的留档；**终验 task-28 尚未出具** |

---

## 三、不在本目录、但读 CUA 时必须一起看的文件

### 3.1 RAG（CUA 的"知识侧"）

| 文件 | 为什么一起看 |
|---|---|
| [../rag-kb.md](../rag-kb.md) | 本地多模态 RAG 设计：表结构、接口清单与已知限制 |
| [../api-contract.md](../api-contract.md) | blog-my HTTP 接口契约；**第 9 节**为 Day 23 新增的 `/api/kb/*` 知识库契约，**第 10 节**为 Day 24 新增的 `/api/cua/*` 电脑操控契约（含内部模型代理 10.6） |
| [../../web/kb.go](../../web/kb.go) | `/api/kb/*` 8 条接口实现（列表 / 上传 / 文本入库 / 详情 / 删除 / 统计 / 检索 / SSE 问答） |
| [../../rag/](../../rag/) | RAG 后端包：`chunk.go` 切块、`embed.go` 嵌入、`vision.go` 视觉转写、`ingest.go` 入库、`store.go` 存储与暴力余弦检索 |
| [../../db/schema-rag.sql](../../db/schema-rag.sql) | `kb_document` / `kb_chunk` 建表脚本，与 `model/kb.go` 严格对齐 |
| [../../templates/static/js/kb.js](../../templates/static/js/kb.js) | Day 23 前端接入：侧栏「知识库」的上传 / 列表 / 检索 / 问答 / 删除交互 |

### 3.2 Day 23 验收证据（RAG 端到端 + 真实浏览器 UI）

> **最终结论**：三组运行**全部 FAIL 0、退出码 0**，合计 **PASS 55 + 59 + 28 = 142**。
> 此前一版 README 引用的「run1 29 PASS / 4 FAIL」是**早期中间态，已作废**。

| 文件 | 行数 | 内容与数字 |
|---|---|---|
| [../day23/notes.md](../day23/notes.md) | 294 | 验收记录正文：一句话结论、环境事实（8080 端口坑，见第四节）、缺陷清单与"设计如此 / 待验证"标注 |
| [../day23/verify-log.txt](../day23/verify-log.txt) | 336 | **最终拼接日志**（含三组汇总）：PASS 55 / WARN 0 / FAIL 0（后端契约第 1 次）、PASS 59 / WARN 0 / FAIL 0（第 2 次含 `--ask`）、PASS 28 / WARN 0 / FAIL 0（真实浏览器） |
| [../day23/verify-log-run1.txt](../day23/verify-log-run1.txt) | 114 | 后端 HTTP 契约第 1 次：PASS 55 / WARN 0 / FAIL 0，退出码 0 |
| [../day23/verify-log-run2-ask.txt](../day23/verify-log-run2-ask.txt) | 124 | 第 2 次（额外跑 `/api/kb/ask` SSE 冒烟）：PASS 59 / WARN 0 / FAIL 0，退出码 0 |
| [../day23/ui-verify-log.txt](../day23/ui-verify-log.txt) | 69 | 真实 Chromium 点页面：PASS 28 / WARN 0 / FAIL 0，退出码 0 |
| [../day23/ui-transcript.json](../day23/ui-transcript.json)、[../day23/rag-transcript.json](../day23/rag-transcript.json)、[../day23/rag-transcript-ask.json](../day23/rag-transcript-ask.json) | — | 三组运行的证据 JSON（逐项期望值 / 实际值） |
| [../day23/screenshots/](../day23/screenshots/) | — | 7 张截图（`kb-1-view` … `kb-7-cleanup`） |
| [../../scripts/verify-rag.mjs](../../scripts/verify-rag.mjs) | — | 后端契约验收脚本（`--ask` 开关；PASS / FAIL / WARN 分级） |
| [../day23/verify-ui.mjs](../day23/verify-ui.mjs)（403 行）、[../day23/check-screenshots.py](../day23/check-screenshots.py)（25 行） | — | 浏览器验收脚本与截图校对脚本 |

### 3.3 独立复验（verifier：task-7 / task-8 / task-9，及进行中的 task-18）

| 路径 | 行数 | 说明 |
|---|---|---|
| [../verify/task-7-run-01.log](../verify/task-7-run-01.log) … [task-7-run-08.log](../verify/task-7-run-08.log) | 945 / 342 / 286 / 154 / 181 / 194 / 159 / 160 | 契约套件的 **8 次**复验运行日志；轨迹为 run-01 `failures=45 errors=19 skipped=11` → run-04 首次归零 → run-05/06 回归（`failures=1` / `failures=2`）→ run-07 `run=90` → **run-08 `run=91 failures=0 errors=0 skipped=0`** |
| [../verify/task-7-run-01.utf8.log](../verify/task-7-run-01.utf8.log) | 945 | run-01 的 UTF-8 转写版（原日志含非 UTF-8 字节，无法直接按文本读） |
| [../verify/task-7-negative-control.log](../verify/task-7-negative-control.log) | 15 | 套件负向对照（注入契约违规时套件必须变红） |
| [../verify/task-7-cua-contract-tests.md](../verify/task-7-cua-contract-tests.md) | 175 | **task-7 契约测试报告正文**：91 用例全绿、负向对照 11/11、能力解析 11/11，第六节列出未覆盖面 |
| [../verify/cua-independent.md](../verify/cua-independent.md) | 190 | **task-9 框架 / 文档一致性复验报告**：41/41 机械比对、8/8 黑盒探测，自己跑的 selfcheck 与 dry-run 闭环全绿。⚠️ 其「81/0」「20 个错误码」是**当时快照**；截至 2026-10-11 19:37 为 **87/0**（task-25 后）与 **21 码** |
| [../verify/rag-independent.md](../verify/rag-independent.md) | 248 | **task-8 RAG 端到端独立复验报告** |
| [../verify/c14-encoding-record.md](../verify/c14-encoding-record.md) | 150 | **C14 非 ASCII 往返保真验证记录**：字节级判据（E8a–E8e、C14a–C14c）+ 负向对照（事件流 11/11、集成链路 16/16） |
| [../verify/integration-baseline.log](../verify/integration-baseline.log) | 257 | 集成层过程日志：21 条闭环探测、能力表、dry-run 取证范围 |
| [../verify/integration-independent.md](../verify/integration-independent.md) | 191 | **task-18 集成层独立复验报告**：全量集成探测 **PASS=39 / FAIL=0 / WARN=0 / SKIP=0**（含 emoji 向量）；含 stop 收进程树、`frame_image` 上限、§3.4 provider+ollama 三条专项与 16/16 负向对照；§9 列 7 条未覆盖面 |
| [../verify/frontend-probe.mjs](../verify/frontend-probe.mjs)（228 行）、[../verify/integration-probe.mjs](../verify/integration-probe.mjs)（599 行） | — | 前端**真实浏览器**探针、集成层 SSE 探针 |
| `docs/verify/tmp/frontend-probe-live-02.log` | 20 | 前端探针实跑汇总：**PASS=13 FAIL=0 SKIP=0**（含 dry-run 启动后轨迹渲染出条目） |

> ✅ **集成层（task-18）的复验报告已出具**：见上表 [../verify/integration-independent.md](../verify/integration-independent.md)（191 行）——全量探测 **PASS=39 / FAIL=0 / WARN=0 / SKIP=0**（含 emoji 向量）、selfcheck **84/0**（**报告出具时点**，task-25 后为 **87/0**）、套件 91/0、§7 门禁 `go build` / `go vet` / `go test -count=1` 全 0 且 `gofmt -l` 干净；负向对照 16/16 + 11/11 + 11/11。
> 该报告 §9 列出 7 条**它没能验证**的面（已折进 [04-integration.md](04-integration.md) §8 的 L6–L11）。task-7 / task-8 / task-9 的报告正文同样已存在。
> 另：`integration-baseline.log` 的采样里记过一次 `go test ./...` 变红（`TestCuaLLMProxyEmptyContentIsError`）；本人 2026-10-11 16:32 复跑全量为**绿**（退出码 0）。
> 表中 verifier 侧文件的行数为 **2026-10-11 16:35–16:50 采样**；verifier 仍可能在追补证据日志（`c14-encoding-record.md`、`integration-baseline.log` 等），引用时请以当时文件为准。

---

## 四、环境与运行（2026-10-11 实测）

- ⚠️ **端口坑（先看这条）**：本机 **8080 绑不上** —— 它落在 **Windows 保留端口段 8015–8114**（Hyper-V/WSL 动态端口预留）里，
  `listen tcp 127.0.0.1:8080` 会返回 `WSAEACCES`（"access permissions" 错误），**不是端口被占用**。
  查证：`netsh interface ipv4 show excludedportrange protocol=tcp` 可见 `8015–8114`。
- ⚠️ **保留段会漂移（2026-10-11 新记）**：实测 **8300 曾 bind `EACCES`**、**8317 / 2343 曾中途 `CONNREFUSED`**，
  而 `netsh` 显示 **8476–8575** 等区间在变——"某端口今天能用"不代表明天能用，**固定端口在长链路里不可靠**。
  **缓解：用 `APP_PORT=0` 让系统分配端口**（Electron 壳本来就是这么做的），脚本里不要硬编码端口。
  来源：Lead 实测（task-27 转记）；Day 23 的三组验收当时跑在 `http://127.0.0.1:8300`（[../day23/notes.md](../day23/notes.md) 第 2 节）。
- **后端**：`npm start` 或 `go run .`；用 `$env:APP_PORT=8300` 时访问 `http://127.0.0.1:8300`。
- **本地模型**：Ollama 在 `http://127.0.0.1:11434`，本机已有 `bge-m3:latest`（嵌入）、`qwen2.5vl:7b`（视觉）、`qwen2.5:7b` / `qwen3:4b`（生成）。
- **数据库**：MySQL 5.7 @ `127.0.0.1:3306`，库 `blog_my`（账号密码见仓库根 `.env`）。
- **Python**：3.13，已装 PyAutoGUI / Pillow / numpy / openai / httpx；**未装 pytest**。
- **Go 测试**：`go test ./rag/` → `ok blog-my/rag 0.054s`；集成层 `go build ./...` 退出码 0、`go test ./web/ -run TestCua -count=1` → **14/14 PASS，`ok blog-my/web 0.236s`**（2026-10-11 19:40 本人复跑；14 个用例名见 [04-integration.md](04-integration.md) §9.2；此后 cua-go 仍可能在加用例）。
- **CUA 离线自检**：`python -m cua.main --selfcheck` → **通过 87 项 / 失败 0 项，退出码 0**（本人 2026-10-11 19:37 复跑）。项数演变：81 →（集成契约 §3.1 加 2 项）83 →（UTF-8 往返加 1 项）84 →（task-25 输入层加 3 项：ASCII 走剪贴板 / 剪贴板不可用退化 / `clear_first` 仍先 `Ctrl+A`）**87**。
  全程使用 `MockScreenshot` + `DryRunDevice`/`RecordingDevice`：不截图、不移动鼠标、不按键；真实 pyautogui 仅被导入用于断言 `FAILSAFE=True`。
- **CUA 契约测试**：`python cua/tests/run_tests.py`（可选 `-v`、`-k <子串>`）→ **`run=91 failures=0 errors=0 skipped=0`，退出码 0**（本人 2026-10-11 复跑）。
  与 verifier 日志最后一次（run-07）的 `run=90` 相比多 1 个用例，说明此后又补了 1 条；两者结论一致（0 失败 / 0 跳过）。
- **`E_MODEL_EMPTY` 独立复验**：`python -X utf8 cua/tests/it_model_empty.py` → **判据 18 组 / 问题 0 项，退出码 0**（含变异自检：把实现换成「空 content 返回正常」的坏版本会被抓住）。
- **真实端到端验收（Lead，2026-10-11）✅**：统一链路 Electron/Go → `/api/cua/*` → `bin/cua-sidecar.exe` → 模型代理 → `deepseek-flash` → pyautogui **真跑通**：
  任务「打开记事本输入 hello from CUA 保存为桌面 cua-demo.txt」**28 步 done**；`E:\虚拟C盘\cua-demo.txt` 实测存在、**14 字节**、raw bytes == `b'hello from CUA'`；
  6 次 `type` 的 `type_path` 全为 `clipboard`；`run_end ok=true` 且无 `E_MODEL_EMPTY`。详见 [04-integration.md](04-integration.md) §9.1。
  ⚠️ 归属：**Lead 实测，非 verifier 独立复验**；终验 **task-28** 尚未出具。
- **预算分档（契约 §8 第 4 条 v1 裁定，`--budget-profile`）**：

  | 档 | 步数 | 墙钟 | 截图长边 | 适用 |
  |---|---|---|---|---|
  | `cloud`（默认） | 40 | 300 s | 1472 | 云端 API 后端 |
  | `local` | 40 | 1800 s | 1280 | 本地 VLM（实测单步 9–78 s） |
  | `auto` | — | — | — | 按 `base_url` 判断；`--max-steps` / `--max-seconds` 可覆盖分档 |

---

## 五、文档与协作规范（本仓库约定）

1. **中文书写**，术语首次出现给中英对照（如 Observation / 观测）。
2. **只写实测到的东西**：没跑过的一律标「设计如此」或「待验证」；不虚构实验数据、性能数字与完成度。
3. **数字必须有出处**：命令输出、日志文件、单测结果或文档小节；估算必须标 📐 并说明是推算。
4. **契约优先**：协议与 Schema 一旦冻结，字段增删或语义变化都必须由项目负责人确认并升版本号（`cua/1` → `cua/2`），不允许"实现时顺手改一下"。并行开发中，契约漂移是最大的返工来源。
5. **v1 裁定要留痕**：像本次两处修订（§2 终态字段、§8 预算分档）那样——**不改接口语义就不升版本号**，但必须在契约内写入裁定块、在 [01-architecture.md](01-architecture.md) §13 记录 R 编号与理由，并同步受影响的 02 文档；否则旧口径会残留在别处。
6. **本索引随文件增减同步更新**：新增文档时在第一节加一行并标状态与行数；`cua/` 改变结构时同步第二节；新增验收日志时更新第三节的数字（**只写实测计数，不写"大概通过"**）。
7. **本机端口**：验收统一用 `APP_PORT=8300`；不要再用 8080，避免把 Windows 保留端口段问题误判成"端口被占用"。
8. **禁止在文档里写"已完成"而没有证据**；验收结论以脚本的 PASS / FAIL / WARN 计数为准。
9. **进程间通信只走事件流**：Go 侧调 CUA **始终传** `--events stdout`（产品路径）；不要用人类可读 stdout 做 IPC——Windows 管道下 Python 按控制台 locale（本机实测 GBK）落字节，中文会坏成 U+FFFD、emoji 会直接崩溃（详见 [04-integration.md](04-integration.md) §2.4）。