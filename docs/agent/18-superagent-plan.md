# 18 · Superagent 实施步骤

契约见 [`18-superagent.md`](18-superagent.md)。三条工作线并行，最后统一集成。

## 第 0 阶段：公共骨架（已完成，开工前必须先提交）

- `internal/model/superagent.go`：7 张 `sa_*` 表与枚举，已加入 MySQL / SQLite 两条迁移。
- `internal/web/sa_contract.go`：跨线函数变量（默认桩）、共享类型、`saRouteRegistrars`。
- `internal/web/sa_contract_test.go`：`setupSATestDB`、`saSwap`、迁移冒烟测试。
- `web.go`：循环注册 `saRouteRegistrars`。

**开工前**：当前分支上长任务运行时（`task_*.go`、`model/task.go` 等）还没有提交，三条线都依赖它。
先把它和第 0 阶段一起提交，三个 agent 从这个提交各自开 worktree / 分支：

| 工作线 | 分支 |
| --- | --- |
| A 入口与渠道 | `feat/sa-intake` |
| B 判断与路由 | `feat/sa-router` |
| C 工单与执行 | `feat/sa-ticket` |

各线只改自己在契约 §6 里的文件。需要改契约（表、接口、语义）时**停下来说明**，不要自己改
`sa_contract.go` 或 `model/superagent.go`。

## 工作线 A：入口与渠道

目标：任何来源的消息都变成 Trigger；任何外发都经过发件箱。

1. `sa_intake.go`：实现 `saIngest`。Thread 找/建（QQ 来源同时建前台会话，标题沿用现在的「QQ · 昵称」「QQ 群 …」，
   `kind` 取 `QQ_BOT_KIND`）；Trigger 去重入库；调 `saFanOut`；未认领且非 Inline 时标 `queued` 并调 `saEvaluate`。
   把现有 `qq_chat_binding` 的绑定在首次遇到时迁成 `sa_thread`。
2. `sa_outbound.go`：实现 `saEnqueueOutbound` 与后台发送器（Thread 内保序、退避、`failed/dropped`、
   重启续发）。`app/server.go` 里启动/停止，和其它调度器一样。
3. `sa_channel_web.go`：网页渠道（写 assistant 消息，返回 message.id）。
4. `qq_onebot.go` 改造：消息 → `saTriggerIn`（`Addressed`：私聊 direct、@ mention、其余 none；
   群里没 @ 的也入库，交给 gate）；移除适配器里的白名单判断与直接跑轮次；保留 `/new`、`/help`。
   `sa_channel_qq.go`：发送带 `echo`，等回执取 `message_id` 作为 `sent_ref`（超时视为临时失败）；
   入站的 `reply` 段填 `ReplyToMsgID`。
5. `chat.go` 的 `postChat`：存完 user 消息后 `saIngest(Inline=true)`；被认领时 SSE 回「已转交任务 #n」+ done。
6. 只读接口：`GET /api/sa/threads`、`GET /api/sa/threads/:id/triggers`、`GET /api/sa/outbound?status=failed`。

验收：

- 同一 QQ 消息重复上报只入库一次；群里没 @ 的消息入库为 queued（由 gate 决定），不回话。
- 发件箱：同 Thread 第一条一直失败时第二条不发；永久错误直接 dropped；重启后 pending 继续发、不重发已发的。
- QQ 回执的 `message_id` 记进 `sent_ref`；入站引用回复能带出 `ReplyToMsgID`。
- 网页聊天在 B、C 未接入时行为与现在一致（扇出桩返回空）。
- `go test ./internal/web -run 'TestSAIntake|TestSAOutbound|TestQQ'` 全过。

## 工作线 B：判断与路由

目标：每条非认领消息经过 gate；前台会话决定回答还是接单。

1. `sa_policy.go`：`saPolicyFor`（首次按 `QQ_ALLOW_USERS/GROUPS`、`QQ_BOT_KIND` 建默认行，模式 `observe`）；
   `GET/PUT /api/sa/policy`（保存时 `version+1`）。
2. `sa_gate.go`：实现契约 §3.2 的表，纯函数，表驱动单测覆盖每一行。
3. `sa_router.go`：`saEvaluate`（`eval_state` 条件更新 + 租约；合并 queued；gate 过滤写 Decision；
   为通过的批次在前台会话跑一轮 `runAgentChat`，headless；回答经 `saEnqueueOutbound` 发回；
   超时/出错重排，三次后 `unavailable`；启动时重新安排遗留的 queued）。
