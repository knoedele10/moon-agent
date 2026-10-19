# TASK: cross_turn_history — 跨轮工具历史连续（借鉴 Cyrene 的 CTA 设计）

项目：E:\blog-my（Go + Gin + GORM + MySQL）。

## 前提

共享仓库：另外两个 worker 在改 `rag/**`（记忆）和新建主动聊天文件。
**你只能改下面列的文件**，不要动别人的文件。

## 你负责的文件

- `web/chat.go`（**只改历史重建那一段**，别动 postChat 的其余逻辑）
- 新建 `web/agent_history.go`
- 新建 `web/agent_history_test.go`

**不要改**：`agent/**`、`web/agent.go`、`web/web.go`、`rag/**`、`model/**`、`templates/**`。

## 问题（这是你项目里唯一的**结构性缺口**）

现在 `chat.go` 重建喂给模型的历史时，明确**跳过 `role="tool_trace"` 的消息**：

```go
// tool_trace 是工具调用轨迹，只给前端回放卡片用。
// 绝不能喂给模型：模型会把自己上一次的工具输出当成对话正文读进去。
if m.Role == roleToolTrace {
    continue
}
```

当时的顾虑是对的（Detail 可能很长、直接塞回去会污染上下文），但**代价被忽略了**：

> **模型跨轮看不到自己上一轮调过什么工具。**

结果就是："我刚才读过那个文件"这类连续推理做不到，只能重新读一遍；用户说"接着改刚才那个"
模型也不知道"刚才那个"是哪个。Cyrene 专门为此写了一篇 CTA（会话轨迹架构）设计稿，
它的目标第一条就是"**跨轮工具历史连续**：打断后纠正、正常跨轮对话不再失忆"。

## 要做的（不建 CTA，做最小可用版）

Cyrene 的完整方案是"会话级 append-only transcript 作为唯一权威源"，那对你是过度设计。
你要做的是**同一目标的轻量版**：

### 1. 把 tool_trace 压成一行摘要注入历史，而不是丢掉

新建 `web/agent_history.go`，提供一个纯函数：

```go
// toolTraceNote 把一条 tool_trace 的 JSON 压成给模型看的简短说明。
// 返回空串表示这条轨迹不值得注入（解析失败 / 空数组）。
func toolTraceNote(raw string) string
```

- 轨迹的 JSON 形状见 `web/agent.go` 里的 `agentTraceEntry`：
  `[{Name, OK, Summary, Detail}]`（**Detail 可能很长，一律丢弃，只用 Summary**）。
- 输出形如：
  ```
  [上一轮你执行过的工具]
  - read（成功）：读取了 a.go（1–40 行）
  - edit（成功）：把 foo 替换成 bar
  ```
- 每条摘要按 rune 截到 **120 字**；整块最多 **6 条**，超出写"（另有 N 次调用未列出）"。
- 解析失败 → 返回空串（**不要让一条脏数据打断历史重建**）。

### 2. 在历史里用它替换掉 `continue`

`chat.go` 里那个循环改成：

```go
if m.Role == roleToolTrace {
    if note := toolTraceNote(m.Content); note != "" {
        llmMsgs = append(llmMsgs, llm.Message{Role: llm.RoleSystem, Content: note})
    }
    continue
}
```

**为什么用 `role=system` 而不是 `role=user`**：这是运行时给模型的旁注，不是用户说的话；
冒充 user 会让模型以为用户提过这件事（Cyrene 的文档里专门警告过这点：
"Runtime Feedback：不伪装成 role:user"）。请在注释里写明这个理由。

### 3. 防止它无限增长（这一条同样重要）

轨迹是**每轮一条**，聊 50 轮就是 50 条注记，本身就会把上下文吃掉。所以：

- 只注入**最近 N 轮**的轨迹注记（`AGENT_TRACE_NOTE_TURNS`，默认 **6**，<=0 表示不注入）。
  在 `chat.go` 重建历史时从后往前数，超过 N 的直接跳过。
- 单条轨迹里如果 `Name` 是纯只读工具（`read`/`list`/`web_search`/`web_reader`/`memory_search`/
  `skill_search`/`skill_read`），摘要可以更短（截 80 字）——只读操作对后续推理的价值低于写操作。
  把"哪些算只读"做成一个函数并单测。

### 4. 单测（`web/agent_history_test.go`，不依赖数据库）

必须覆盖：
- 正常轨迹 → 生成带工具名与摘要的注记
- `Detail` 很长 → 被丢弃，不影响输出
- 摘要超长 → 按 rune 截断（**中文不能被截出乱码**）
- 非法 JSON / 空数组 / `null` → 返回空串，不 panic
- 只读工具与非只读工具的摘要长度差异
- 条数超过 6 → 出现"另有 N 次"的字样

## 硬性要求

- **向后兼容**：没有 tool_trace 的会话行为完全不变；`postChat` 的其余逻辑一个字不动。
- `llm.RoleSystem` 常量已存在（见 `llm/provider.go`）。
- 验收（PowerShell）：
  ```
  $env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'
  go build ./... ; go test ./... -count=1
  ```
  必须全过（尤其 `./web` 里已有的 SSE / 历史重建断言）。

## 完成后报告

- 改了/新建了哪些文件
- `toolTraceNote` 的最终签名与输出格式（给一个真实例子）
- 环境变量名与默认值
- 只读工具名单
- 测试结果 + 你没做/不确定的地方
