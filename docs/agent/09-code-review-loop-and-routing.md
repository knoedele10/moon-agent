# 代码实读笔记：运行时循环与会话路由

本文是对现有代码的**实读记录**，不是设计提案。
目的是把「代码实际怎么跑」和「文档怎么写」之间的偏差挑出来，供后续维护对照。

**本文覆盖**：`agent/loop.go`、`agent/eino.go`、`agent/tool.go`、`agent/guard.go`、`agent/safety.go`、`agent/subagent.go`、`agent/team.go`、`web/chat.go`、`rag/hybrid.go`、`rag/dynamics.go`。

**本文不覆盖**（尚未实读，见文末待办）：`web/agent.go`、`agent/eino_model.go`、`agent/stream.go`、`rag` 写入线、`agent/toolkit/`、`agent/fs/`、`agent/skill/`。

---

## 0. 一句话结论

这个仓库有**两套完整的 agent 执行循环**（legacy `loop.go` 与 Eino `eino.go`），两套都实现了同一组执行规则，但**规则是各自写了一份，不是共享的**。改动其中一份不会影响另一份。

---

## 1. 两条循环并存

| | legacy（`agent/loop.go`） | Eino（`agent/eino.go`） |
|---|---|---|
| 谁决定下一步 | 本项目自己写的 for 循环 | Eino 的 `react.Agent` 图 |
| 工具调用上限 | `cfg.MaxToolCalls` | 同一字段，但由 `state.call` 把关 |
| 迭代上限 | `cfg.MaxIterations` | `MaxStep = maxIter*2 + 2`，另配 `retryEinoModel` |
| 执行规则 | 直接写在循环里 | 写在 `einoToolAdapter` → `state.call` 里 |
| 事件流 | `emit(Event)` | 同一个 `emit`，经 `einoRunState.send` |

`08-eino-runtime.md` 记录了切换到 Eino 的事实，但 `01-design.md` 描述的仍是 legacy 循环的结构。
两份文档并列存在、都没错，只是**读者无法从任一份单独得知当前默认走哪条**——这是本次实读最先暴露的问题。

### 1.1 `einoToolAdapter` 是一层薄壳

```go
func (t *einoToolAdapter) InvokableRun(ctx context.Context, args string, ...tool.Option) (string, error) {
	return t.state.call(ctx, t.inner.Name(), args)
}
```

适配器本身不做事，全部逻辑在 `einoRunState.call`。**`call` 里的规则与 `loop.go` 里的规则是平行的两份实现**，包括：

1. 执行次数上限（`s.executed >= s.maxCalls`）
2. 非幂等操作在 `s.halted` 之后拒跑（错误码 `NOT_EXECUTED_AFTER_UNCERTAIN`）
3. 相同指纹的未知结果操作拒绝重复执行（错误码 `UNCERTAIN_EFFECT_BLOCKED`）
4. 错误分类 → `decideRetry` → 重试一次（仅一次，不循环）
5. `isUnknownOutcome` → 记录 `uncertainEffect` + 置 `halted`
6. `lastMutationSeq` / `lastVerifySeq` 序号维护

第 2、3、5 条是这个仓库最有价值的执行安全规则，**但它们现在有两份实现**。
将来若调整（例如改为允许幂等操作在 halted 后继续），必须同时改两处，否则两条路径行为分叉。

---

## 2. Eino 循环里几个具体机制

### 2.1 `rewrite`：每次模型调用前的消息整形

`einoRunState.rewrite`（`eino.go:172-216`）做三件事：

1. **删掉旧的状态注入**：跳过以 `【本轮实际执行状态】` 开头的 system 消息，再在末尾重加一条。这样每轮只有一条而不是累积一堆。
2. **产图工具转 user 图片消息**：工具结果里带 `Image` 时，在对应 tool 消息后插一条 user 消息，内含 base64 图片。用掉即在 `s.images` 里 `delete`。
3. **中途压缩**：`estimateMessagesTokens >= midLoopCompactTokens()` 时调 `midLoopCompact`，保留 `midLoopKeepTail` 条尾巴。

最后追加的状态文本是：

> 已尝试执行 N 次工具调用；<核对状态>。失败或结果未知的操作不能声称完成。

其中 `<核对状态>` 有三档，由 `lastMutationSeq` 与 `lastVerifySeq` 的大小关系决定：
- 没有成功修改 → 「本轮尚无成功修改需要核对」
- 改过但没核对 → 「最后一次成功修改之后尚未调用核对工具」
- 改过且核对过 → 「最后一次成功修改之后已调用核对工具，请依据工具返回内容判断目标是否达成」

