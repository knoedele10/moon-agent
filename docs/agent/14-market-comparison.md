# 14 市面 Agent 对标与 Moon Agent 改进建议

> 调研时间：2026-10-25。对象：编码类 agent、通用/自主 agent、桌面本地 agent，以及支撑它们的协议、记忆与评测基础设施。
> 口径：能力判断来自各产品公开文档、官方博客与公开评测页面；矩阵中的「◐」表示部分支持、需插件或公开资料不足以确认。产品迭代很快，具体能力以文末链接的官方页面为准。
> 结论都尽量落到本仓库的包与文件上，避免停留在「建议加强」。

## 0. 结论摘要

1. **同生态位不是 Claude Code。** Moon Agent 是「本地优先的个人桌面 agent」，直接竞品是 Block Goose、Anthropic Claude Cowork、Manus；Claude Code / Codex / Cursor 是**能力标尺**，不是替代对象。
2. **你的差异化在「记忆 × 技能进化 × 定时唤醒 × 本地多模型」**。这四件事大厂要么不做（隐私/成本），要么做得浅；这是应该继续加厚而不是砍掉的部分。
3. **最大短板不是模型能力，是产品化三件套**：装得上（MySQL 门槛）、看得见（diff/产物/成本）、断得了（关掉应用任务就没了）。
4. **安全是下一个分水岭。** 现在的「路径沙箱 + 命令白名单」在个人场景够用，但一旦放开 shell/browser，就必须上 OS 级隔离；Windows 侧已经有官方抓手（Microsoft Execution Containers）。
5. **不要追多 agent 框架。** Eino + 现有 subagent/team 足够；把预算花在 ACI、评测和可观测上，用 20~30 条真实任务证明每一步改进。

## 1. 市场分层：先分清谁跟你是同一生态位

| 层级 | 代表产品 | 本质 | 与 Moon Agent 的关系 |
| --- | --- | --- | --- |
| 终端编码 agent | Claude Code、OpenAI Codex CLI、Gemini CLI、Aider、OpenCode | 在 repo 里读改跑，靠 shell/git 闭环 | 能力标尺：工具设计、上下文工程、权限 |
| IDE agent | Cursor、Cline、Roo Code、GitHub Copilot agent | 编辑器内的补全 + 多文件编辑 + 审阅 | 交互标尺：diff 审阅、checkpoint、规则文件 |
| 自主 SWE agent | Devin、OpenHands、Factory Droids | 云端/容器里的长任务，交付 PR | 参考：沙箱、任务持久化、验收 |
| 云端通用 agent | Manus、ChatGPT Agent | 虚拟机 + 浏览器 + 异步交付 | 参考：异步任务、产物、人类接管点 |
| 桌面/本地 agent | **Moon Agent**、Claude Cowork、Goose | 本机文件/应用/浏览器，隐私优先 | **直接竞品** |
| 基础设施 | MCP、Agent Skills、AGENTS.md、A2A、Mem0/Letta、OTel GenAI | 协议、记忆、评测、可观测 | 决定「接得上谁」和「能不能证明自己」 |

一句话：**跟 Goose/Cowork 抢「个人桌面 agent」的位置，用 Claude Code/Codex 的工程标准要求自己。**

## 2. 能力矩阵

