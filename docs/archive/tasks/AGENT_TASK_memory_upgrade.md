# TASK: memory_upgrade — 记忆系统四项升级（借鉴 AAAAGENT）

项目：E:\blog-my（Go + Gin + GORM + MySQL + Ollama 嵌入）。

## 前提

共享仓库：另外两个 worker 在改 `web/chat.go` / `agent/loop.go` / 主动聊天的**新文件**。
**你只能改下面列的文件**，不要动别人的文件，不要回退已有改动。

## 你负责的文件

- `model/kb.go`
- `rag/*.go`（整个 rag 包；可以新建文件）
- `web/agent_memory.go`
- `web/kb.go`（**只允许**在列表/统计里加 scope 的过滤与展示，别动别的）
- 允许新建 `rag/dynamics.go`、`rag/dynamics_test.go`

**不要改**：`agent/**`、`web/agent.go`、`web/chat.go`、`web/web.go`、`templates/**`、`model/code.go`。

## 先读

- `rag/memory.go` —— `MemoryPrompt` / `RunMemorySearch`（检索入口）
- `rag/hybrid.go` —— 混合检索（语义+关键词+实体）与打分
- `rag/store_lexical.go` —— 无嵌入时的关键词兜底
- `rag/recency.go` —— **当前的时间加权（要替换掉的那段）**
- `rag/supersede.go` —— 记忆替代（已有，保留）
- `rag/extract.go` —— 事实抽取（要加 scope 与 importance/emotion）
- `rag/ingest.go` —— `IngestFact` / `IngestText`
- `model/kb.go` —— `KBDocument` 表结构
- `web/agent_memory.go` —— `saveAutoMemory` / `memoryQuery` 等

## 要实现的四件事

### 1. 陪伴记忆与工作记忆分开（scope）

AAAAGENT 的核心设计："**不把所有工程记录塞进陪伴记忆**"。你的记忆现在全在一个池子里。

- `model/kb.go`：`KBDocument` 增加 `Scope string`（列 `scope`，size 16，加索引）。
  取值：`"life"`（陪伴/生活）、`"work"`（工作/项目）、`""`（未分类，兼容旧数据）。
- `rag/extract.go`：抽取提示词里让模型给**每条事实**打一个 scope。写清楚判据：
  - `life` —— 关于这个人本身：称呼、喜好、情绪、生活、健康、关系、习惯
  - `work` —— 关于在做的项目/技术/代码/业务：项目名、技术栈、端口、文件、任务进度
  - 拿不准就给空串（**不要硬猜**）
- `rag/ingest.go`：`IngestFact` 接受并写入 scope。
- `rag/memory.go` / `hybrid.go` / `store_lexical.go`：检索时按 scope **加权而不是硬过滤**——
  同 scope 的记忆乘一个加成（建议 1.15），不同 scope 不惩罚（旧数据是空串，惩罚它会误伤）。
  这样"陪伴时优先生活记忆，但不至于完全查不到工作事实"。
- 检索的 scope 由调用方传入（当前会话的类型），**未指定时按空串处理 = 不加权**。

### 2. 召回公式加维度（替换现在的 recencyFactor）

现在只有"7 天内满分、之后最多衰减 15%"。换成 AAAAGENT 的公式：

```
活跃度半衰期 H = 30 × (1 + 2×重要性) 天        // 重要性越高衰减越慢
活动度 A(t) = A₀ × 2^(-t/H)                   // 稳定档案类恒为 1
情绪强度 E(t) = E₀ × 2^(-t/7)
优先级 P = 相关性 × (0.55 + 0.25A + 0.15I + 0.05E)
强化 A' = A + 0.2 × (1 - A)
```

- 新增文件 `rag/dynamics.go`，把这套公式实现成**纯函数**（可单测）：
  `decay`、`evolve`、`reinforce`、`priority`，参数放在一个 `DynamicsParams` 结构里，
  默认值与上面一致，权重之和必须等于 1（加断言）。
- `model/kb.go`：`KBDocument` 增加
  `Importance float64`（列 importance，0~1，默认 0.5）、
  `Activity float64`（列 activity，默认 1.0）、
  `Emotion float64`（列 emotion，0~1，默认 0）、
  `ReinforcedAt *time.Time`（列 reinforced_at，可空）。
- 抽取时让模型给每条事实一个 `importance`（0/0.5/1 三档即可）与 `emotion`（0~1，没有就 0）。
- `priority` 替换掉 `recencyFactor` 在检索打分里的位置：**相关性 × 权重和**，而不是"相关性 × 时间衰减"。
- **自然淡化不删除**（这条你原来就做对了，继续保持）：活跃度下降只影响排序，`stale` 仍然只在
  supersede 时置位，绝不因为时间流逝删行。
- 强化：用户**明确重提**同一件事时（走已有的 supersede/去重命中路径），把 Activity 提升
  `A + 0.2×(1-A)`，并更新 `ReinforcedAt`。**仅浏览/检索不强化**。

### 3. 记忆按 token 预算贪心选取，而不是 topK

现在 `MemoryPrompt` 是取 topK 条然后拼文本。改成：

- 先按 `priority` 降序排好候选（多取一些，比如 limit×4，最少 60），
- 然后**按 token 预算贪心填充**：逐条累加估算 token，超过预算就停，剩下的记为省略。
- token 估算用**近似**即可（中文按 1 字 ≈ 1 token、英文按 4 字符 ≈ 1 token，写成一个纯函数并单测），
  不要引入 tokenizer 依赖。
- 预算从环境变量读：`AGENT_MEMORY_TOKEN_BUDGET`（默认 **1200**），非法/<=0 用默认。
- `MemoryPrompt` 的签名可以扩展（加一个 budget 参数或 options 结构），但要**保持旧的调用行为兼容**
  （调用方 `web/agent.go` 由主 agent 改，你只需在报告里写清最终签名）。
- 注入文本里保留"共命中 N 条、因预算省略 M 条"的说明，便于排查。

### 4. 顺带：scope 在知识库列表里可见

`web/kb.go` 的 `kbListDocuments` 响应里加 `scope` 字段（与上一轮加的 `source` 并列），
`kbStats` 的 `by_source` 旁边加一个 `by_scope`（life / work / unclassified）。
**只加这两个，别动别的。**

## 硬性要求

- 向后兼容：旧数据 scope 为空、importance=0.5、activity=1、emotion=0，行为与改造前**尽量接近**。
- 所有新逻辑抽成**纯函数并写单测**（`rag/dynamics_test.go`）：衰减、半衰期随重要性变化、
  权重和断言、token 估算、贪心选取的边界（预算刚好够/不够/空候选）。
- 迁移：新列由 GORM AutoMigrate 自动加（`model/code.go` 已有 `&KBDocument{}`），**不要改 model/code.go**。
- 验收（PowerShell）：
  ```
  $env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'
  go build ./...
  go test ./... -count=1
  ```
  必须全过。

## 完成后报告

- 改了/新建了哪些文件
- `KBDocument` 最终新增了哪些列
- `MemoryPrompt` / `RunMemorySearch` 的**最终函数签名**（主 agent 要改调用方）
- 公式的默认参数与最终取值
- scope 的判据与加权方式
- 测试结果 + 你没做/不确定的地方
