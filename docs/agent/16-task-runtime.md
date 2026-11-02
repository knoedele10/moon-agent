# 长任务运行时：持久目标、自动续跑与宿主验收

用户提交一个带明确验收条件的复杂任务后，系统自行执行、保存进度、修正失败并持续推进。
任务会一直推进到验收通过，或明确停在以下状态之一：等待人工、待审、失败、已取消、预算耗尽。
**模型说“完成了”不等于完成**，是否完成只由宿主验收决定。

代码位置：`internal/model/task.go`（表）、`internal/web/task_store.go`（存储/状态/outbox）、
`task_runner.go`（调度/续跑/失败处理）、`task_verify.go`（验收）、`task_tools.go`（工具与人工操作）、
`task_api.go`（接口）、`task_context.go`（模型窗口）、`templates/static/js/longtask.js`（界面）。

## 1. 任务状态

`agent_task` 每行保存这些内容：

- **任务定义**：原始目标、约束、验收条件（JSON）、进度、已完成步骤、当前阻碍。
- **执行预算**：轮数、工具调用次数、token、截止时间。
- **关联对话**：主对话 `main_conversation_id`，以及执行对话 `exec_conversation_id`（唯一）。
- **运行时字段**：租约、`next_run_at`、`needs_reconcile`、`revision`。

另有三张辅助表：

- `agent_task_round`：每一轮的状态和工具流水 `journal`。
- `agent_task_outbox`：待投递给执行对话的消息。
- `agent_task_event`：审计事件，失败原因可以在这里查到。

状态：`running → verifying → done | review | running(修正)`；`waiting_human`、`paused`；
终态 `done / failed / canceled / budget_exhausted`。服务重启后，任务及剩余工作都能从这些表中查到。

## 2. 自动续跑调度

`StartTaskScheduler` 每 3 秒扫描一次（新任务或按钮操作会立即触发扫描），并选出同时满足以下条件的任务：

- 状态为 `running` 或 `verifying`；
- `next_run_at` 已到；
- 没有有效租约。

拿租约是一条带条件的 `UPDATE`，多个调度方最多只有一个能拿到。执行时还会原子登记执行对话的忙碌状态。
因此**同一执行对话不会同时跑两轮**；如果用户正在手动使用执行对话，任务轮次会让路，稍后再跑。

每轮结束时由宿主判断下一步：

| 本轮结果 | 下一步 |
|---|---|
| 调用了 `task_submit` | 进入 `verifying` |
| 调用了 `task_block`，或 `ask_user`、权限拦截 | 进入 `waiting_human`，并通知主对话 |
| 自然收尾，例如只回复了“下一步……” | 投递“自动续跑”消息，下一轮继续，不需要用户说“继续” |
| 单轮触顶 | 同上，自动续跑 |

暂停或取消会立即打断正在进行的轮次。工具执行前会检查取消状态，所以取消后不会再启动新步骤。

## 3. 长上下文

- **按模型窗口触发**：`modelContextTokens` 的取值来源依次为 `AGENT_MODEL_CONTEXT_TOKENS`（`前缀=token`）、内置表、`AGENT_CONTEXT_TOKENS`，都没有时默认 32000。
- **入口压缩**：估算 token 超过窗口的 50% 时触发（原来只看条数）。按 token 触发时，不会继续沿用过期的旧摘要。
- **单轮工具执行中的压缩**：`agent.Config.ContextTokens` 让循环内压缩在窗口的 60% 触发。压缩只摘要中间部分，系统提示始终保留。
- **每轮注入任务卡**：每轮都把任务卡注入系统提示，内容包括原始目标、约束、验收条件、预算、进度、已完成步骤（标明不要重复执行）、上次验收的失败项和重启说明。因此无论压缩多少次，关键要求都在。

## 4. 完成验收

执行器用 `task_submit` 逐条提交证据，缺少证据的条件会被直接拒收。之后宿主逐项核对：

- `command`：通过 `run_command` 使用同一套白名单和沙箱运行检查命令，退出码为 0 才算通过。由模型代填的命令验收只在自主档允许；用户在界面中填写的不受此限制。
- `auto`：使用一次独立的、不带工具的模型调用作为裁判。裁判只看证据和宿主记录的工具流水，拿不准时判为 `review`。
- `manual`：一律进入待审。

