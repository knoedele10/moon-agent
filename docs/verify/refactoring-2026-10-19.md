# 项目结构与代码优化验收记录

日期：2026-10-19。工作区：`E:\blog-my`。本轮修改源码与构建流程，没有提交 Git。

## 1. 结构整理

采用应用入口 `cmd/` 与内部实现 `internal/` 的布局（[Go 官方模块布局说明](https://go.dev/doc/modules/layout)）。没有给现有应用额外套一组只有转发作用的分层接口。

| 原位置 | 当前位置 |
| --- | --- |
| `main.go` | `cmd/blog-my/main.go`，组装逻辑在 `internal/app/server.go` |
| `agent/`、`llm/`、`model/`、`rag/`、`web/` | `internal/` 下同名目录 |
| `model/code.go`、`model/data.go` | `internal/model/db.go`、`internal/model/chat.go` |
| `deploy/`、`cloudbase/` | `examples/mock-web/`、`examples/cloudbase/` |
| 根目录 AGENT_TASK 文档 | `docs/archive/tasks/` |
| 作业 DOCX、提取文本、图片附件 | `artifacts/documents/` |
| 早期使用验收快照 | `docs/archive/agent-usage-2026-10-12.md` |

同步修正 Go import、集成测试读取 .env 的相对路径、构建命令和验收脚本位置。`go list ./...` 只列出 11 个项目包，不再扫描 `build/gomodcache`。

`bin/`、`build/`、`dist/`、`node_modules/`、`workspace/` 和历史缓存目录被忽略。用户文档归档保留；技能模板是有效发布资源，没有因文件多而删除。

## 2. 实际实现改动

### 应用启动

- 进程入口负责信号与错误退出，`app.Run(ctx)` 组装环境、数据库、供应商注册、Gin 与 HTTP 服务。
- `model.InitDB()` 返回错误，由入口决定如何报告；初始化不再从数据层直接终止进程。
- 关闭临时建库连接，失败迁移关闭连接；正常应用退出释放数据库池。
- 主动聊天调度随应用启动和取消，移除导入包就启动的副作用。
- HTTP 有读取请求头和空闲连接超时；保留 SSE 所需的长响应，退出取消流式请求。
- 默认模型注册汇总至 `llm.NewDefaultRegistry()`；Grok/xAI 的目录仍使用现有 OpenAI 兼容适配。

### Agent 运行器

- `tool_state.go` 集中两引擎重复的只读重试、未知副作用保护、修改后验证计账。
- `model_retry.go` 集中模型重试分类和可取消等待，保留引擎之间不同的首片流式边界。
- 保留事件、图片、需要用户输入、预算、续行和工具消息配对。
- 新增跨引擎回归：一次只读重试、非幂等超时不重试、同批与后续轮次阻止副作用、成功与失败写的验证义务。
- 全量强制重跑暴露旧引擎取消竞争：流通道关闭与 context 取消同时就绪时 select 可能选择 EOF 并返回 nil。现在接收和结束路径重新检查取消；Eino 扫描与收尾也检查。新增确定性回归，并把相关用例连续运行 100 次。

### RAG

- 修复语义与关键词检索每块追加两次候选的问题；每块只生成一条含 Scope、重要性、活跃度、情绪与强化时间的候选。
- 查询分词、实体和反查文本每请求准备一次；元信息仅在本次请求内复用，下一次查询读取更新。
- 直接读取 float32 编码字节算余弦，减少每候选解码数组的内存分配，保留 float64 累加顺序。
- 不改变查询候选池大小、融合评分公式和 stale 过滤，不添加可能损害召回的 SQL LIMIT。
- 新增真实 MySQL 固定向量检索测试，不依赖 Ollama；验证去重、用户隔离、stale 过滤、完整元数据和请求间更新。

### 工具链与打包

- Go 构建只编译 `./cmd/blog-my` 一次，启用 trimpath；缓存和临时目录由共享模块管理。
- jsdom 加入项目开发依赖并锁定安装树；六处脚本移除个人目录硬编码，统一加载器。
- 新增 npm test / check / test:go / test:ui 与任务卡检查入口；Node 要求 24+。
- 当前桌面壳只有 Electron 与 Node 内置模块。beforeBuild hook 在没有运行时依赖时跳过依赖扫描；以后添加 dependencies 或 optionalDependencies 自动恢复常规处理。已加入回归防止漏打包依赖。
- 打包排除 Electron 测试文件，保留实际桌面代码、后端、模板和技能。

## 3. 验证证据

本轮迁移前目标 Go 包基线通过。修改后验证如下：

| 检查 | 结果与范围 |
| --- | --- |
| `npm test` | 工具链、13 项 Electron 测试、Go、3 组离线页面回归通过 |
| 最终 `test:go -json -count=1 -timeout=180s` | **643 项测试及子测试通过、20 项条件跳过、0 失败**；8 个有测试的包通过，model 与两个命令包无测试文件 |
| 取消回归 `-count=100` | 旧引擎流取消、已关闭/完成的取消流、Eino EOF 取消均通过 |
| 最终 `npm run check` | 3 项工具链测试、全后端 Go vet 通过 |
| `test:ui:taskcard` | 专项检查通过（工具链子任务执行） |
| 真实 MySQL HTTP 集成 | 3 项聊天兼容/工具入口、4 种自主执行场景、11 种权限场景通过；模型为 mock，文件读写真执行 |
| MySQL 画像集成 | `TestProfileMySQLIntegration` 通过，使用固定模型回调验证真实持久化与注入 |
| MySQL 检索集成 | `TestStoreMySQLRetrieval` 两路检索与数据隔离通过，输入向量为固定测试值 |
| `npm run dist:dir` | 成功更新 `dist/win-unpacked/` |
| 打包后端启动 | 实际打包 EXE：READY、健康接口、UTF-8 登录页、匿名 API 拒绝、注册会话、聊天页、含 xAI 的模型目录、静态 JS 通过 |
| 打包页面回归 | 从 resources/templates 加载的 3 组离线 UI 检查通过 |
| 发布资源完整性 | 后端、templates、skills 共 **13043** 个文件 SHA-256 对照一致；asar 中四个桌面源文件一致，不包含测试文件或根 .env |
| 迁移完整性 | 备份内 216 个文件按迁移清单均能找到，没有丢失项；gofmt 与 git diff --check 通过 |

专用测试库：`blog_my_refactor_20261019`。集成用例清理各自数据；打包启动冒烟注册了独立测试账号，保留在该测试库。测试没有操作日常业务库的用户数据。启动检查使用打包后的后端和 templates，配置来自本地环境，未自动打开 Electron 窗口；测试进程已结束。

原始证据保存在本地 `build/refactor/`：`npm-test.log`、`go-tests-final.jsonl`、`go-tests-summary.json`、`cancel-regression.log`、`check-final.log`、`mysql-http-final.log`、`mysql-rag-final.log`、`mysql-profile.log`、`package-final.log`、`package-smoke-final.log`、`packaged-ui.log`、`package-integrity.json`、`layout-audit.json`。这些是本地生成记录，不进入 Git。

## 4. 微基准

同机 1024 维向量、三次运行（`BenchmarkCosineEncoded`）：

| 实现 | ns/op（三次） | B/op | allocs/op |
| --- | --- | --- | --- |
| 原解码数组 | 2150 / 2093 / 2510 | 4096 | 1 |
| 直接读取编码 | 1317 / 998.3 / 934.6 | 0 | 0 |

中位数约快 2.15 倍。只代表余弦计算，不代表数据库查询、模型生成或完整聊天的总体提速。来源：`build/refactor/rag-benchmark.log`。

## 5. 验证边界与后续方向

- 本轮 Ollama 的 11434 端口未运行，没有测试真实本地模型推理或新的外部供应商请求。
- 页面行为采用 jsdom 与实际 HTTP 资源验证，没有做 Electron 窗口的人工视觉检查或 CUA 操作验收。
- 向量检索仍是 Go 进程扫描排序；大规模知识库需要独立测量和索引方案。现有 HTTP 包仍承担业务接线，未来可以按业务需求继续拆分。
- 数据库 AutoMigrate 和全局连接方式仍保留，未引入版本化迁移框架。
- 首次打包的 Node 依赖扫描遇到 spawn EPERM，通过符合当前零运行时 Node 依赖结构的官方 beforeBuild hook 解决。一次“启动并清理进程”的组合命令被自动审批拦截（仅返回 blocked by policy），改用独立启动、HTTP 检查与会话取消后完成验证。

## 6. 使用与恢复

新版程序：`E:\blog-my\dist\win-unpacked\blog-my.exe`。
开发入口：`npm run dev`；只运行服务：`go run ./cmd/blog-my`。

源码对照备份：`build/refactor/snapshots/2026-10-19-before-layout.zip`；迁移映射：`build/refactor/layout-moves.json`。备份不含 .env、数据库、缓存与运行数据。请按映射比较恢复，不要覆盖以后新增的修改。

架构索引见 [architecture.md](../architecture.md)，日常命令见根 [README](../../README.md)。
