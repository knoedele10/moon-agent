# blog-my

Windows 私人 AI 助手：Electron 桌面端 + Go/Gin 后端，支持多模型聊天、流式工具调用、RAG 记忆、用户画像、助手人格、子 Agent 与团队、CUA。

## 开始开发

需要 Go 1.25.3 或兼容版本、Node.js 24+、npm 和可连接的 MySQL。使用本地模型时另需 Ollama；CUA 另需 Python 与 `cua/requirements.txt` 中的依赖。

```powershell
npm ci
Copy-Item .env.example .env
```

在 `.env` 填写 MySQL 连接信息，并设置固定、非空的 `APP_SECRET`。初始化会自动建库与迁移表，因此数据库账号需要相应权限。已有库以 `internal/model/` 中的 GORM 定义为准；`db/` 保存历史 SQL 参考。

```powershell
npm run dev
```

开发模式先构建后端，再启动桌面应用。只启动已构建的桌面端使用 `npm start`。只运行 HTTP 服务可在仓库根目录执行 `go run ./cmd/blog-my`，默认地址为 `http://127.0.0.1:8080`。

开发版优先使用根目录 `.env`；安装版使用 Electron 用户配置目录下的 `.env`，首次启动自动生成。`APP_SECRET` 用于加密已保存的 API Key，升级时需保留原值。安装包不包含仓库 `.env`。

## 项目结构

```text
cmd/
  blog-my/           后端可执行入口
  skillimport/       技能导入命令
internal/
  app/               应用组装、启动和退出
  web/               HTTP、SSE、用户配置、工具权限与业务接线
  agent/             Eino/legacy 运行器、共享执行保护、工具及子 Agent
  llm/               模型目录、供应商适配、默认注册
  model/             GORM 数据模型、数据库初始化和历史迁移
  rag/               入库、切块、向量/关键词融合检索与记忆
electron/           桌面窗口、后端进程与用户配置
templates/          页面与静态资源
cua/                Python 电脑操作能力
packaging/          CUA 打包定义
scripts/            构建、检查与验收入口
skills/             随应用发布的技能
docs/               当前文档、架构决策与历史验证记录
examples/           独立 mock 应用及云函数示例
artifacts/          用户文档、提取文本与附件（本地保留，不入库）
db/                 历史 SQL 参考
```

`bin/`、`build/`、`dist/`、`node_modules/`、`workspace/` 是生成内容或运行数据。构建缓存放在 `build/`；其中的嵌套 `go.mod` 隔离旧模块缓存，避免根目录 Go 命令扫描依赖源码。

详细调用关系、数据位置和改动说明见 [项目架构](docs/architecture.md)、[本轮整理记录](docs/verify/refactoring-2026-10-19.md)。

## 常用命令

| 命令 | 用途 |
| --- | --- |
| `npm run build` / `npm run build:go` | 单次构建 `./cmd/blog-my`，输出 `bin/blog-my.exe` |
| `npm test` | 工具链、Electron、Go 单元测试、离线页面回归 |
| `npm run test:go -- -count=1 -timeout=180s` | 强制重跑后端测试；默认跳过需外部服务的测试 |
| `npm run test:ui:taskcard` | 额外运行任务卡回归 |
| `npm run check` | 工具链检查及 Go vet |
| `npm run dist:dir` | 构建后端与 `dist/win-unpacked/` 桌面目录 |
| `npm run dist` | 构建 Windows 安装包 |
| `npm run build:cua-sidecar` | 用 PyInstaller 构建 CUA sidecar |

Go 构建脚本默认使用工作区缓存，同时尊重已有 `GOCACHE`、`GOMODCACHE`、`GOTMPDIR`、`TEMP`、`TMP` 设置。测试脚本从项目依赖加载 jsdom，不依赖开发者个人目录。

打包前如需 CUA，先安装 Python 依赖与 PyInstaller，再生成 sidecar。桌面壳目前只使用 Electron 与 Node 内置模块，打包 hook 会跳过不存在的运行时 Node 依赖；以后添加 dependencies / optionalDependencies 时自动恢复常规依赖处理。打包资源为 `bin/`、`templates/`、`skills/`；示例应用、文档附件和构建缓存不发布。

## Agent 与记忆

会话用途（聊天、工作、代码、学习）控制可用工具；执行模式 `plan`、`confirm`、`auto` 控制操作权限。工作区可以有多个活动目录，别名由工具的路径适配层解析。具体约束见 [使用说明](docs/agent/03-usage.md)。

默认启用 Eino；`AGENT_ENGINE=legacy` 可回退对照。两个运行器共享工具安全执行规则；预算、续行和流式事件见 [Eino 运行说明](docs/agent/08-eino-runtime.md)。

聊天、用户画像、人格配置、记忆原文和向量都保存在 MySQL。知识库采用切块、向量与关键词/实体融合检索；画像属于用户级摘要配置，不是独立向量数据库。目录迁移不会搬运 MySQL 数据。

## 真实服务验证

这些检查需要运行中的服务，默认不属于 `npm test`。使用专用测试库，避免初始化对账影响日常数据：

```powershell
$env:DB_NAME = 'blog_my_refactor_test'
$env:AGENT_IT = '1'
node scripts/go.cjs test -run 'TestChatHTTP|TestAutonomousHTTPExecution|TestPermHTTPMatrix' -count=1 -timeout=180s
Remove-Item Env:AGENT_IT
Remove-Item Env:DB_NAME
```

`KB_STORE_IT=1` 配合显式 `DB_NAME` 启用真实 MySQL 固定向量检索测试；`KB_IT=1` 启用真实 MySQL + Ollama 记忆集成；`AGENT_LIVE_MODEL` 指定已安装的 Ollama 模型可启用真实模型文件任务测试。真实服务检查、模拟模型检查与供应商可用性分别记录，不把离线回归当作真实模型验收。