4. `session_router.go`：`orchestrator_route` 换成契约 §3.4 的批量 decisions 版本，调 C 的 accept/attach/stop；
   逐项检查强度与模式；每项写 `sa_decision`。`saHandBack` 实现。
5. `agent.go` / `agent_tool_policy.go`：前台轮次的系统提示加入 Trigger 卡片、工单卡片（`saLiveTickets`）、策略段；
   路由工具对前台会话开放（含网页 Inline 轮次），执行对话不开放。去掉「不要循环轮询」以外对团队成员路由的提示。
6. 只读接口：`GET /api/sa/decisions?thread_id=`。

验收：

- gate 表每一行有用例；`off/observe/auto` 三种模式下 inferred 的去向正确，observe 记 `would_accept`。
- 同一 Thread 并发两次 `saEvaluate` 只跑一轮，期间到达的 Trigger 进入下一批。
- 路由一批 4 项、其中 1 项被拒，其余 3 项照常执行，结果按序。
- 非 explicit 的 accept/stop 被拒；attach 到未订阅本 Thread 的工单被拒。
- 用 `saSwap` 假实现 C 与 A，确定性脚本模型跑通「QQ 私聊 → 接单」和「群聊闲聊 → 直接回答」。
- `go test ./internal/web -run 'TestSAGate|TestSAPolicy|TestSARouter|TestSessionRouter'` 全过。

## 工作线 C：工单与执行

目标：接单变成带授权的长任务；后续消息能续上工单；工单的结果和问题回到原对话。

1. `sa_ticket.go`：`saAcceptTicket`（事务：`createTask` + `sa_mandate` + origin 订阅；
   Kind/Mode 取上限与发起人设置的较小者；入队 ack；同一 Origin 幂等返回已有任务）。
2. `saFanOut`：契约 §3.3 的三条认领规则；投递补充输入（复用 `enqueueTaskMessage` / `input` 动作）；
   结束的工单先复核（§3.7，调 `saPolicyFor` + `saGate`），失败写 `revoked_at`。
3. `saAttachTicket`、`saStopTicket`（取消 + `withdrawn_at`）、`saLiveTickets`（Stage 映射）。
4. `task_*.go`：状态进入 waiting_human / review / failed / budget_exhausted / done 时入队 question / result；
   `notifyTaskMain` 改走发件箱；每轮开始检查 Mandate 未被收回/吊销；执行对话加 `orchestrator_handback` 工具。
5. 只读接口：`GET /api/tasks/:id` 返回附带 `mandate`、`subscriptions`；`longtask.js` 显示来源与发起人。

验收：

- 同一 Origin 接单两次只有一个任务。
- 访客（非本机主人）发起的工单 Kind/Mode 不超过策略上限。
- 回复工单的 ack 消息 → 认领；写 `#id` → 认领；无关消息 → 不认领。
- waiting_human 时提问发到 origin Thread；在那里回答后任务恢复 running。
- 被叫停的工单不再认领任何消息；策略移除发起人后，结束的工单无法被续上。
- 结局与提问都带 dedupe，重复状态变化不重复发消息。
- `go test ./internal/web -run 'TestSATicket|TestTask'` 全过（原有长任务测试不得回归）。

## 集成（三条线合并后）

合并顺序：A → C → B（B 依赖 C 的接单与 A 的发件箱最多，最后合并便于解决冲突）。

1. 删掉 `sa_contract.go` 里已被实现覆盖的桩注释，确认 `init()` 都赋值了（加一条测试：所有变量都不是默认桩）。
2. 端到端测试：QQ 私聊「帮我做 X」→ gate explicit → 前台 accept → ack 发回 QQ → 任务执行 → 验收通过 →
   结果发回 QQ；中途 waiting_human 的问题发回 QQ，引用回复后继续。
3. 网页回归：普通聊天流式不变；在网页里接单后结果写回同一会话且不重复。
4. 面板：Superagent 页（需要人的工单、系统问题、Thread / Decision 历史），参照 dsh 005。
5. 文档：README 加入口，`15-session-router.md` 标为被 18 取代。

## 给每个 agent 的开场白（复制即可）

> 读 `docs/agent/18-superagent.md`（契约）和 `docs/agent/18-superagent-plan.md` 里「工作线 X」一节，
> 在分支 `feat/sa-xxx` 上实现它。只改契约 §6 分给你的文件；需要改契约时停下来告诉我。
> 跨线依赖用 `saSwap` 假实现测试，不访问真实模型或 QQ。完成标准是该节的验收清单和指定的 `go test` 全过；
> 最后列出每条验收项对应的测试名。