**这一条值得单独记住**：它是在用 prompt 表达运行时的真实状态，而不是让模型自己回忆。
`agent/execution_state_test.go` 钉住了这段。

### 2.2 `scanEinoOutput`：绕过 Eino 默认的「有内容就算有输出」

有些供应商在第一个工具调用块之前先吐一段短文本。Eino 默认的 `StreamToolCallChecker` 看到首个内容块就判 `false`，导致后面的工具调用被漏掉。

这里的实现自己扫完整条流：每收到一个非空 `Content` 就 `send(EvDelta)` 并记进 `modelText`；一旦收到 `ToolCalls` 立刻 `markCheckedToolCall()` 并返回 `true`。

`markCheckedToolCall` 会把当前累积的 `modelText` 存进 `toolPreface`——因为调用工具后模型的正文会被丢弃，而用户已经看到那段前言了。
`runEino` 在 356-360 行再把这段前言补回 `passText` 开头，避免用户看到的内容从事件流里消失。

### 2.3 三轮 pass 与两种反馈

```go
for pass := 0; pass < 3; pass++ { ... }
```

- `pass == 0`：若有未知结果操作，注入 `uncertainFeedback`
- `pass < 2` 且 `needsVerification`：注入 `verificationFeedback`
- 都没有：`send(EvDone)` 并返回

反馈是怎么注入的：拿 `state.lastHistory`（`rewrite` 里存下的）追加本轮的 assistant 文本 + 一条 system 反馈，作为新的 `input` 重跑。
所以反馈不是「打断」，而是**开启新的 pass，历史完整保留**。

上限是 3 轮，也就是说最多两次反馈，之后无论状态如何都结束。
这是有意的：**宁可收尾不完美，也不无限自我怀疑**。

### 2.4 `errEinoNeedsInput`

工具通过 `Result.AwaitUser` 把问题抛给用户时，`call` 会置 `s.needInput` 并返回 `errEinoNeedsInput`。
`runEino` 在两处捕获它（`Stream` 报错处、`reader.Recv` 报错处）并 `return nil`——**故意不当作错误**，因为流程是「等用户回答」而不是失败。

---

## 3. `web/chat.go`：会话层与两条分支

全文件 505 行，核心在 `streamReply`。职责分四块：

1. 侧栏会话列表（含自动标题回填 `backfillTitles`）
2. 会话 CRUD（`createConversation` / `listMessages` / `deleteConversation`）
3. `postChat`：解析请求、建/取会话、落用户消息、调 `streamReply`
4. `streamReply`：建 SSE → 分 agent / 直连 → 落库

### 3.1 全程「旧行为逐字节不变」

`postChat` 的请求体里每个新字段都写了缺省语义：

| 字段 | 缺省 | 说明 |
|---|---|---|
| `tools` | `false` | 不传走直连老路径 |
| `assistant_id` | `0` | 全部走用户全局设置 |
| `system_prompt` / `temperature` / `max_tokens` | 零值 | 归一化后行为与改造前一致 |
| `kind` | 空 → `work` | 记在会话上，切走再回来一致 |

**这是一条明确的工程约束**：这个仓库正在被持续改造，但任何新参数都不得改变旧客户端的观察结果。
后面加功能时应沿用这个习惯。

### 3.2 `streamReply` 的分岔

```go
if shouldUseAgent(toolsRequested) {
	in := turnInputFromGin(c)
	in.conv / in.userID / in.modelID / in.key / in.content / in.history / in.sendEvent = ...
	runAgentChat(in)
	return
}
```

两条路径唯一的共同点是 `sse.send` 被作为 `sendEvent` 传进 agent 分支。
**直连分支的 SSE 事件名是 `delta` / `done` / `error`；agent 分支的事件种类更多（工具调用、工具结果、需要输入）**，由 `runAgentChat` 自己发。
客户端因此必须两个都认——这是契约上的既成事实。

直连分支的错误处理有一个细节：`EventError` 时把**已累积的部分照样落库**（方便排查），但**不更新 `updated_at` 和标题**。

### 3.3 `rebuildLLMHistory` 的三个角色判断

这是全文注释最密的地方，也是最有判断力的地方：

- **`tool_trace` 不整条回灌**：`Detail` 可能很长，整条喂回去模型会把它当成对话正文。所以压成一行摘要（`toolTraceNote`），且**只注入最近 N 轮**（`traceNoteTurns()`），否则注记本身会把上下文吃掉。
- **子 agent 完成通知用 `role=user`**：它是运行时投回的**外部事件**，既不是用户说的也不是助手说的。注释点名参考实现 deepseek-harness 也是这么做的。
- **运行时旁注用 `role=system`**：注释原话——「冒充 user 会让模型以为用户提过这件事」。这句警告很有价值，**任何新加的运行时反馈都应遵守**。