| 维度 | Moon Agent | Claude Code | Codex | Cursor | Goose | OpenHands | Manus |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 形态 | Windows 桌面（Electron+Go） | 终端 CLI | CLI + 云任务 | IDE | 桌面 + CLI | Web/CLI | 云端 |
| 运行位置 | 本机 | 本机 | 本机/云 | 本机/云 | 本机 | Docker/云 | 云 VM |
| 模型 | 10+ BYOK、Ollama | Claude | OpenAI | 多模型 | 多模型 | 多模型 | 内置 |
| 工具扩展 | MCP + SKILL.md | MCP + Skills | MCP | MCP | MCP（extensions） | MCP | 内置 |
| 子 agent | ✅ subagent/team | ✅ | ✅ 子 agent/云并行 | ✅ subagents | ◐ | ✅ | ✅ 内部 |
| 计划外化 | ✅ update_plan | ✅ plan mode | ✅ update_plan | ✅ Plan Mode | ◐ | ◐ | ◐ |
| 上下文工程 | ✅ 压缩+落盘+渐进披露 | ✅ 自动压缩 | ✅ | ✅ | ◐ | ✅ | ✅ |
| 文件回滚 | ✅ restore / restore_turn | ✅ rewind | ◐ git | ✅ checkpoint（仅文件） | — | ◐ git | — |
| 权限模型 | ✅ plan/confirm/auto + 风险元数据 | ✅ allow/deny + 模式 | ✅ 审批模式 | ✅ allowlist | ◐ | ◐ | ◐ |
| OS 级沙箱 | — 仅路径+白名单 | ◐ 命令沙箱 | ✅ Seatbelt/Landlock | ◐ 终端策略 | ◐ | ✅ Docker | ✅ VM |
| 后台/定时 | ✅ 唤醒+排期 | ◐ 后台任务 | ✅ 云任务 | ✅ background | — | ◐ | ◐ |
| 长期记忆 | ✅ RAG+画像+结构化条目 | ◐ CLAUDE.md | ◐ AGENTS.md | ✅ memories | ◐ | ◐ microagents | ✅ |
| 产物交付 | ◐ 文档导出，无产物中心 | ◐ 文件编辑 | ◐ PR | ◐ | ◐ | ◐ | ✅ artifacts |
| 评测 | ◐ 轨迹工具命中分 | — 内部 | — 内部 | — 内部 | — | ✅ SWE-bench | — |
| 可观测 | ◐ SSE+JSONL 轨迹 | ✅ OTel | ◐ | — | — | ◐ | — |
| 跨平台 | — 仅 Windows | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ Web |

> 读法：这张表不是「谁更强」，而是「哪些能力已经被市场验证为刚需」。Moon Agent 在上下文工程、记忆、技能上并不落后；差距集中在**沙箱、持久任务、产物与评测**。

把 2026 年的产品动作连起来看，竞争焦点已经从「补全多准」转到「长任务能不能跑完、跑错了能不能退」——Plan / subagent / checkpoint / AGENTS.md 正在被收敛成事实标准，但还没有跨厂商规范。这恰好是 Moon Agent 已经有骨架、但还没做透的方向。

### 2.1 三个标准信号（决定能不能接上生态）

