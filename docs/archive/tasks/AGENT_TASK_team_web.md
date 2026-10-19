# 任务：实现团队服务（`model/team.go` + `web/team.go`）

先读 `E:\blog-my\docs\agent\07-team.md` 的 **1.2 与第二节**（冻结契约）。
再读 v1 的实现：`web/subagent.go`（`subagentRunner.Delegate` / `settle` / `runChild`）、`model/subagent.go`。

## 你负责的文件（只有四个）

1. `model/team.go`（新建，`agent_team_task` 表）
2. `model/code.go`（**只加两行**：AutoMigrate 列表里追加 `&AgentTeamTask{}`；`AgentSubagentRun` 加 `Name` 列由 AutoMigrate 自动完成，你只需在 `model/subagent.go` 里加字段——**那条也算你的文件**）
3. `model/subagent.go`（只加一列 `Name string` + 索引）
4. `web/team.go`（新建，实现 `agent.TeamService`）+ `web/team_test.go`（新建）

**不要动** `web/agent.go` / `web/subagent.go` / `web/chat.go` / `web/web.go` / `templates/`（root 自己改）。

## 要点

- `SpawnTeammate` 复用 v1 的 `subagentRunner.Delegate`（把 `agent.SubagentRequest` 填上 Name），不要重写一套。
  名字规则与唯一性校验按契约；重名（包括已结束的）必须报错。
- `SendMessage`：消息以 `role="user"` 落进目标子会话（前缀 `【来自 <from>】`），目标不在 running 就起一轮续跑
  （新 run 行、同一个子会话、depth 不变、Name 继承）。
- `WaitForChange`：每 500ms 查一次子 run 的状态快照（父会话下所有 run 的 id+status 拼成的指纹），变了返回 true。
- 任务板：CAS、依赖校验、claim/complete/reassign 的权限与前置条件、delete 留 tombstone，全部按契约。
- `newTeamService()` 供 `agentToolsFor` 调用。

## 单测（不连真 MySQL）

至少覆盖：名字校验、任务状态机（claim 要求 pending+依赖完成、complete 要求 owner）、CAS 拒绝旧 revision、
blocked_by 指向不存在任务时报错、write_scopes 只存不拦。

## 验收

- `go build ./...`（GOCACHE/GOTMPDIR 同 v1）；若因别人的文件没跟上而失败，稍后重试，**不要改别人的文件**。

完成后回报：文件清单 + 测试结果 + 缺口。
