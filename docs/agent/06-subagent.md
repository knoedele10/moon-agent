# 子 agent 委派（v1）设计与契约

照 `docs/agent/05-subagent-and-team.md` 里 deepseek-harness 的做法，给 blog-my 加**子 agent 委派**。
v1 只做委派与完成通知，不做团队（roster / inbox / 任务板），那部分留到 v2。

## 一、用户看到什么

1. 用户说「把这三份文档分别总结一下，派三个子 agent」，主 agent 调 `subagent` 工具，**立即**返回
   「已派出去 #12」并继续说话，不阻塞。
2. 父会话里出现一张**子任务卡片**：标签、状态（运行中 / 完成 / 失败）、耗时、可点开看子会话。
3. 子会话是一条真实会话，在侧栏里缩进挂在父会话下面。
4. 子 agent 跑完 → 往父会话落一条 `role=subagent` 的**完成通知**（卡片形式）。
5. 父级所有子任务都结束后，**自动唤醒父 agent** 跑一轮（把这批通知读进来做汇总），结果作为普通 assistant 消息落库。

## 二、分层（和参考实现一一对应）

| 参考实现 | 我们 |
| --- | --- |
| `subagent` 工具立即返回 id | 同左，工具在 `agent/subagent.go` |
| `SubagentManager` 管准入/容量/结算 | `web/subagent.go` 的 `subagentRunner`（实现 `agent.Delegator`） |
| 提供方（spawn/fork） | 用同一个 `agent.Run` 跑，`context: fresh` 只给任务；`fork` 附带父级历史前缀 |
| 完成通知（user 角色） | 父会话里一条 `role=subagent` 消息，历史重建时映射成 user 消息喂模型 |
| `maxDepth` / 容量池 | `AGENT_MAX_DEPTH`（默认 2）/ `AGENT_MAX_CHILDREN`（默认 4，按 root 会话统计运行中的） |
| `subagent/catalog` | `agent_subagent_run` 表（父子关系 + 状态 + 收尾文本） |
| 权限快照 + 审批钉死 | 子级继承父级工作区与 kind；**子级的风险工具一律直接拒绝**，除非父级是 auto 档 |

## 三、冻结的契约（三个 worker 都按这个写）

### 3.1 `agent/subagent.go`（worker A 写）

```go
// SubagentRequest 是一次委派请求。
type SubagentRequest struct {
    ParentSessionID string         // 父会话 id（十进制串）
    RootSessionID   string         // 整棵树的根会话 id（容量按它统计）
    Depth           int            // 父级深度；子级深度 = Depth + 1
    Label           string         // 3-5 个词的标签，用于卡片标题
    Prompt          string         // 交给子 agent 的完整任务（必须自足）
    Context         string         // "fresh" | "fork"
    History         []llm.Message  // fork 时父级已完成轮次的前缀；fresh 为空
    Model           string         // 子级模型（继承父级）
    Kind            string         // chat/work/code/learn（继承父级）
    Root            string         // 工作区根目录（继承父级）
    Mode            string         // 父级档位；子级据此决定风险工具是否直接拒绝
}

// SubagentHandle 是「已受理」回执。
type SubagentHandle struct {
    ChildID string // 子会话 id（十进制串）
    Label   string
    Depth   int
}

// SubagentInfo 是 list_subagents 返回的一行。
type SubagentInfo struct {
    ChildID    string
    Label      string
    Status     string // running | done | failed | aborted
    StopReason string
    Depth      int
    Reply      string // 收尾文本（已截断，最多 2000 字）
    Error      string
    StartedAt  time.Time
    FinishedAt *time.Time
}

// Delegator 由 web 层注入（agent 包不能 import web）。
type Delegator interface {
    Delegate(ctx context.Context, req SubagentRequest) (SubagentHandle, error)
    ListSubagents(ctx context.Context, parentSessionID string) ([]SubagentInfo, error)
}

// SubagentNotice 是一条完成通知的内容。
type SubagentNotice struct {
    ChildID    string `json:"child_id"`
    Label      string `json:"label"`
    Status     string `json:"status"`
    StopReason string `json:"stop_reason,omitempty"`
    Reply      string `json:"reply,omitempty"`
    Error      string `json:"error,omitempty"`
    DurationMS int64  `json:"duration_ms"`
}

// LLMText 把通知渲染成喂给父级模型的文本。
func (n SubagentNotice) LLMText() string
```

工具：

- `SubagentTool{Delegator, Depth, MaxDepth, Mode}`，`Name() = "subagent"`，
  参数 `{description: string(必填), prompt: string(必填), context: "fresh"|"fork"(可选，默认 fresh)}`。
  **Execute 立即返回**，`Content = "已派出子任务 <label>（id=12），它跑完会自动通知你，先继续别的事。"`，
  `Display = {"subagent": {"child_id":"12","label":"...","status":"running"}}`。
  深度超限时返回 `Result{IsError:true, Content:"子 agent 最多嵌套 N 层，这一层不能再派活。"}`（不抛 error）。
- `ListSubagentsTool{Delegator}`，`Name() = "list_subagents"`，无参数，返回父级所有子任务的一行行文本 + `Display`。

### 3.2 `model/subagent.go`（worker B 写）

表 `agent_subagent_run`，结构体 `AgentSubagentRun`：
`Id / UserID / ParentConversationID(index) / ChildConversationID(uniqueIndex) / RootConversationID(index) /
Depth / Label(size 120) / Prompt(longtext) / ContextMode(size 16) / Status(size 16,index) /
StopReason(size 32) / Reply(longtext) / ErrText(text) / Model(size 128) / Kind(size 16) /
CreatedAt / UpdatedAt / FinishedAt *time.Time`，`TableName() = "agent_subagent_run"`。
状态常量：`SubagentRunning/Done/Failed/Aborted`。注册进 `model/code.go` 的 AutoMigrate。

