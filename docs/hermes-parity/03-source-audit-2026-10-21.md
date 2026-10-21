# Hermes 源码核验与项目结构建议

核验日期：2026-10-21。本文记录本轮开始时的源码差距，作为实现和验收依据；同轮后续修复的最终结果见验收报告。本文未将静态阅读当作模型或线上服务验证。

## 源码基准

本地研读目录为 `references/hermes-agent-main/`，仅作只读参考，不作为应用依赖或发布内容。已通过 GitHub 接口核验上游主分支提交 [40cc180823d182826536416de2fc27be44b4ac57](https://github.com/NousResearch/hermes-agent/commit/40cc180823d182826536416de2fc27be44b4ac57)，提交时间为 2026-10-21 10:57:37（北京时间）。本地源码快照未发现可供比对的 Git 元数据，因此不能声称逐文件与该提交完全一致。

以下能力判断来自实际源码：

| 关注点 | Hermes 实现与来源 | 本项目可复用的设计 |
|---|---|---|
| 工具按需发现 | [`tools/tool_search.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/tool_search.py)、`tool_search_catalog.py` | 搜索可用工具，再向下一轮模型提供定义；搜索结果与真正可执行集合必须一致 |
| 参数纠偏 | [`tools/arg_coercion.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/arg_coercion.py) | 按 schema 纠偏数字、布尔、JSON 字符串，再做执行前校验 |
| 长结果回读 | [`tools/tool_result_storage.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/tool_result_storage.py) | 保存完整结果，回传有限预览及后续读取入口 |
| 明确学习任务 | [`agent/learn_prompt.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/learn_prompt.py) 的 `build_learn_prompt` | 使用现有模型和工具读取真实来源，先找已有技能，整理可执行过程、前提、失败处理和验证方式 |
| 自动学习触发 | [`agent/turn_finalizer.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/turn_finalizer.py) 的技能间隔检查 | 在用户任务收尾后、达到间隔时后台复盘；不让每次普通聊天都重复审查 |
| 后台权限收束 | [`agent/background_review.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/background_review.py) 的 `_review_tool_whitelist` | 后台维护仅使用技能工具、按触发条件开放记忆工具及只读文件；拒绝任意终端与文件写入；避免复盘递归触发复盘 |
| 使用反馈 | [`tools/skill_usage.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/skill_usage.py) 的 `bump_view`、`bump_use`、`bump_patch` | 区分阅读、使用和修改；仅“读取技能成功”不能证明“采用技能后的任务成功” |
| 技能库整理 | [`agent/curator.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/curator.py) 的 `run_curator_review`、`maybe_run_curator` | 有间隔、空闲条件、运行互斥、来源保护、归档和报告 |
| 跨会话回忆 | [`tools/session_search_tool.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/session_search_tool.py) 的 `session_search` | 搜索、浏览、读取和定位真实历史消息；限制返回量，防止一条旧长结果撑爆上下文 |

这里的“自我进化”主要是过程技能、长期记忆与用户偏好的持续更新，不是修改模型权重。更有价值的对标对象是有证据的学习闭环，而非工具数量。

## 首次核验发现的关键缺口

### 1. 文件数据未按用户隔离

`internal/web/hermes_evolve.go:newHermesEvolve` 接收 `userID`，但最初直接将 `hermesDataDir()` 用作所有用户的轨迹、账本、记忆和报告目录。数据库技能按 `user_id` 查询，文件数据却没有同等边界，可能让甲用户记忆进入乙用户提示词，或用甲用户轨迹为乙用户提炼技能。

`internal/web/hermes_catalog.go:buildAgentToolSource` 最初也将长结果写入共同的 `hermesDataDir()/spill`。读取工具虽校验目录与符号链接，仍需要先确保该目录属于当前用户，才能建立完整隔离。

验收应覆盖：两个用户目录不同；记忆、报告、轨迹、账本和长结果不可交叉读取；未验证归属的旧共享数据不自动迁移到任一用户。

### 2. 候选技能缺少可复用证据

`internal/agent/evolve/candidate.go` 的重复序列识别最初只看工具名字，正文是“按顺序调用某工具”的固定骨架。它缺少成功参数、失败原因、正确修复方式与结果验证。不同任务即使工具序列相同，也未必共享同一种解决方法。

同文件的 `lessonCandidates` 最初只统计“存在成功且失败次数足够”，没有验证失败发生在成功之前；先成功后失败也会得到“失败后已解决”的结论。重复序列还需要过滤失败步骤，避免把失败模式当作成功流程保存。

`internal/web/hermes_evolve.go:ObserveToolCall` 最初仅记录开始时间，未保存参数；`ObserveToolResult` 仅写摘要。应保留有界且脱敏的调用参数与真实反馈，生成的技能给出成功用法、修复线索和来源轨迹。原始日志属于不可信材料，不应变成越过权限的指令。

验收应覆盖：失败后成功产生带修复证据的候选；成功后失败不产出“已修复”候选；缺少证据时不将空骨架自动启用；密钥和密码不进入长期技能或报告。

### 3. 统计、触发和回滚接线不完整

最初 `RecordSkillUse` 已有定义，但未找到接入 `skill_run` 或技能采用后的最终结果；使用统计因此不能可靠地驱动风险判断。阅读事件、执行事件和最终任务完成需要明确语义。

最初 `CurateAsync` 仅处理 `suggest` 档，`auto` 档不会在任务后自动固化。建议明确 `off/observe/suggest/auto` 的行为，后台按用户限并发、设置间隔和单次改动上限。自动执行也只能修改约定的学习数据，不得扩展文件或工具权限。

`internal/agent/evolve/journal.go` 有变更日志及回滚实现，但最初 web 层 `Curate` 未注入 Journal，也没有用户可调用的回滚入口。后台技能改写必须保留前后版本及批次，且回滚应检查技能是否被后续人工修改，避免旧快照覆盖新内容。

验收应覆盖：真实技能执行改变账本；后台仅一份同用户固化运行；自动变更可查、可撤销；无法写审计记录时不静默宣称可回滚。

### 4. 原始历史检索缺失

已有 RAG 和会话摘要解决相关信息提取，最初没有找到与 Hermes `session_search` 等价的原始历史工具。用户询问“上次怎么修好”时，模型需要能定位原始消息和证据，而非仅依赖提炼后的记忆。

可复用现有 conversation/chat 数据表实现：所有查询带当前用户归属限制，过滤内部轨迹消息，限定单页数量和每条长度，支持关键词发现及指定会话读取。无数据库时明确返回不可用，禁止退化成共享文件扫描。

### 5. 评测分数不能代替任务成功证据

`internal/agent/evolve/eval.go` 已实现确定性工具命中率与失败率评分。这能回归工具行为，但不能证明网页、文件、数据库或业务目标已完成。验收应同时记录模型调用链、实际产物与相应服务验证，并区分离线模型桩和真实提供商。

## Go/Electron 项目结构规范

Hermes 是 Python 项目，目录名不应机械照搬。当前项目已具备合理的 Go 分层：

```text
cmd/blog-my/          主应用入口，只处理进程信号并调用 app.Run
cmd/skillimport/      独立技能导入命令
internal/app/         配置读取、数据库初始化、服务启动与关闭
internal/web/         身份、会话、HTTP/SSE 以及业务装配
internal/agent/       工具契约、执行循环、预算、委派与团队
  toolsys/            工具目录、校验、披露、批处理、长结果
  mcps/               MCP 协议和外部工具适配
  evolve/             轨迹、账本、候选、记忆、固化、评测与审计
  fs/                 工作区沙箱与文件工具
  skill/              技能解析与目录处理
  toolkit/            通用工具
internal/llm/         模型提供商与统一请求契约
internal/model/       数据模型和数据库访问
internal/rag/         文档、向量与词法检索
electron/             桌面启动、进程管理与系统交互
templates/            页面、样式和浏览器脚本
scripts/              构建、测试、检查与验收入口
docs/                 架构、能力契约、用户说明及验证证据
references/           本地外部源码参考，忽略且不打包
```

可立即落实的边界：

1. 保持 `cmd → app → web → agent/llm/model/rag` 的依赖方向；`agent/evolve`、`toolsys` 和 `mcps` 不导入 web 或数据库实现。数据库适配留在 web 接线层，通过接口注入。
2. `internal/app` 统一拥有生命周期。MCP 子进程、后台固化和调度器应受应用关闭信号约束，避免桌面退出后留下工作进程。
3. HTTP handler 不复制候选分析、JSON Schema 纠偏、轨迹文件格式等核心逻辑；核心规则写在对应包，web 负责用户、模式和配置的装配。
4. 单元测试与被测文件同目录；HTTP、模型桩与跨模块行为测试明确标记验证边界。统一脚本分别执行 Go、Electron 和页面测试，所有入口使用一致缓存与超时约定。
5. 根目录保留入口、配置样例、依赖清单与发布配置。构建产物进入 `bin/build/dist`，生成文件进入 `artifacts/workspace`；学习数据进入应用数据目录并按用户隔离，不写回源码目录。
6. `.env`、模型密钥、用户记忆和外部参考源码不入库。构建只打包运行所需内容，不能把 references、测试缓存或整份用户数据混入安装包。
7. 新能力的公共说明记录在 `docs/agent`，跨模块装配与依赖关系记录在 `docs/architecture/agent-runtime.md`，验证报告放 `docs/verify`；避免把临时任务分工当成永久架构规范。

本轮不需要搬动大批历史文件。先补齐接口接线、入口一致性和验证文档，保持应用可构建与现有工作区行为，再按具体边界逐步拆分过重的 web 文件。

## 研究范围与验证边界

本研究读取 Hermes 的工具、技能、会话搜索、后台审查源码，以及本项目的工具目录、MCP、学习核心和 web 接线；上游提交通过 GitHub 只读接口确认。初始源码研究阶段未执行模型任务、数据库写入或 Electron 交互，也未修改应用代码；后续独立复核的执行范围见下节。最终能力清单以本轮完成后的测试和验收报告为准。

## 同轮修复与独立复核补记

以下为初始审查之后的进展，不再作为未解决缺口：

- 已看到 web 接线将记忆、轨迹、账本、报告和长结果改为用户隔离目录；MCP 归入独立工具集，并应用主账号、会话用途和执行档位限制。
- 确认重放已重新读取当前会话用途并校验用户归属，使用原会话身份保存长结果。工具结果事件增加 `AwaitUser`，等待确认的调用不再进入“已执行成功”的学习记录；子级外部路径访问同样检查主账号身份。
- 后台自动固化增加本轮用途、执行档位和子级约束；子级生成报告，由主级处理变更。候选增加真实脱敏参数及成功证据，旧空骨架只作建议，归档保护用户创作和导入技能。
- 工具工程师报告已修复长结果的会话校验、超长单行字符分页、schema 本地引用展开、MCP 写入与响应的完整超时，以及取消后的重试边界。Eino 也已改为每次模型请求前绑定最新目录定义，覆盖同一 ReAct 流程中搜索后立即使用工具的情况；其报告的三包回归范围为 `agent/toolsys/mcps`。这些包级测试由对应工程师执行，本文不重复声称独立执行了同一批测试。

研究角色另在 `internal/web/hermes_review_test.go` 增加并亲自运行了三项真实 MySQL 复核：

1. `TestHermesReviewSessionSearchIsolationAndPaging`：关键词中的 `%` 与 `_` 按字面匹配；原文片段不超过 1200 字；两页结果为 2+1 条且不重复；内部工具轨迹不返回；猜测其他用户会话编号得不到原文；对方仍能读取自己的会话。
2. `TestHermesReviewPendingWriteRespectsChangedConversationKind`：通过真实 HTTP/Eino 模型桩先在工作会话发起写入，下一请求切为日常聊天并确认；文件没有落盘、返回拒绝且待确认记录被清空。
3. `TestHermesReviewPendingSpillCanBeReadByContinuingConversation`：真实待确认写工具执行后产生落盘结果，后续主会话能用自己的会话身份读回完整结果。

执行方式：设置 `AGENT_IT=1` 后运行 `go test ./internal/web -run '^TestHermesReview' -count=1 -timeout=90s -v`。数据库数据采用独立测试用户并限定归属清理；HTTP 用例关闭自动 RAG 记录。测试不连接外部模型提供商，不代表 Electron 真机点击或真实外部服务验收。