| 信号 | 意味着什么 | 对 Moon Agent 的动作 |
| --- | --- | --- |
| MCP 已是事实标准，2026-07 版规范强调无状态核心 | 工具接入不再是差异化，而是入场券；stdio 只适合本地，远程要 Streamable HTTP | 现有 `mcps` 只做 stdio + 文件配置；补 HTTP 传输、健康检查与权限声明 |
| SKILL.md 正在跨产品标准化（据公开信息已捐给 AAIF） | 技能可以像插件一样分发、复用 | 现在只解析 `name/description`；补 `allowed-tools`、版本、来源，支持从 Claude 技能目录导入 |
| [AGENTS.md](https://agents.md/) 成为仓库级约定事实标准（Linux Foundation 下 AAIF 托管） | 用户会默认「我的项目规则你应该读」，且支持就近覆盖/嵌套 | P0-2 直接做，别再自定义格式 |

A2A / AG-UI 这类跨 agent、agent-前端协议，对单机个人产品暂时是过度设计；但 AG-UI 的事件分类可以对照你的 SSE 契约查漏。

链接：[MCP 规范](https://modelcontextprotocol.io/) · [Agent Skills](https://code.claude.com/docs/en/skills) · [A2A](https://a2a-protocol.org/) · [AG-UI](https://docs.ag-ui.com/)

### 2.2 主观评分（1~5，仅用于排优先级）

| 维度 | 自评 | 依据 |
| --- | --- | --- |
| 工具与 ACI | 4.5 | grep/glob、edit 回显、错误自纠、结果落盘、渐进披露 |
| 上下文工程 | 4.5 | 中段压缩、单条上限、只读并发、子 agent 隔离 |
| 记忆与个性化 | 4 | RAG + 画像 + 结构化记忆 + 技能账本 |
| 安全隔离 | 2 | 路径校验强，但无 OS 级隔离；`run_command` 靠白名单 |
| 生态接入 | 2.5 | MCP 有但无市场；技能无导入/分发 |
| 可观测与评测 | 2 | 有轨迹与 SSE，无成本、无 trace、无任务级 eval |
| 部署体验 | 2 | 必须自备 MySQL + 手填 `APP_SECRET` |
| 跨平台与多端 | 1 | Windows only，无远程/移动 |

## 3. 你已经领先、应该放大的东西

1. **本地优先 + BYOK 多模型**：DeepSeek/OpenAI/Claude/Gemini + 兼容端点 + Ollama。这是隐私敏感用户选 Goose 而不是 Manus 的核心理由，也是国产模型用户的刚需。
2. **记忆是「厚」的**：文档 RAG、自动记忆、用户画像、结构化 memory 条目（topic/去重/forget）四层并存。Claude Code 只有 CLAUDE.md，Cursor 的 memories 也远没到这个粒度。
3. **技能可以自我进化**：SKILL.md 解析、lint、使用账本、轨迹提炼候选、后台固化。市面产品大多只有「写规则」，没有「度量技能是否有效」。
4. **工具治理成体系**：toolset 声明式分组、`tool_search` 渐进披露、参数纠偏、大结果落盘、只读并发、风险元数据三边对齐（`toolsys` + web + agent）。这套东西比多数开源 agent 完整。
5. **计划外化 + 三档权限 + 文件检查点**：`update_plan` 每轮注入、plan/confirm/auto、`restore_turn` 整组撤销，正好覆盖「长任务跑飞」的三个失效点。
6. **定时唤醒 + 内容排期导出**：真正把 agent 变成「会自己干活」的产品能力，而不是聊天框。这是 Cowork/Goose 在公开资料里都没做深的部分。
7. **中文语境与 Windows 桌面**：中文文档、中文错误文案、Office 读写、CUA。英文产品在这块基本不设防。

## 4. 差距与改进清单

优先级按「投入产出比 × 对留存的影响」排，不按技术炫技排。

### P0：先做，2~3 周内见效

#### P0-1 默认 SQLite，MySQL 变可选

**现状**：所有持久化都走 MySQL；新用户要先装库、建账号、填连接串、生成 `APP_SECRET`，才能看到第一句回复。
**市面做法**：Goose / Cowork / LM Studio 这类本地产品都是「装完就能用」，数据落在本机单文件；云端产品则完全不暴露存储。
**建议**：
- `internal/model` 增加 `DB_DRIVER`（`sqlite` 默认 / `mysql` 可选），GORM 双驱动；SQLite 文件放 `%APPDATA%\blog-my\data.db`。
- 检查并去掉 MySQL 专有类型/函数（向量已是 Go 侧排序，迁移成本可控）；启动时自动迁移 + 每日备份。
- 首次启动向导：选模型 → 填 Key（可跳过）→ 选工作区 → 选权限档，四步内进入对话。
- 保留 MySQL 作为「多设备/团队」模式，而不是默认。
**验收**：干净 Windows 机器上，从安装到完成第一个文件任务 < 10 分钟；`DB_DRIVER=sqlite` 下全量单测通过；提供「导出/导入数据」按钮。
**链接**：[Goose](https://github.com/block/goose)、[Claude Cowork](https://support.claude.com/en/articles/13345190-get-started-with-claude-cowork)

#### P0-2 读 AGENTS.md / 项目规则

**现状**：没有项目级约定文件；每次都要靠 `system_prompt` 或用户口头说明。
**市面做法**：Codex 用 `/init` 生成 [AGENTS.md](https://agents.md/)；Cursor 支持 `.cursor/rules` 且能读嵌套 AGENTS.md；Claude Code 的 CLAUDE.md 也可引用 AGENTS.md。
**建议**：
- 新包 `internal/agent/rules`：从活动工作区根向上查找 `AGENTS.md`，叠加 `~/.moon/AGENTS.md` 全局规则；近的覆盖远的。
- 解析为「项目约定」段落注入 system prompt，限制总字节（如 16KB），超长给模型「用 read 查看完整规则」。
- 明确边界：规则文件是**用户授权的指令**，但不能覆盖安全策略与权限档；与 untrusted 内容分开标记。
- 提供 `/init` 式扫描：识别语言、构建/测试命令、目录约定，生成初版模板（构建命令、测试命令、代码风格、禁区）。
**验收**：改 AGENTS.md 下一轮生效；多级目录优先级有单测；注入不改变未配置时的提示词字节。
**链接**：[AGENTS.md](https://agents.md/)、[Codex 文档](https://developers.openai.com/codex/)

#### P0-3 Git 工具 + diff 审阅 + 自动验证

**现状**：能读写文件，但没有 `git status/diff/log/commit`；用户只能自己去终端看改了什么；改完是否通过也没有自动回路。
**市面做法**：Aider 以 git 为工作台自动提交，并在改完后自动跑 lint/test 修错；Cursor/Cline 每轮给 diff 并支持逐文件接受/拒绝；Claude Code 有 rewind。
**建议**：
- `internal/agent/toolkit/git`：`git_status`、`git_diff`（staged/unstaged/path）、`git_log`、`git_show`、`git_add`、`git_commit`（confirm 档必须确认）。只用 argv 调 `git`，不拼 shell，限制输出与超时；**永不 push/force**。
- 每轮结束生成「改动清单 + unified diff」，UI 支持逐文件「保留 / 撤销」（撤销复用现有 checkpoint，不用新机制）。
- 脏工作区提醒：agent 准备大改前先 `git_status`，有未提交改动就提示先建快照。
- 验证钩子：AGENTS.md 里声明 `verify`（build/test/lint），write/edit 后自动跑，失败摘要回灌给模型（参考 Aider 的 lint/test 自动修复）；同一轮最多重试一次，避免死循环。
**验收**：临时 repo 的单测覆盖 add/commit/拒绝路径；无 git 仓库时工具返回可自纠错误；diff 面板能撤销单文件且不影响其他文件；验证失败能让模型自己修一轮。
**链接**：[Aider](https://aider.chat/)、[Aider Docs](https://aider.chat/docs/)、[Cursor Docs](https://cursor.com/docs)、[Claude Code Checkpoints](https://code.claude.com/docs/en/checkpoints)

#### P0-4 Token 计量 + Prompt Caching

**现状**：`internal/llm` 没有 usage 统计，也没有缓存提示；多轮对话每轮重发 system + 工具 + 技能，成本随轮数线性涨。
**市面做法**：Claude 用 `cache_control` 显式缓存；OpenAI 对 >1024 token 的前缀自动缓存；DeepSeek 自动上下文缓存（命中价约为 1/10）；Devin 干脆用 ACU 把 token 折成可预算的工时。
**建议**：
- `internal/llm/usage.go`：从各 provider 响应抽 `input/output/cached` tokens，落到 run 记录与会话汇总。
- 稳定前缀：工具顺序、system 分段、技能清单固定排序；把计划、时间、检索结果等易变内容放到**末尾**，别插在 system 中间。
- Claude 后端对 tools + system 打 `cache_control`；DeepSeek/OpenAI 只需保证前缀稳定即可吃到自动缓存。
- UI 显示「本轮 12.3k in / 1.2k out / 缓存 8.1k」，按模型单价估算成本；超预算提醒。
**验收**：多轮对话缓存命中率 > 50%（先埋点测基线）；关闭缓存开关时行为与现在逐字节一致；成本数字可与 provider 账单对上。
**链接**：[Anthropic Prompt Caching](https://docs.claude.com/en/docs/build-with-claude/prompt-caching)、[DeepSeek Context Caching](https://api-docs.deepseek.com/guides/kv_cache)、[OpenAI Prompt Caching](https://platform.openai.com/docs/guides/prompt-caching)、[Context Engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)

### P1：两个月内，拉平一线产品

#### P1-5 持久任务队列与断点续跑

**现状**：agent 跑在进程内，关应用/崩溃即中断；`wakeup` 能定时起任务，但任务本身没有 journal。
**市面做法**：Codex 云任务、Cursor background agents、Devin sessions 都是「提交后离开，回来看结果」；Devin 还给每个会话独立 VM 与快照，支持暂停/恢复/分叉。
**建议**：
- `internal/agent/runstore`：每次 run 写 journal（run id、会话、消息、工具调用、预算、checkpoint 引用、状态）；复用 evolve 的轨迹格式，别再造一套。
- 启动时扫描 `running` 状态：可恢复的自动续跑，副作用未知的（写/执行类）先问用户「重放 / 放弃」。
- 后台任务面板：排队、运行、失败重试、取消、产物路径；与现有 SSE 事件共用状态机。
- 幂等：给工具加 `Effect: read | idempotent | unknown`，续跑时只自动重放前两类。
**验收**：任务跑到一半强杀应用，重开后能看到「已恢复/待确认/已放弃」，且不会重复写文件。
**链接**：[Codex](https://developers.openai.com/codex/)、[Cursor Background Agents](https://cursor.com/docs)、[Devin](https://devin.ai/)

#### P1-6 OS 级沙箱，替换「命令白名单」

**现状**：`run_command` 只允许 npm/npx/go/node/python，靠白名单保安全；一旦要跑 `git`、`make`、自定义脚本就不够用。
**市面做法**：Codex 用 macOS Seatbelt / Linux Landlock；OpenHands 用 Docker；Claude 的 computer-use 文档直接要求把 agent 关进隔离 VM/容器并明示提示注入风险；Windows 侧 2026-10 起有 [Microsoft Execution Containers](https://blogs.windows.com/windowsdeveloper/2026/10/07/microsoft-execution-containers-policy-driven-containment-for-ai-agents/)（策略驱动的 agent 隔离）。
**建议**：
- `internal/agent/sandbox` 定义策略：可读写根、可执行程序、出网域名、CPU/内存/时长上限。
- Windows 首选 MXC；不可用时退回 AppContainer + Job Object + 受限令牌；POSIX 工具链走 WSL2 后端。
- 沙箱内放开 shell（仍禁交互式），白名单作为「无沙箱环境」的降级路径。
- 审计：每次越权尝试写结构化日志并在 UI 标红。
**验收**：越权写 `C:\Windows`、起 `cmd.exe`、访问非白名单域名均被拒；`npm run build`、`go test` 正常；逃逸用例进 CI。
**链接**：[Microsoft Execution Containers](https://blogs.windows.com/windowsdeveloper/2026/10/07/microsoft-execution-containers-policy-driven-containment-for-ai-agents/)、[Windows Agentic](https://developer.microsoft.com/en-us/windows/agentic)、[OpenHands Runtime](https://github.com/All-Hands-AI/OpenHands)

#### P1-7 浏览器工具（CDP），别只靠 CUA

**现状**：网页任务要么 `web_reader` 抓静态正文，要么 CUA 点像素；前者处理不了登录/交互，后者慢且脆。
**市面做法**：Cursor/Claude Code 接 Playwright MCP；OpenHands 内置浏览器；Manus 用云浏览器。
**建议**：
- `internal/agent/toolkit/browser` 用 chromedp/rod 连用户已登录的 Chrome：`browser_open`、`browser_snapshot`（可访问性树）、`browser_click/type`、`browser_screenshot`、`browser_network`。
- 分层：静态正文 → `web_reader`；需要登录/点击 → 浏览器；桌面应用 → CUA。别让模型自己猜该用哪个，提示词里写清。
- 安全：域名 allowlist、提交表单/支付前 confirm、禁止读取密码框、下载落独立目录。
**验收**：本地 fixture 页面完成「搜索→点进详情→截图→提取字段」；不触碰凭据；失败时返回可自纠的 DOM 摘要。
**链接**：[Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)、[Playwright MCP](https://github.com/microsoft/playwright-mcp)、[browser-use](https://github.com/browser-use/browser-use)

#### P1-8 任务级评测 + 可观测性

**现状**：`evolve.Score` 只评「工具命中率/失败率」，证明不了「任务是否完成」；没有 trace，出问题只能翻日志。
**市面做法**：OpenHands 用 SWE-bench 证明能力；Terminal-Bench 测终端任务；OSWorld 测桌面操作；OTel GenAI 语义约定已成型（仍在演进）。
**建议**：
- `evals/`：20~30 条本机任务（整理目录、修一个 Go bug、生成周报、网页调研、Office 报表），每条带 fixture、断言（文件存在/内容匹配/命令退出码/工具序列）和成本上限。
- CI 用 mock 模型跑确定性断言；发版前用真模型跑 5 条 smoke，记录通过率/步数/成本。
- `internal/obs`：LLM 请求、工具调用、子 agent 各一个 span，字段按 [OTel GenAI semconv](https://opentelemetry.io/docs/specs/semconv/gen-ai/)；本地导出 JSONL，UI 做瀑布图。
**验收**：每次改动能看到「通过率 / 平均步数 / 平均成本 / P95 延迟」四个数；失败能定位到具体 span。
**链接**：[SWE-bench Verified](https://www.swebench.com/verified.html)、[Terminal-Bench 2.0](https://www.tbench.ai/news/announcement-2-0)、[OSWorld](https://os-world.github.io/)、[τ-bench](https://arxiv.org/abs/2406.12045)、[OTel GenAI](https://opentelemetry.io/docs/specs/semconv/gen-ai/)

### P2：产品化与差异化，别提前做

| 项 | 现状 | 建议 | 参考 |
| --- | --- | --- | --- |
| MCP 生态 | 手动配 JSON | 精选市场、一键安装、权限声明、健康检查、OAuth | [MCP](https://modelcontextprotocol.io/)、[Kilo Marketplace](https://kilo.ai/docs) |
| 技能分发 | 本地 SKILL.md | 兼容 Anthropic Agent Skills，支持导入/导出/版本 | [Agent Skills](https://code.claude.com/docs/en/skills) |
| 产物中心 | 聊天里给路径 | 每次 run 的文件/截图/下载清单 + 预览 + 一键打开/导出 | [Manus](https://manus.im/) |
| 生命周期钩子 | 无 | PreToolUse/PostToolUse/Stop 钩子：审计、格式化、拦截 | [Claude Code Hooks](https://code.claude.com/docs/en/hooks) |
| 记忆升级 | 向量+关键词+实体 | 时间衰减、冲突合并、来源引用、用户可编辑/遗忘、可选图记忆 | [Mem0](https://github.com/mem0ai/mem0)、[Letta](https://github.com/letta-ai/letta)、[Graphiti](https://github.com/getzep/graphiti) |
| 多端 | Windows 单机 | 局域网/隧道 Web 访问 + 手机审批 + 会话分享 | [AG-UI](https://docs.ag-ui.com/) |
| 跨平台 | Windows only | 先 macOS（路径/钥匙串/进程管理抽象） | [Goose](https://github.com/block/goose) |
| 多模态 | 图片输入、CUA | 语音输入、屏幕录制转任务、截图批注 | [Claude Computer Use](https://docs.claude.com/en/docs/agents-and-tools/computer-use) |
| 主动触发 | 定时唤醒 | 文件变化、邮件/日历连接器、事件触发 | [Cowork](https://support.claude.com/en/articles/13345190-get-started-with-claude-cowork) |

记忆这块单独说一句：Letta 的 core/recall/archival 分层、Graphiti 的双时态边（失效而不是硬删）、Mem0 的后台抽取，正好对应你现在 `memory_write` 的三处短板——**没有常驻核心块、没有时间维度、写入在主路径上**。先补「核心画像常驻 + 记忆带时间与来源 + 后台整理」，比换向量库收益大。

## 5. 不建议追的方向

- **全自主无人值守的电脑操作**：责任与信任成本远高于个人产品的收益；保留 confirm 档作为默认。
- **自研多 agent 编排框架**：Eino 的 ReAct + 现有 subagent/team 够用；复杂度会吃掉调试预算。
- **换独立向量数据库**：当前数据量与 Go 侧排序不是瓶颈；先解决部署重量。
- **把所有 MCP server 都接进来**：工具越多，选择越难、注入面越大；精选 + 权限声明比数量重要。
- **自建云端 VM 托管**：与「隐私优先、BYOK」的定位冲突，成本也不适合个人开发者。

## 6. 16 周路线图与指标

| 阶段 | 交付 | 指标 |
| --- | --- | --- |
| 第 1~3 周 | SQLite 默认、首启向导、AGENTS.md | 首装到首个任务 < 10 分钟；无 MySQL 也能跑通 |
| 第 4~6 周 | Git+diff 审阅、token/成本与缓存 | diff 可逐文件撤销；缓存命中率 > 50% |
| 第 7~11 周 | 持久 run journal、后台面板、OS 沙箱、浏览器工具 | 强杀恢复 100%；沙箱逃逸 0；网页任务成功率 > 70% |
| 第 12~16 周 | 20~30 条 eval、OTel trace、MCP 市场、记忆升级、macOS/远程 | 任务通过率 +10pt；trace 覆盖 100%；macOS 可日常使用 |

> 指标先埋点再定基线，不要拍脑袋写目标值；上表的数字是「合理起点」，跑一轮后按实际调整。

## 7. 参考链接

**编码 agent**
- Claude Code：[功能总览](https://code.claude.com/docs/en/features-overview) · [Subagents](https://code.claude.com/docs/en/sub-agents) · [Checkpoints](https://code.claude.com/docs/en/checkpoints) · [Agent Skills](https://code.claude.com/docs/en/skills)
- OpenAI Codex：[文档](https://developers.openai.com/codex/) · [CLI](https://developers.openai.com/codex/cli) · [GitHub](https://github.com/openai/codex)
- Cursor：[Docs](https://cursor.com/docs) · [Plan Mode](https://cursor.com/docs/agent/plan-mode)
- 开源 IDE/终端：[Cline](https://docs.cline.bot) · [Roo Code](https://docs.roocode.com) · [Kilo Code](https://kilo.ai/docs) · [Gemini CLI](https://github.com/google-gemini/gemini-cli) · [Aider](https://aider.chat/docs/) · [OpenCode](https://opencode.ai/docs/)
- 云端异步：[Copilot coding agent](https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-coding-agent) · [Jules](https://jules.google/docs)

**通用 / 自主 agent**
- [OpenHands](https://github.com/All-Hands-AI/OpenHands)（[Docs](https://docs.openhands.dev/)） · [Devin](https://devin.ai/)（[Docs](https://docs.devin.ai/)） · [Manus](https://manus.im/)
- [Block Goose](https://github.com/block/goose)（[Docs](https://block.github.io/goose/docs/)） · [Claude Cowork](https://support.claude.com/en/articles/13345190-get-started-with-claude-cowork)
- [ChatGPT Agent](https://openai.com/index/introducing-chatgpt-agent/) · [Claude Computer Use](https://docs.claude.com/en/docs/agents-and-tools/computer-use)

**协议与标准**
- [Model Context Protocol](https://modelcontextprotocol.io/) · [AGENTS.md](https://agents.md/) · [Agent Skills](https://code.claude.com/docs/en/skills)
- [A2A](https://a2a-protocol.org/) · [AG-UI](https://docs.ag-ui.com/)

**记忆**
- [Mem0](https://github.com/mem0ai/mem0) · [Letta (MemGPT)](https://github.com/letta-ai/letta) · [Graphiti](https://github.com/getzep/graphiti) · [LangMem](https://github.com/langchain-ai/langmem)

**评测与可观测**
- [SWE-bench](https://www.swebench.com/) · [Terminal-Bench](https://www.tbench.ai/) · [OSWorld](https://os-world.github.io/) · [GAIA](https://huggingface.co/spaces/gaia-benchmark/leaderboard)
- [OTel GenAI Semantic Conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) · [Langfuse](https://langfuse.com/docs)

**平台与运行时**
- [Microsoft Execution Containers](https://blogs.windows.com/windowsdeveloper/2026/10/07/microsoft-execution-containers-policy-driven-containment-for-ai-agents/) · [Windows Agentic](https://developer.microsoft.com/en-us/windows/agentic)
- [Eino](https://github.com/cloudwego/eino) · [Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/) · [Playwright MCP](https://github.com/microsoft/playwright-mcp)

## 附：本文与本仓库的对应关系

| 结论 | 落点 |
| --- | --- |
| 工具治理已是强项 | `internal/agent/toolsys`、`internal/web/agent_tool_policy.go`、`docs/agent/02-tools.md` |
| 计划/权限/回滚 | `internal/agent/plan.go`、`internal/web/agent_confirm.go`、`internal/agent/fs/checkpoint.go` |
| 记忆与技能进化 | `internal/rag`、`internal/agent/skill`、`internal/agent/evolve` |
| 上下文工程 | `internal/agent/tool_state.go`、`toolsys/spill.go`、`docs/agent/08-eino-runtime.md` |
| 规则 / Git / journal / 沙箱策略 | `internal/agent/rules`、`internal/agent/toolkit/git.go`、`internal/agent/runstore`、`internal/agent/sandbox` |

## 8. 实现状态（2026-10-25）

对照第 4 节。GPT 的未提交改动保留为 `598c164`。下表的「现在」是其后补上的后端。

| 项 | GPT 之前 | 现在 | 关键文件 | 还没做 |
| --- | --- | --- | --- | --- |
| P0-1 SQLite 默认 | 未开始（只有 MySQL） | 后端完成 | `internal/model/driver.go`、`sqlite.go`、`export.go`、`lock.go`；`GET/POST /api/agent/data/export|import`；`GET /api/setup/status` | 首次启动向导界面。已有环境只要 `DB_HOST` 等非空且未写 `DB_DRIVER`，仍走 MySQL |
| P0-2 AGENTS.md | 部分：向上查找、注入、单测 | 完成预算共享、`/init`、验证命令解析 | `internal/agent/rules`、`init_agents_md`、`POST /api/agent/rules/init` | 规则不能改权限，这是提示词约定，不是沙箱强制 |
| P0-3 Git + diff + 验证 | 部分：status/diff/log/show/add/commit，无 push | 脏工作区提醒、write/edit 后跑一次 verify、diff 与单文件恢复 API | `internal/agent/toolkit/git.go`、`dirty.go`、`rules/verify.go`、`GET /api/agent/diff`、`POST /api/agent/restore` | diff 审阅面板。恢复要带会话 id，才能找到对应检查点 |
| P0-4 用量与缓存 | 未开始 | 完成记账、SSE `usage`、Claude `cache_control`、已知模型估价 | `internal/llm/usage.go`、`cache.go`、`internal/model/usage.go`、`GET /api/agent/usage` | 输入框旁的用量条。缓存开关默认只作用于 Claude |
| P1-5 run journal | 部分：JSONL 读写，未接线 | 启动对账、工具效果写入、不重放、确认/放弃 | `internal/agent/runstore/reconcile.go`、`internal/web/runs.go`、`internal/app/server.go` | 后台任务面板界面 |
| P1-6 OS 沙箱 | 未开始 | 策略门 + 审计 JSONL。`AGENT_SANDBOX=1` 时替换命令白名单，并拒绝 cmd/powershell | `internal/agent/sandbox`、`internal/web/run_command.go` | MXC、Landlock、WSL、AppContainer。关着开关时仍是白名单 |
| P1-7 浏览器 | 未开始 | 静态抓取 + 内存 DOM：open/snapshot/click/type/screenshot/network。密码框拒绝输入 | `internal/agent/toolkit/browser.go` | Chrome / CDP / 已登录浏览器。截图返回可访问性文本 |
| P1-8 评测与 trace | 未开始 | `AGENT_TRACE_DIR` 才写 JSONL span；`evals/` 有脚本任务 | `internal/obs`、`evals/harness_test.go` | 20~30 条真模型任务、瀑布图、发版 smoke |
| P2 MCP | 未开始（原有 stdio） | URL、权限声明、`GET /api/agent/mcp/health` 探测 | `internal/agent/mcps/config.go`、`probe.go` | Streamable HTTP 会话、市场、OAuth |
| P2 技能元数据 | 未开始 | `version` / `source` / `allowed-tools`，不改 `ParseMarkdown` | `internal/agent/skill/meta.go` | 技能市场与导入导出 |
| P2 产物 | 未开始 | journal 里带路径的工具结果可列出 | `GET /api/agent/runs/:id/artifacts` | 预览和一键打开 |
| P2 钩子 | 未开始 | `.moon/hooks.json` 的 PreToolUse / PostToolUse / Stop。没有文件则不改变行为 | `internal/agent/hooks` | 格式化类钩子的产品界面 |
| P2 记忆来源 | 时间衰减原本已有 | `memory_write` 可选 `source`，写进正文开头 | `internal/rag/memory_tool.go` | 冲突合并、图记忆 |
| P2 文件触发 | 未开始 | 快照对比 API，`auto_run` 恒为 false | `internal/agent/trigger`、`GET /api/agent/triggers/files` | 不自动开跑，避免变成无人值守 |

明确留下的：首启向导、用量条、diff 面板、操作系统级沙箱、CDP 浏览器、MCP 全双工会话、多端、macOS、语音。这些要么是界面，要么依赖当前构建里没有的系统能力。文件变化只报告，不启动 agent。
