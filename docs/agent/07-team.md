# 团队层（v2）设计与契约

v1（子 agent 委派）已完成，见 `docs/agent/06-subagent.md`。本文只写**新增**的团队层：
具名成员、成员间消息、等待、共享任务板。参考实现的做法见 `docs/agent/05-subagent-and-team.md` 第二节。

## 一、冻结的接口（三个 worker 都按这个写）

### 1.1 `agent/team.go`（worker A 写，工具层）

```go
// TeamMember 是 roster 里的一行。
type TeamMember struct {
    Name        string // 小写短名，父会话内唯一，永不复用
    Description string
    Role        string // lead | teammate
    Status      string // running | inactive | failed
    Depth       int
    ChildID     string // 十进制串；lead 为空
    Model       string
}

// TeamTask 是任务板上的一条。
type TeamTask struct {
    ID          string   // task-1、task-2…
    Revision    int      // CAS 用；每次变更 +1
    Subject     string
    Description string
    Status      string   // pending | in_progress | completed
    Owner       string   // 成员名；空=无人认领
    BlockedBy   []string // 依赖的任务 id
    WriteScopes []string // 提示性路径前缀，不加锁
    Ready       bool     // 依赖是否都完成了
}

// TeamService 由 web 层实现（agent 包不能 import web）。
type TeamService interface {
    SpawnTeammate(ctx context.Context, req TeammateRequest) (TeamMember, error)
    Roster(ctx context.Context, parentSessionID string) ([]TeamMember, error)
    SendMessage(ctx context.Context, parentSessionID, from, target, message string) error
    WaitForChange(ctx context.Context, parentSessionID string, timeoutMS int) (changed bool, err error)
    CreateTask(ctx context.Context, parentSessionID string, t TeamTaskInput) (TeamTask, error)
    ListTasks(ctx context.Context, parentSessionID string, filter TaskFilter) ([]TeamTask, error)
    GetTask(ctx context.Context, parentSessionID, id string) (TeamTask, error)
    UpdateTask(ctx context.Context, parentSessionID, id, actor string, expectedRevision int, action string, patch TeamTaskInput) (TeamTask, error)
}

type TeammateRequest struct {
    ParentSessionID string
    RootSessionID   string
    Depth           int
    Name            string
    Description     string
    Prompt          string
    Context         string // fresh | fork
    History         []llm.Message
    Model, Kind, Root, Mode string
}
type TeamTaskInput struct {
    Subject, Description string
    BlockedBy   []string
    WriteScopes []string
    Owner       string
}
type TaskFilter struct {
    Status string // 空=全部
    Owner  string
    Ready  *bool
}
```

工具（都在 `agent/team.go`，实现 `agent.Tool`）：

| 工具 | 参数 | 语义 |
| --- | --- | --- |
| `spawn_teammate` | `name`* `description`* `prompt`* `context?` | 建一个**具名**成员；名字非法/重名 → IsError |
| `send_message` | `target`* `message`* | 给成员发消息（见 2.2）；成功只表示**已收下** |
| `list_agents` | 无 | roster 一行行文本 + `Display.team` |
| `wait_agent` | `timeout_ms?`（10000..3600000，默认 30000） | 等下一次变化；只报有没有变化 |
| `team_task_create` | `subject`* `description`* `blocked_by?` `write_scopes?` | 建 pending 任务 |
| `team_task_list` | `status?` `owner?` `ready?` | 列任务（一行一条 + Display） |
| `team_task_get` | `task_id`* | 读一条 |
| `team_task_update` | `task_id`* `expected_revision`* `action`* `subject?` `description?` `blocked_by?` `write_scopes?` `owner?` | action ∈ claim/release/complete/reopen/edit/set_dependencies/reassign/delete |

- 所有工具在 `Service == nil` 或服务报错时返回 `Result{IsError:true}`，**不抛 error**。
- `actor` 从 `Exec.SessionID` 推：根会话 → `lead`，子会话 → 它自己的名字（web 层提供 `ActorFor(sessionID)`，见下）。
  简化：工具直接用 `Exec.SessionID` 作为 actor 传给服务，服务内部再解析成名字。

