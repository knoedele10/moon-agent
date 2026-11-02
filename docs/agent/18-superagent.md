# 18 · Superagent 主框架契约

把项目从「一个会用工具的聊天」改成参照 [dsh-superagent](https://github.com/deepseek-ai/dsh-superagent-prototype)
的主框架：**所有入站消息都是 Trigger，所有正经工作都是带授权的工单，所有回话都经过可重试的发件箱。**
网页聊天、QQ、以后的其它来源，都只是 Source。

本文是三条并行工作线的**契约**。代码以这里为准；要改，先改本文，再改
`internal/model/superagent.go`（表）与 `internal/web/sa_contract.go`（接口）。分工与步骤见
[`18-superagent-plan.md`](18-superagent-plan.md)。

## 0. 原则

- **少占人的注意力，多给价值。** 只在被请求、或规则允许时干活；只在有人要行动、或有结果可用时说话。
- **前台只分流，执行端干活。** 简单问题前台直接答；需要多步、要交付、要验收的事建工单交给执行对话。
- **授权由宿主执行，不由提示词保证。** 名单、强度、授权上限、能用哪些工具，都在代码里检查。消息、引用、文档只是数据。
- **完成由宿主判定。** 工单沿用长任务的验收（[16](16-task-runtime.md)）；模型说「完成了」不算。
- **先落库再动作。** 入站先存 Trigger，出站先存 Outbound；重启不丢、不重复。

## 1. 名词对照

| dsh-superagent | 本项目 | 存放 |
| --- | --- | --- |
| Source | 来源：`web`、`qq`（以后 `wakeup`、`proactive`） | 代码（适配器） |
| Trigger | 一条入站事件，不可变 | `sa_trigger` |
| Thread | 一个对话：网页会话 / QQ 私聊 / QQ 群 | `sa_thread` |
| Main Session（路由） | **每个 Thread 的前台会话**（见 §3.4） | `conversation` |
| Eval = gate + assessment | `saGate`（确定性）+ 前台轮次的判断 | 代码 + `sa_decision` |
| Decision | reply / accept / attach / stop / ignore / unavailable | `sa_decision` |
| Ticket | **长任务 `agent_task`**（不新建表） | `agent_task` |
| Assignment / Executor | 任务的执行对话与轮次 | `agent_task_round` |
| Mandate | 工单的授权：谁、强度、上限、策略版本 | `sa_mandate` |
| Subscription | 工单 ↔ Thread | `sa_subscription` |
| Channel | 来源的写端；所有外发先进发件箱 | `sa_outbound` |
| Policy | 每用户的接单策略（名单、模式、规则、访客上限） | `sa_policy` |

和 dsh 的有意差异：dsh 一个仓库只有一个主 Session；我们是个人助理，**每个 Thread 的前台会话就是它的路由**，
这样网页 SSE 体验不变，QQ 群和私聊也各有各的上下文。

## 2. 总流程

```
来源适配器 ──saTriggerIn──▶ saIngest（A）
   │ 去重入库 Trigger，找/建 Thread
   ├─▶ saFanOut（C）：显式引用了某工单？──是──▶ 作为补充输入投给该工单，结束
   └─ 否 ──▶ Inline（网页）？──是──▶ 调用方自己跑前台轮次（SSE）
                 └─ 否 ──▶ saEvaluate（B）：saGate 准入 ──拒──▶ filtered
                                             └─ 过 ──▶ 前台轮次（带 orchestrator_route）
前台轮次：直接回答（reply）或 orchestrator_route：accept / attach / stop / ignore
   accept ──▶ saAcceptTicket（C）：AgentTask + Mandate + origin 订阅 + 受理确认
工单结局 / 需要人回答 / 前台回答 ──▶ saEnqueueOutbound（A）──▶ 渠道发送（重试、保序）
```

## 3. 行为契约

### 3.1 入口（A）

- `ThreadKey`：`web:conv:<会话id>`、`qq:private:<QQ号>`、`qq:group:<群号>`。一个 Thread 一个前台会话
  （`sa_thread.conversation_id`）；QQ 的 `/new` 只换前台会话，Thread 不变。旧 `qq_chat_binding` 迁移进来后不再写。
- Trigger 以 `(source, source_msg_id)` 去重：重复事件返回 `Duplicate=true`，不再扇出、不再判断。
- 机器人自己发的消息在适配器里丢掉，不入库。
- 网页来源 `Inline=true`：`postChat` 存完 user 消息后调 `saIngest`（`source_msg_id` = message.id）；
  被工单认领时 SSE 回一句「已转交任务 #n」并结束，否则照常流式跑前台轮次。
- 入口不判断「谁有权」：名单、@、模式都交给 gate。适配器只如实填 `ActorKind` 和 `Addressed`。

### 3.2 准入 `saGate`（B）

输入：策略、Trigger、该 Thread 是否有活着的订阅工单。输出强度：

| 情况 | 强度 |
| --- | --- |
| 本机主人（网页登录用户本人） | explicit |
| 名单内的人，私聊或 @ | explicit |
| 名单内的人，群里没 @ | inferred（`admission` 可设为 ignore） |
| 不在名单的人，@ 或私聊 | 按 `admission`，默认 ignore |
| 不在名单的人，在有工单的 Thread 上跟进 | inferred |
| 名单内的机器人 | inferred（永远不能 explicit，不能叫停） |
| 其它机器人、系统 | none |
| 接单模式 `off` 下的一切 inferred | none |

`none` → Trigger 记 `filtered`，写一条 `by=gate` 的 Decision（ignore），**不回任何话**。
QQ 名单的默认值取自 `QQ_ALLOW_USERS` / `QQ_ALLOW_GROUPS`，第一次建 `sa_policy` 时写入，之后以表为准。

### 3.3 扇出：后续消息交给工单（C）

聊天型来源一个 Thread 很宽（整段私聊、整个群），所以**只按显式引用认领**，不按话题相似：

1. 回复了某工单发出的消息（`Trigger.ReplyToMsgID` = 某条 `sa_outbound.sent_ref`）；
2. 文本里写了 `#<任务id>`，且该任务订阅了这个 Thread；
3. 工单处于 `waiting_human`，且这个 Thread 上只有这一个在等人回答，发送者是当初的发起人。

认领后：投成任务的补充输入（`waiting_human` 时等价于 `input` 动作），Trigger 记 `fanned_out`。
认领**不扩大授权**；执行端认为超出范围时调 `saHandBack`，消息回到判断。被人叫停的工单不认领。
已结束（done/failed/budget_exhausted，非人叫停）的工单被认领时，先按 §3.7 复核授权，再续跑。

### 3.4 前台判断与路由（B）

- `saEvaluate(thread)`：用 `sa_thread.eval_state` + 租约保证一个 Thread 同时只有一次判断；
  期间新到的 queued Trigger 合并进下一批。
- 前台轮次在 Thread 的前台会话里跑，复用 `runAgentChat`；系统提示追加：本批 Trigger 卡片（发送者、强度）、
  `saLiveTickets` 的工单卡片、策略段（版本、模式、启用的规则）。
- 工具 `orchestrator_route`（**替换**现有基于团队成员的版本）：

  ```json
  {"decisions":[
    {"action":"accept","trigger_id":12,"title":"…","goal":"…","constraints":"…","criteria":["…"],"ack":"好的，我来……"},
    {"action":"attach","trigger_id":13,"task_id":5,"note":"…"},
    {"action":"stop","trigger_id":14,"task_id":5,"reason":"…"},
    {"action":"ignore","trigger_id":15,"reason":"…"}
  ]}
  ```

  1–20 项，逐项独立执行，单项被拒不影响其它项，结果按序返回 `{results:[{trigger_id,decision?,task_id?,refused?,reason}]}`。
  宿主检查：accept / stop 需要该 Trigger 为 explicit；inferred 的 accept 需要模式 `auto` 且至少命中一条启用规则
  （`observe` 下记成 ignore + `would_accept=true`）；attach 只能指向订阅了本 Thread 的工单。
- 没有调用路由工具、只是回答：记一条 `reply` Decision。非 Inline 时把回答经 `saEnqueueOutbound`
  发回（`purpose=reply`，`dedupe=reply:<本批最后一个trigger_id>`）。
- 前台**不等待**：不得 sleep、不轮询任务；工单的结果由宿主直接发到原对话。
- 非网页来源的入站文本由 B 在跑前台轮次时写进前台会话（一批合成一条 user 消息，群里带「[昵称(QQ号)]:」）；
  A 只入库 Trigger，不写会话消息。网页来源由 `postChat` 自己写。
- 推断候选不进前台轮次：没有启用规则时直接 ignore；有规则时由一次无工具的模型调用逐条评估，
  observe 记 `would_accept`，auto 直接接单（`strength=inferred`，带命中规则）。
- 判断超时或出错：重新排队（5s 起翻倍），连续三次后记 `unavailable`。失败计数在进程内，重启清零。
- `StartSARouter` 随服务启动：重新安排遗留的 queued 消息，之后每分钟巡检一次。
- 交回的消息：`sa_decision` 记一条 `by=handback`，消息回到 queued；前台提示里会附上交回原因，
  同一工单不能再接收它。
- `spawn_teammate` 等轮内委派工具保留，仍属于「前台在一轮内自己干完」的范畴。

### 3.5 工单（C）

- 工单 = `agent_task`；`saAcceptTicket` 在一个事务里建任务、`sa_mandate`（`origin_trigger_id` 唯一 → 幂等）、
  origin 订阅，然后入队受理确认（`purpose=ack`，`dedupe=task:<id>:ack`）。
- 任务的 `Kind/Mode` 取「策略允许的上限」与「发起人的设置」中较小的一个：本机主人用自己的设置，
  其他人用 `sa_policy.guest_kind/guest_mode`（默认 chat / confirm）。
- 阶段（`saTicketCard.Stage`）：

  | 任务状态 | Stage |
  | --- | --- |
  | waiting_human | `person/answer` |
  | review | `person/review` |
  | failed、budget_exhausted | `person/failed` |
  | paused | `paused` |
  | running、verifying | `working` |
  | done、canceled | `ended` |

- 需要人时（进入 waiting_human / review / failed / budget_exhausted）和完成时，向 origin Thread 入队一条消息
  （`question` 或 `result`，`dedupe=task:<id>:<状态>:<revision>`）。`notifyTaskMain` 改为经发件箱发出，
  不再直接写会话，避免网页重复。
- 执行对话新增工具：`orchestrator_handback(trigger_id, reason)`（交回超范围的跟进消息）。
  提问继续用 `task_block` / `ask_user`，宿主负责把问题发到原对话。
- 人在对话里叫停 → 前台 `stop` → `saStopTicket`：取消任务、写 `mandate.withdrawn_at`。被叫停的工单不再接收任何消息。

### 3.6 发件箱（A）

- 同一 Thread 内按入队顺序发出：前一条未发出（且失败未满 3 次）时，后一条等待。不同 Thread 互不影响。
- 失败退避：5s 起翻倍，封顶 300s；连续失败 3 次标 `failed`（继续重试，记为系统问题）。
- 渠道返回 `saPermanentError`（如 QQ 群已退出）→ `dropped`，记录原因，不再重试。
- 网页渠道 = 写一条 assistant 消息到前台会话，`sent_ref` = message.id。
- QQ 渠道 = OneBot 动作；用 `echo` 匹配回执，取回 `message_id` 作为 `sent_ref`。无在线连接时视为临时失败。
- 单条超长按渠道上限切分，切出的多条共享同一 Outbound 行，`sent_ref` 记第一条。

### 3.7 授权复核（C 调 B）

工单从结束状态被续上前：发起人的当前强度（`saGate(当前策略, 原 Trigger)`）不低于 Mandate 里的强度；
inferred 的还要求模式 `auto` 且命中的规则仍启用。复核失败 → 写 `revoked_at`，消息按「没有工单认领」走判断。
策略变更不取消正在跑的工单。

### 3.8 重启

- 入口：Trigger 已入库即算收到；`queued` 状态的 Trigger 在启动后由 B 重新安排判断。
- 发件箱：`pending/failed` 在启动后继续发；已有 `sent_ref` 的不重发。
- 工单：沿用长任务的对账与续跑。

## 4. 接口（`internal/web/sa_contract.go`）

| 提供方 | 函数 | 调用方 |
| --- | --- | --- |
| A | `saIngest(ctx, saTriggerIn) (saIngestResult, error)` | 来源适配器、`postChat` |
| A | `saEnqueueOutbound(saOutboundReq) (SAOutbound, error)` | B（前台回答）、C（受理/提问/结局） |
| A | `saChannels[source]`（实现 `saChannel`） | 发件箱 |
| B | `saPolicyFor(userID)`、`saGate(policy, trigger, hasTicket)` | A 不调；C 复核时调 |
| B | `saEvaluate(ctx, threadID)` | A（入口）、B 自己（重排） |
| B | `saHandBack(ctx, taskID, trigger, reason)` | C（执行端工具） |
| C | `saFanOut(ctx, trigger) ([]taskID, error)` | A（入口） |
| C | `saAcceptTicket` / `saAttachTicket` / `saStopTicket` | B（路由工具） |
| C | `saLiveTickets(threadID)` | B（前台提示） |
| 各自 | `saRouteRegistrars = append(…)` | `web.go` 循环注册 |

每个变量默认是「未实现」桩；实现方在自己文件的 `init()` 里赋值。单测用 `saSwap(t, &变量, 假实现)` 替换。

## 5. 表的写权限

| 表 | 写入方 | 说明 |
| --- | --- | --- |
| `sa_thread` | A 建行与 `conversation_id`；B 写 `eval_state/eval_lease` | |
| `sa_trigger` | A 建行；A/B/C 只写 `disposition`、`decision_id` | 其它列不可变 |
| `sa_decision` | B（路由、gate）；C 不写 | |
| `sa_mandate` | C | 只追加；只有 `withdrawn_at/revoked_at/end_reason` 可更新 |
| `sa_subscription` | C | |
| `sa_outbound` | A（发件箱与状态）；B/C 只通过 `saEnqueueOutbound` | |
| `sa_policy` | B | |
| `agent_task*` | C | 其它工作线只读 |

## 6. 文件所有权

| 工作线 | 独占的文件 | 允许改动的既有文件 |
| --- | --- | --- |
| A 入口与渠道 | `sa_intake*.go`、`sa_outbound*.go`、`sa_channel_*.go` | `qq_onebot*.go`、`chat.go`（只在 `postChat` 加钩子）、`app/server.go`（启动发件箱） |
| B 判断与路由 | `sa_gate*.go`、`sa_policy*.go`、`sa_router*.go` | `session_router*.go`、`agent.go`（前台提示与工具）、`agent_tool_policy.go`、`chat_kind.go` |
| C 工单与执行 | `sa_ticket*.go` | `task_*.go`、`templates/static/js/longtask.js` |
| 契约（第 0 阶段） | `sa_contract*.go`、`model/superagent.go`、本文 | `model/db.go`、`model/sqlite.go`、`web.go` |

不在表里的文件，谁要改先在计划文档里登记，避免两条线同时改。

## 7. 测试约定

- 用 `setupSATestDB(t)`（临时 SQLite，已迁移会话、消息、任务和全部 sa 表）。
- 跨线依赖一律 `saSwap` 成假实现；**不访问真实模型、真实 QQ**。
- 每条线至少一条「从入口到出口」的单测；集成阶段再补一条 QQ 私聊 → 接单 → 验收完成 → 结果发回 QQ 的端到端测试。

## 8. 本期不做

- 唤醒、主动聊天改成 Source（保持现状，只在集成阶段评估）。
- 跨来源身份映射（QQ 号 ↔ 网页账号），按 dsh 的做法写在提示词里。
- 面板大改：本期只加接口；界面在集成阶段统一做。
- 提示词版本管理（dsh 的 004），沿用现有灵魂/规则。
