# 开发与项目结构约定

本项目使用 Go 服务、Electron 桌面壳和浏览器界面。保留一个 Go 模块，应用代码放在 `internal/`，独立程序入口放在 `cmd/`。布局参考 [Go 官方模块组织指南](https://go.dev/doc/modules/layout)，桌面进程边界参考 [Electron 进程模型](https://www.electronjs.org/docs/latest/tutorial/process-model)。

## 代码放在哪里

| 目录 | 职责 | 新增代码规则 |
| --- | --- | --- |
| `cmd/` | 可执行入口与进程信号 | 不写聊天、工具和数据库业务 |
| `internal/app/` | 配置、数据库、路由、服务启停 | 统一管理长生命周期资源 |
| `internal/web/` | 鉴权、HTTP/SSE、用途与权限装配 | 不把学习算法和 MCP 协议塞入 handler |
| `internal/agent/` | 执行循环、预算、事件、团队协议 | 不依赖 `web` 或 Gin |
| `internal/agent/toolsys/` | 工具目录、参数处理、大结果 | 通过 `agent.Tool` 接口组合 |
| `internal/agent/evolve/` | 轨迹、候选、技能质量、反馈与撤销 | 通过 `SkillStore` 隔离数据库，核心逻辑可离线测试 |
| `internal/agent/mcps/` | MCP 进程与协议适配 | 超时覆盖完整请求，关闭时释放进程和管道 |
| `internal/llm/` | 模型供应商适配与注册表 | 不在这里做用户权限判断 |
| `internal/model/` | MySQL 模型与持久化 | 查询必须限定用户，先复用已有表 |
| `internal/rag/` | 文档、切块、向量及混合检索 | 与结构化学习记忆分别保存、分别注明来源 |
| `electron/` | 窗口、后端进程、明确的 IPC 桥 | 页面通过 preload 暴露的窄接口访问本机能力 |
| `templates/` | 页面、样式、浏览器交互 | 学习结果用真实接口数据呈现，不在前端伪造统计 |
| `scripts/` | 构建、验证、打包辅助 | 与业务运行时代码分开 |
| `docs/adr/` | 架构决策 | 说明问题、决定和边界 |
| `docs/verify/` | 可复现验收记录 | 写清离线、真实数据库、真实模型各验证到了哪里 |

`bin/`、`build/`、`dist/`、缓存、`workspace/` 和用户配置都是生成或运行数据；不得作为源代码提交。`references/` 是忽略的上游研究快照，不作为产品依赖。保留已有用户文件，不通过整理目录删除运行数据。

## 改动流程

1. 先读实际调用路径及最近测试。已有未提交修改属于当前工作，不能重置覆盖。
2. 修改核心逻辑时先确定模块接口；相同策略只保留一个实现。主会话、子会话和确认重放统一走 `assembleRuntimeTools`。
3. 新工具必须说明输入、输出、副作用、取消和错误行为；新增工具不能绕过用途、执行档位、用户或工作区边界。
4. 测试放在同包的 `*_test.go`；真实服务测试显式开关并使用独立数据库。模型桩通过只代表流程验证，不等于真实供应商质量验证。
5. 文档与示例配置同步更新。部署配置、密钥、数据库内容及学习轨迹不得提交。

## 验证

```powershell
go build ./...
go vet ./...
go test ./... -count=1 -timeout 180s
npm run test:tooling
npm run test:electron
npm run test:ui
```

Windows 缓存或临时目录被限制时，运行 `scripts/verify-hermes.ps1`，或将 `GOCACHE`、`GOTMPDIR`、`TMP`、`TEMP` 指向仓库内已忽略的验证目录。

学习闭环的真实 HTTP/MySQL 验收：先按 `.env.example` 配好数据库服务，再显式指定独立库名：

```powershell
$env:AGENT_IT = '1'
$env:DB_NAME = 'moon_agent_hermes_test'
go test ./internal/web -run 'TestHermesLearningLifecycleHTTP|TestHermesReview' -count=1 -v -timeout 120s
```

更详细的运行时设计见 [Agent 能力装配决策](docs/adr/0002-agent-capability-assembly.md)，使用方式见 [Hermes 能力指南](docs/hermes-parity/04-usage.md)。