### 1.2 `model/team.go` + `web/team.go`（worker B 写）

- 表 `agent_team_task`：`Id / UserID / TeamConversationID(index) / TaskNo(int) / Subject / Description /
  Status / Owner / BlockedBy(text,JSON) / WriteScopes(text,JSON) / Revision(int) / CreatedAt / UpdatedAt`，
  `TableName()="agent_team_task"`；`ID` 对外呈现为 `task-<TaskNo>`。注册进 AutoMigrate。
- `agent_subagent_run` 加一列 `Name`（size 64，索引）：成员名。**AutoMigrate 会自动加列**。
  名字规则：`^[a-z][a-z0-9-]{1,31}$`，父会话内唯一（含已结束的），永不复用。
- `web/team.go` 实现 `agent.TeamService`：
  - `SpawnTeammate`：校验名字 → 走 v1 的 `subagentRunner.Delegate`（填 Name）→ 返回 roster 行。
  - `Roster`：lead（父会话本身）+ 该父会话的所有子 run。
  - `SendMessage`：把消息以 `role=user` 落进**目标子会话**（前缀 `【来自 <from>】`），
    若目标当前不是 running，则起一轮续跑（新的 run 行、同一个子会话、depth 不变）；
    返回错误仅当目标不存在/不是本团队成员。
  - `WaitForChange`：每 500ms 查一次该父会话的子 run 状态快照，变了就返回 true；超时返回 false。
  - 任务板：CAS（`expected_revision` 不匹配 → 错误）、`blockedBy` 必须指向存在且未删除的任务、
    claim 要求 pending 且依赖全完成、complete 需要是 owner、delete 留 tombstone（Status=deleted）。
- `newTeamService()` 供 `agentToolsFor` 用；`TeamTaskView` 转 `agent.TeamTask` 的映射写在 web 层。

### 1.3 前端（worker C 写）

- `GET /api/conversations/:id/team` → `{"root_id":1,"members":[{name,description,role,status,depth,child_id,parent_id,model,activity,progress}],
  "tasks":[{id,revision,subject,status,owner,blocked_by,write_scopes,ready}]}`。成员数组按整棵委派树排序，包含匿名子 agent 和子子 agent；从子会话打开时也返回同一棵树。
- 对话页右上角的「当前任务」卡片（`taskcard.js`）加一段**团队**区域：成员一行一个（名字 + 状态点 + 角色），
  任务一行一个（`task-1` + 标题 + 状态 + owner）。没有成员/任务时整段隐藏。
- 团队中仍有运行中的子 agent 时，面板每 2.5 秒更新成员状态和进度；全部结束后停止轮询。
- `scripts/verify-ui.cjs` 加 2 条断言：喂一条 roster/task 数据后卡片里出现成员与任务行；空数据时整段隐藏。

## 二、语义（必须写进工具描述，模型靠它决策）

1. `spawn_teammate` 只在**明确要求团队/成员**时才用（普通任务不要自己组队）。
2. `send_message` 成功只表示「消息已进对方收件箱」，**不表示对方已处理**；要看结果得 `list_agents` + `wait_agent`。
3. 给**运行中**的成员发消息 = 排队（它这一轮结束后会读到）；给**已结束**的成员发消息 = 冷启动一轮续跑。
4. `wait_agent` 只报「有没有变化」，调用方要自己重新 `list_agents` 读状态；没有活跃成员时立即返回 `changed=false`。
5. 任务板用 `expected_revision` 做 CAS：拿到旧快照再提交会被拒（`TEAM_TASK_STALE_REVISION`），
   这是为了两个成员不会互相覆盖成果。`write_scopes` 只是提示，**不加锁**。
6. 只有 Lead（根会话）能 `spawn_teammate`；成员之间可以互发消息、都能用任务板。

## 三、v2 明确不做

跨进程成员、worktree 隔离、消息重试/去重、owner 自动释放、嵌套团队。