### 3.3 `web/subagent.go`（worker B 写）

```go
type subagentRunner struct { /* 依赖注入或直接用 model.DB */ }
func newSubagentRunner() *subagentRunner            // 或包级单例
func (r *subagentRunner) Delegate(ctx context.Context, req agent.SubagentRequest) (agent.SubagentHandle, error)
func (r *subagentRunner) ListSubagents(ctx context.Context, parentSessionID string) ([]agent.SubagentInfo, error)
func subagentMaxDepth() int                          // AGENT_MAX_DEPTH，默认 2
func subagentMaxChildren() int                       // AGENT_MAX_CHILDREN，默认 4
func runningChildren(rootID uint) int                // 统计 running
func subagentRunsFor(convID uint) ([]model.AgentSubagentRun, error) // 给接口用
```

`Delegate` 必须：
1. 校验 depth（`req.Depth+1 > subagentMaxDepth()` → 返回 error）、容量（`runningChildren(root) >= subagentMaxChildren()` → error）。
2. 建子会话 `model.Conversation{UserID, Title: label, Model: req.Model, Kind: req.Kind}`（kind 用**现有四值**之一，不要造新 kind）。
3. 建 run 行（status=running）。
4. 起 goroutine：拼 `agent.Config`（工具用 `agentToolsFor(userID, req.Root, req.Mode, false, req.Depth+1)` 过滤 kind，
   `Exec{SessionID: 子会话 id, Root: req.Root, ReadRoot: agentSkillsDir()}`），历史 = `req.History + user(prompt)`，
   `agent.Run(ctx, cfg, history, emit)`（emit 只收集文本，不推 SSE）。
5. 收尾：更新 run 行（status/stopReason/reply/err/finishedAt）、往**父会话**落一条 `role="subagent"` 的消息
   （内容 = `agent.SubagentNotice{...}.LLMText()`，**同时把结构化 JSON 放 Display**——见 3.5 的约定），
   `touchConversation(parent)`；若该父会话此刻没有 running 的子任务，调 `wakeParentAfterSubagents(parentConvID)`。
6. 子级风险工具策略：`req.Mode != "auto"` 时，子级工具集里风险工具**不包 confirmTool 而是直接替换成拒绝壳**
   （`subagentDenyTool{inner}`：Execute 返回 `IsError:true`，文案「子 agent 不能执行需要用户确认的操作，
   请把这件事写进结论让主 agent 处理」）。这个壳定义在 `web/subagent.go`，不要改 `web/agent_confirm.go`。

### 3.4 `web/agent.go` 的接缝（root 自己改）

- `agentToolsFor(userID, root, mode string, confirmed bool, depth int)` 增加 `depth` 参数；
  `depth < subagentMaxDepth()` 且 mode != plan 时追加 `agent.SubagentTool{...}` 与 `agent.ListSubagentsTool{...}`。
- `runAgentChat` 的签名从 `(c *gin.Context, ...)` 改成 `(in turnInput)`（字段：ctx / userID / conv / modelID / key /
  content / history / assistantID / kind / params / sendEvent / headless）。`postChat` 侧照旧从 gin 取值填进去。
  `headless=true` 时跳过 `saveAutoMemory` 与 `noteTurnAndMaybeProfile`。
- 新增 `func wakeParentAfterSubagents(convID uint)`：读父会话历史 → 用 `turnInput{headless:true}` 跑一轮，
  content 用「子任务都结束了，请汇总结果」这类固定提示；失败只记日志，不打扰用户。

### 3.5 通知消息的落库格式（前后端约定，**冻结**）

`message.role = "subagent"`，`message.content` = `SubagentNotice.LLMText()` 的纯文本。
结构化字段不塞进 content，而是**由前端调 `GET /api/conversations/:id/subagents` 拿**（同一份 run 行）。
`rebuildLLMHistory`（`web/chat.go`）遇到 `role="subagent"` 时，把它当成**一条 user 消息**喂给模型（内容就是 content）。

### 3.6 接口（root 自己加路由）

- `GET /api/conversations/:id/subagents` → `{"runs":[{"child_id":12,"label":"...","status":"running",
  "stop_reason":"","reply":"","error":"","depth":1,"started_at":"...","finished_at":null}]}`
- 会话列表 `GET /api/conversations` 每项追加 `"parent_id": 0, "parent_title": ""`（子会话才非空）。

### 3.7 前端（worker C 写）

- `chat.js` 的 `consumeSSE` 增加 `subagent` 事件分支（payload 同 3.6 的 run 形状），渲染成**子任务卡片**。
- 一轮结束后若当前会话有 running 子任务，每 4 秒轮询 `GET /api/conversations/:id/subagents`，
  刷新卡片；发现状态变化就重新拉一次消息列表（新通知/汇总会出现在对话里）。
- 侧栏 `renderConvList` 支持缩进显示子会话（`parent_id` 非空 → 加 `.conv-item.is-child` + 小徽标）。
- 历史回放：`role="subagent"` 的消息渲染成同一张卡片（复用 `renderSubagentCard`）。
- CSS 加在 `style.css` 末尾（`.subagent-card` 系列）。

## 四、不做的（v2）

`send_message` 给运行中的子级、`wait_agent`、任务板、具名团队、跨进程提供方、子级结构化输出。