### 3.4 `listMessages` 双契约

- 不传 `limit`：旧行为，`order by id asc`，直接返回数组
- 传 `limit`：游标分页，`id < before` + `order by id desc` + `limit+1` 判断 `has_more`，再反转成正序，返回 `{messages, has_more, before}`

分页上限 200。两个分支返回的 JSON 形状不同（数组 vs 对象），靠查询参数区分——**同一个 URL 有两种响应形状**，改的时候要小心。

---

## 4. 检索层：`rag/hybrid.go` 与 `rag/dynamics.go`

这两份文件是本次实读里**注释质量最高**的部分——设计意图直接写在文件头，读代码就能知道为什么这么做。

### 4.1 `hybrid.go`：三信号融合

融合公式来自 mem0 v3，三条关键设计写在文件头：

1. **语义分先过阈值再融合**（`semanticThreshold = 0.1`）——否则关键词/实体会把不相关的块拉进来。
2. `combined = (semantic + keyword + entity) / maxPossible`，`maxPossible` 随启用信号数自适应：
   仅语义 `1.0` / 语义+实体 `1.5` / 语义+关键词 `2.0` / 三者 `2.5`。
   权重：`keywordWeight = 1.0`，`entityWeight = 0.5`。
3. 语义先过采样出候选池，再**在池内**算关键词与实体，避免全量做昂贵计算。

`tokenize` 的中文处理值得注意：按**字符二元组**切（「发布口令」→ 发布/布口/口令），
因为中文没有空格，二元组是最省事又够用的近似。ASCII 与数字按连续串切并小写，
**单字符英文词直接丢弃**（噪声大），但 `-` 和 `_` 算词内字符，所以 `Galaxy-42`、`bge-m3` 不会被切开。

### 4.2 `dynamics.go`：记忆召回公式

```
活跃度半衰期 H = 30 × (1 + 2×重要性) 天     // 重要性越高衰减越慢
活动度 A(t)   = A₀ × 2^(-t/H)              // 稳定档案类恒为 1
情绪强度 E(t) = E₀ × 2^(-t/7)
优先级 P      = 相关性 × (0.55 + 0.25A + 0.15I + 0.05E)
强化 A'       = A + 0.2 × (1 - A)
```

**全部实现成纯函数**：不碰数据库、不读时钟（时间由调用方传入）。
这是有意的——公式本身可以离线单测，检索层只负责把库里的值喂进来。这个分层值得其他模块学习。

三条设计判断记在这里：

- **权重之和必须为 1**（`validate` 检查），这样优先级落在「相关性 × [0,1]」的量纲上，调参不会把相关度淹掉。
- **默认参数写错就 panic**，不静默用错公式。注释原话：「默认参数写错属于编程错误」。
- **铁律：自然淡化只影响排序，绝不删除行。** 活跃度再低，行也照样留在库里。

相比旧的「7 天内满分、之后最多衰减 15%」，多了重要性 I（重要的事衰减更慢，不被新写入的琐事盖过）
和情绪 E（半衰期 7 天，让刚发生的强烈感受优先浮现）两个维度。

### 4.3 与本文件其余部分的关系

`hybrid.go` 决定**取哪些块**，`dynamics.go` 决定**怎么排序**。两者都不写库，
所以记忆「怎么写进去」这次没读到——见第 6 节。

---

## 5. 与现有文档的偏差

| 偏差 | 文档现状 | 代码现状 |
|---|---|---|
| 当前默认循环 | `01-design.md` 描述 legacy `loop.go` | `08-eino-runtime.md` 称默认 Eino；`loop.go` 仍在且规则完备 |
| 执行规则位置 | `01-design.md` 写在循环内 | 两处：`loop.go` 内联 + `eino.go` 的 `state.call` |
| Eino 文档规模 | — | 1.5KB，未覆盖 adapter、rewrite、pass 循环、needsInput |

**建议（不擅自改旧文档）**：在 `01-design.md` 顶部加一行指向 `08` 与本文件，标明「本文描述 legacy 路径；当前运行时见 08/09」。是否要加，由维护者决定。

---

## 6. 待补读

- [ ] `web/agent.go`——`runAgentChat` 的真入口，事件序列的定义处
- [ ] `agent/eino_model.go`——`retryEinoModel` 的重试语义
- [ ] `agent/stream.go`——`Streamer()` 与压缩的关系
- [ ] `rag` 写入线——记忆怎么进库
- [ ] `agent/toolkit/`、`agent/fs/`、`agent/skill/`

---

*本文只记录读到的内容与判断，未修改任何代码。第 6 节列出的文件是下一轮的实读目标。*
