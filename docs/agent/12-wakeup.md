# 定时唤醒（agent wakeup）

> 最后更新：2026-10-25　相关代码：`internal/model/wakeup.go`、`internal/web/wakeup.go`、`internal/web/wakeup_tool.go`

## 1. 它是什么

用户或 agent 指定「某个时间点，在这个会话里做某件事」。到点后后端让 agent 带着完整会话历史自己跑一轮，
结果作为普通 assistant 消息落库；用户打开会话就能看到，不需要一直挂着窗口。

```
POST /api/wakeups { conversation_id, run_at, prompt, title }
        ↓
agent_wakeup 表（pending）
        ↓ 每 30 秒扫描
领取（租约）→ 后台轮次（headless）→ done / failed
```

## 2. 它不是什么

| | 定时唤醒 | 主动聊天（proactive） | 子 agent 唤醒 |
|---|---|---|---|
| 触发者 | 用户 / agent 明确指定时间 | 全局策略判断「该不该说话」 | 子任务全部结束 |
| 时间 | 精确到点 | 间隔轮询 + 拦截条件 | 事件驱动 |
| 范围 | 每会话多条，可取消 | 每用户一条状态 | 每批一次 |
| 落点 | 目标会话的历史 | 「主动消息」专用会话 | 父会话历史 |

不值得复用 `agent_proactive_state`：它是全局状态，唤醒是「每会话多条、带时间和正文」的队列。

## 3. 数据与状态

`agent_wakeup`：

| 列 | 说明 |
|---|---|
| `user_id` / `conversation_id` | 归属；唤醒只写回这条会话 |
| `title` / `prompt` | 列表里看的标题 / 到点交给 agent 的说明 |
| `run_at` | 计划时间（本地时区存 UTC，展示转本地） |
| `status` | `pending` / `done` / `failed` / `canceled` |
| `attempts` / `error` | 失败次数与最后一次原因 |
| `next_attempt_at` | 一列两用：失败退避 + 调度器领取租约 |
| `fired_at` | 真正执行的时间（可能晚于 `run_at`） |
| `created_by` | `user` / `assistant` |

状态只增不改：pending 可以被取消或执行；done / failed / canceled 是终态，不能再取消。

## 4. 调度语义

- **间隔**：`AGENT_WAKEUP_INTERVAL_SECONDS`，默认 30 秒；`<=0` 只关后台调度，建单接口仍然可用。
- **领取**：`UPDATE ... WHERE status='pending' AND run_at<=now AND next_attempt_at<=now` 原子抢，
  抢到就把 `next_attempt_at` 推成 `now+15m`。进程中途崩了，租约到期会被重新捡起。
- **会话忙**：目标会话正在跑（用户刚发消息 / 子 agent 在跑）时不执行，只推后 2 分钟，**不计失败次数**。
- **限流**：沿用 `wakeAllowed`（同会话 30 秒一次、一小时 6 次）。撞上就推后 5 分钟。
  这道闸防的是「唤醒 → agent 又定一次几分钟后的唤醒」形成快环。
- **失败**：1 分钟起、翻倍、上限 1 小时；默认 3 次后落 `failed`，原因写进台账。
- **串行**：同一进程内一轮只跑一批（TryLock），长轮次会跳过中间那一拍，不会叠跑。

## 5. 到点之后

复用 `runAgentChat` 的 `headless=true` 路径（与子 agent 唤醒父级同一条）：

- 带完整历史、工具集、记忆与压缩策略；
- 不写自动记忆、不刷画像（它不是用户发起的一轮对话）；
- 回答作为普通 assistant 消息落库；
- 提示词会写明「这是你自己定的定时唤醒」+ 当时的时间，并要求「不需要做了就直说，不要编造进展」。

## 6. 安全阀

- 每个会话最多 5 条 pending（工具与接口都校验）；
- 建单要求 `run_at` 至少领先 15 秒、最多一年；
- `prompt` 上限 4000 字，标题 120 字；
- `schedule_wakeup` 在 `confirm` 档要先问用户（`toolsys` 元数据：`RiskSelf` + `Confirm`）；
- 取消 / 列表只读或幂等，不需要确认；
- 跨用户一律按「不存在」处理（403/404 不区分）。

## 7. 接口与工具

```
GET    /api/wakeups?conversation_id=&status=&limit=
POST   /api/wakeups        { conversation_id, run_at, prompt, title? }
DELETE /api/wakeups/:id
```

Agent 工具：

| 工具 | 作用 | 确认 |
|---|---|---|
| `schedule_wakeup` | 定一次唤醒（`run_at` / `prompt` / `title`） | confirm 档要问 |
| `list_wakeups` | 看自己定过什么（默认 pending） | 不问 |
| `cancel_wakeup` | 取消一条 pending | 不问 |

`run_at` 用 RFC3339 带时区（`2026-10-25T09:00:00+08:00`）；模型不确定现在几点时应先调 `datetime`，
提示词里也明确要求不要凭记忆推算。

## 8. 验证

- `internal/web/wakeup_test.go`：时间解析、文案/标题截断、提示词、退避；执行器的成功 / 会话忙 / 限流 /
  失败重试 / 次数用尽 / 被别人领走 / 上一轮未结束；工具元数据与「不在会话里」的拒绝；坏 JSON → 400。
- `internal/web/wakeup_db_test.go`（`AGENT_WAKEUP_IT=1`）：领取与租约、完成、失败退避与次数用尽、
  取消语义、跨用户隔离、**真库 + 真执行器**（会话没模型 → 记一次失败，不调模型）。
