# 参考实现拆解：subagent 与 agent team（deepseek-harness）

来源：`C:\Users\woshiss\Downloads\deepseek-harness-master.zip`（解压在 `build/deepseek-harness/`，`build/` 已在 .gitignore 里）。
三份带行号的详细报告在 `build/` 下：`research-subagent-core.md`（运行时）、`research-subagent-tools.md`（工具面）、
`research-agent-team.md`（团队层）。本文只留结论。

## 一、子 agent 是怎么做的

分三层，互相不越界：

| 层 | 是什么 | 位置 |
| --- | --- | --- |
| 服务 | `ctx.subagents`（`SubagentManager`）：准入、容量、父子所有权、结算、释放 | `packages/subagent/subagent/src/manager.ts` |
| 提供方 | spawn / fork / ACP / Codex / Claude Code / DSH SDK，按名字注册 | `packages/subagent/subagent-*/` |
| 模型工具 | `subagent`、`send_message`、`interrupt_agent`、`list_agents` | `packages/subagent/tool-subagent*/` |

七条关键机制：

1. **工具立即返回 id，绝不等结果**。`subagent(...)` 返回 `{kind:'activation', subagentId}`，只保证「子级收下了初始消息」；
   子级跑完后，运行时往父会话投一条 **user 角色的完成通知**（不是 tool result）。
   通知 = 状态摘要（120 字符上限）+ 子级最后一条非空 assistant 文本（不截断）+ 可选结构化 JSON + 诊断（4KiB 上限）。
   父级空闲时走 queue（起新一轮），忙时 steer 到最近 step 边界。
2. **深度与并发是两套独立护栏**。`maxDepth` 默认 1（写进子会话 header，单调不回退，冷恢复也不会把自己当顶层重新计数）；
   `maxActiveSubagents` 默认每棵 activation 树 8 个，进程内池，**预占后不排队、耗尽直接 `ACTIVATION_LIMIT_REACHED`**。
   不排队的理由很实在：排队会让「父级等子级」和「子级占着父级的名额」互相咬死。
3. **权限是委派瞬间的同步快照，且子级审批钉死为 `never`**。子级拿父级显式设过的沙箱覆盖，
   但任何需要审批的操作（例如申请扩权）会被确定性拒绝，连「愿意放行的 answerer」都不会被咨询。
   模型侧还有固定声明告诉它「权限已固定、别重试」。所以**子级只能比父级更窄，不能更宽**。
4. **可续跑子级**：本地 spawn/fork 保留持久会话。`send_message` 对运行中的子级是 steer（最近 step 边界插入），
   对空闲/未加载的子级是启动或冷恢复（`agents.resume`，不再经提供方）。外部提供方（ACP/Codex/…）是一次性的。
5. **子级目录写在父会话日志里**（`subagent/catalog`）。重启后仍能发现子级，而**读目录不加载也不恢复任何子 agent**。
   它只记「成员关系」，不记执行状态，所以列表不会因为历史而变慢。
6. **进程外的能力被压平**：外部提供方不支持 `agentOptions/outputSchema/maxDepth/toolFilter/persona` 时，
   启动前直接以 `UNSUPPORTED_CAPABILITY` 拒绝，不做「尽力而为」的降级。
7. **结果语义严格**：发布前失败 = 工具报错、不留子级；发布后失败 = 一条带 `stopReason` 的通知
   （`completed/aborted/max-tokens/refusal/error`），未知原因一律归 `error`（宁可报失败不报成功）。

## 二、agent team 是怎么做的

它是**建在子 agent 之上的领域层**，比子 agent 只多三样：具名 roster、成员间直接消息、共享任务 DAG。

1. **隐式团队**：`TeamId` 就是 root `SessionId`，没有「创建团队」事件，root agent 天然是 Lead，
   持久状态从第一条 member/task 记录开始。省掉了一整套团队生命周期状态机。
2. **成员是具名的可续跑子级**：`spawn_teammate { name, description, prompt, context: fresh|fork }`。
   名字小写、不可变、**创建失败也永久占名**，且绝不与 Session UUID 一起暴露给模型（模型只认名字）。
   只有 Lead 能创建和中断。