核对结果的处理：

- 有失败项：失败项和原因交回**原执行对话**修正。失败次数超过 `AGENT_TASK_MAX_VERIFY_ATTEMPTS`（默认 3）后，任务判为 `failed`。
- 没有失败项但有待审项：进入 `review`，由用户在界面中选择通过或驳回。
- 全部通过：任务完成，交付报告写回主对话。

## 5. 恢复与失败处理

- **消息不丢、不重复**：所有发给执行对话的消息先写入 outbox。“写消息”和“标记已投递”在同一事务中完成，所以重启后既不会丢消息，也不会重复投递。
- **启动对账**：运行中的轮次改记为 `interrupted`，释放租约，并设置 `needs_reconcile`。下一轮的系统提示会列出被打断那一轮的工具流水（已成功 / 结果未知），要求**先核对现场再继续**。
- **避免重复外部操作**：在核对轮中，如果某个风险工具调用与重启前已成功的调用完全相同，会被拦截。
- **临时故障**（网络、超时、429、5xx）：指数退避重试（2s 起，最长 5 分钟）。连续失败达到 `AGENT_TASK_MAX_FAILURES`（默认 4）后停止；非临时错误重试一次即停止。失败原因写入 `last_error` 和事件流水。
- **预算耗尽**：标记为 `budget_exhausted`，并明确写明“未完成”。用户可以追加预算后重试。

## 6. 界面与接口

聊天页顶栏的“长任务”面板可以完成以下操作：

- 新建任务。验收条件每行写一条，`[命令]`、`[人工]` 前缀分别表示命令验收和人工审核。
- 查看任务的目标、约束、状态及**停下的原因**、进度、预算、执行轮次、逐项验收结果和事件。
- 打开执行对话。
- 暂停、继续、取消、重试（预算耗尽时可追加轮数）、补充信息、审核通过或驳回。

接口：`GET/POST /api/tasks`、`GET /api/tasks/:id`、`POST /api/tasks/:id/{pause|resume|cancel|retry|input|review}`。
所有查询都按当前用户过滤，跨用户访问返回 404。
主对话中的模型使用 `task_manage`（create/list/get/pause/resume/cancel）创建和管理任务。

## 7. 验证

`go test ./internal/web -run TestTask`：使用临时 SQLite、确定性脚本模型和真实调度器，不访问真实模型供应商。

| 场景 | 测试 |
|---|---|
| 正常完成（无须催促） | `TestTaskNormalCompletion` |
| 提前收尾后自动续跑 | `TestTaskEarlyStopAutoContinues` |
| 验证失败 → 自动修复 → 重新验证 | `TestTaskVerifyFailureRepairs`、`TestTaskModelClaimIsNotCompletion` |
| 多次压缩后仍遵守原始约束 | `TestTaskCompactionKeepsConstraints`、`TestTaskContextWindowLookup` |
| 中途重启：先核对现场再继续，不重复投递或操作 | `TestTaskRestartReconciles`、`TestTaskReconcileGuardBlocksDuplicateSideEffect` |
| 人工接管 | `TestTaskHumanTakeover` |
| 取消与限额 | `TestTaskCancelStopsNewSteps`、`TestTaskBudgetExhausted` |
| 退避重试与重复失败停止 | `TestTaskTransientRetryThenFail`、`TestTaskRepeatedFailureStops` |
| 并发与隔离 | `TestTaskConcurrencyAndIsolation` |

## 已知边界

- “不重复外部操作”的保证范围有限：已投递的消息不会重复投递；重启后的核对轮会拦截与重启前已成功调用完全相同的风险调用。对于“执行中被打断、结果未知”的调用，宿主只能要求模型先核对现场，无法替外部系统做幂等。
- token 预算优先采用供应商返回的用量；供应商没有返回时，按输出文本粗估。
- 裁判是同一个模型的独立调用，并不能替代人工审核。需要主观判断的条件应标为 `[人工]`。
