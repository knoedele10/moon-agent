# 任务：实现 `agent/team.go`（团队工具层）

先读 `E:\blog-my\docs\agent\07-team.md` 的 **1.1 与第二节**，那是冻结契约（类型/工具名/参数/语义照拄）。
也看一眼 `E:\blog-my\agent\subagent.go`（v1 工具层的写法，风格要一致）。

## 你负责的文件（只有两个）

1. `agent/team.go`（新建）
2. `agent/team_test.go`（新建）

**不要动** 其它任何文件（别人在并行改）。

## 要点

- 8 个工具全部实现 `agent.Tool`：`spawn_teammate` / `send_message` / `list_agents` / `wait_agent` /
  `team_task_create` / `team_task_list` / `team_task_get` / `team_task_update`。
- 每个工具的 `Parameters()` 返回的 JSON Schema 里，**描述要写清楚语义**（特别是 send_message 的「已收下≠已处理」、
  wait_agent 的「只报有没有变化」、team_task_update 的 CAS）。模型只能靠描述决策。
- 所有失败路径（参数非法、Service 为 nil、Service 报错）→ `Result{IsError:true}`，**不抛 error**。
- 输出用紧凑一行行文本（参考 `agent/subagent.go` 的 `ListSubagentsTool`），同时在 `Display` 里带结构化数据
  （`Display["team"]` / `Display["tasks"]` / `Display["task"]`），前端和测试都靠它。
- `actor` 直接用 `ex.SessionID`（web 层会把它解析成成员名，你不管）。
- 单测（不连 DB，自己写假 Service）：schema 形状、参数校验、Service 报错折 IsError、
  roster/task 的文本与 Display 形状、wait_agent 超时（changed=false）时的文案、CAS 失败的错误文案。

## 验收

- `go vet ./agent/...` 与 `go test ./agent/...` 必须过（需要 `$env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'`）。
- `web/` 可能因为别人还没改完而编不过，不影响你。

完成后回报：文件清单 + 测试结果 + 留下的缺口。