3. **消息只走目标 inbox，不建第二个 outbox**：`send_message { target, message }` 返回「已接收」，
   **不表示对方已处理**；running 在最近 step 边界收，inactive 启动/冷恢复收。
   代价：不重试、不去重、不保证跨发送顺序（顺序由目标 inbox 管）。用一条简化换掉了重试/确认/排序三套状态。
4. **共享任务板**：每个任务是一份完整快照 + 单调 `revision`，每次变更带 `expected_revision` 做 CAS，
   陈旧写入被拒而不是覆盖；`blockedBy` 是有向无环图，依赖未完成不能 claim；删除留 tombstone（保 id 稳定）。
   `writeScopes` 只是「两个 in-progress 任务路径重叠」的**警告**，绝不加锁。
5. **权限靠「确切实时调用方」**：每个服务方法第一个参数都是调用方 `Agent` 实例本身，在执行时判定，
   而不是靠角色标签或会话 id。
6. **持久化 = 写 Lead 的会话日志**：`team/member`、`team/task` 追加后 flush 才报成功；
   这些事件**不进会话表面**，所以协作记录不污染模型上下文；前端读的是投影（roster + 未删除任务）。
7. **九个工具**（注册在每个成员自己的作用域，Lead 与 teammate schema 相同、权限执行时判）：
   `spawn_teammate` / `send_message` / `list_agents` / `wait_agent` / `interrupt_agent` /
   `team_task_create` / `team_task_list` / `team_task_get` / `team_task_update`。
   `wait_agent` 是「注册之后的下一条变化」的一次性等待，且在没有活跃同伴时立即返回 `noProgress`，专门避免模型轮询。
8. **它自己承认的短板**（README 的「已知限制」）：单进程、成员共享同一个 checkout（没有 worktree / 文件锁）、
   roster 扁平不可嵌套、**任务 owner 不会自动释放**、write scope 只提示、每次变化把完整视图广播给所有浏览器。

## 三、和我们（blog-my）现在的差距

| 能力 | 它的做法 | 我们现在 |
| --- | --- | --- |
| 委派 | 服务 + 提供方 + `subagent` 工具 | 没有，只有一个 agent 循环（`agent/loop.go`） |
| 并发/深度护栏 | `maxDepth` + 容量池 | 没有 |
| 结果回传 | 完成通知（user 消息） | 不适用 |
| 任务编排 | CAS + DAG 的任务板 | 计划模式写 markdown（`web/agent_plan.go`），非结构化 |
| 权限 | 快照 + 子级审批钉死 | plan/confirm/auto 三档 + 工作区沙箱（`agent/fs`），思路同源但更粗 |
| 记忆 | 协作记录不进模型历史 | 分域记忆 + 召回公式（这块我们更细，不用学它） |

## 四、值得抄的（按性价比排序）

1. **委派工具契约**：立即返回 id + 异步完成通知。同步等结果会让父 turn 卡死，是最容易踩的坑。
2. **两个护栏**：`maxDepth`（默认 1）+ 每树容量池（默认 8，不排队、直接拒绝）。代码量很小，能防跑飞。
3. **权限快照 + 子级审批钉死**：委派边界就是权限边界，子级不能自己扩权。和我们的档位/沙箱能直接接上。
4. **子级目录写父日志**：重启后仍能发现子级；读目录不加载子 agent。我们已有 `agent_history` 的轨迹表，可复用。
5. **任务板 CAS + DAG**：比现在的 markdown 计划强得多，而且 revision 冲突检测实现很便宜。
6. **具名成员 + 不可变名字**作为模型侧唯一地址（不暴露内部 id）。
7. **控制面与委派面分开**：`send/interrupt/list` 与 `start` 分成两组工具，各自薄。
8. **`wait_agent` 的一次性等待**：只报「有没有变化」，调用方自己重读状态，天然免疫轮询。

## 五、明确不建议抄的

- **进程外提供方全家桶**（ACP / Codex / Claude Code）：单人本机助手用不上，成本极高。
- **全量广播投影**：它自己都标为限制，我们没必要复制。
- **共享 checkout 且无文件锁**：我们的文件工具已经有工作区沙箱，不该退回到「靠人协调」。
- **不做发送重试/去重**：它是为了简化而主动放弃的，我们的消息量小，可以做得更稳。
