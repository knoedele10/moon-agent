# TASK: proactive — 主动聊天（借鉴 Cyrene 的策略状态机）

项目：E:\blog-my（Go + Gin + GORM + MySQL；桌面端由 Electron 起后端）。

## 前提

共享仓库：另外两个 worker 在改记忆（`rag/**`）和跨轮历史（`web/chat.go`）。
**你只能改下面列的文件**，不要动别人的文件。

## 你负责的文件

- 新建 `model/proactive.go`
- 新建 `web/proactive.go`、`web/proactive_policy.go`、`web/proactive_test.go`
- 新建 `templates/static/js/proactive.js`
- `model/code.go` —— **只允许**改 `AutoMigrate(...)` 那一行的模型列表

**不要改**：`web/web.go`（路由由主 agent 接）、`web/chat.go`、`web/agent.go`、`rag/**`、
`templates/chat/index.html`（主 agent 加容器与 script 标签）、`templates/static/js/chat.js`。

## 背景：为什么不能做成"定时发消息"

Cyrene 的主动聊天不是定时器，而是一整套**拦截条件 + 代次防错**。直接照抄它的策略：

```
NORMAL_QUIET_MS            = 30 分钟    （刚聊完不打扰）
GLOBAL_PROACTIVE_INTERVAL  = 2 小时     （全局冷却）
FOLLOWUP_INTERVAL          = 6 小时     （上一次没回时的追加间隔）
NIGHT_ACTIVE_IDLE_LIMIT    = 60 秒      （夜间空闲才允许）
FOLLOWUP_MIN_SCORE         = 85         （追加要求更高分）
```

拦截原因（要**原样保留语义**，便于排查）：
`disabled / screen_locked / conversation_busy / generation_busy / night_inactive /
normal_quiet_period / global_cooldown / scene_cooldown / unanswered_limit /
followup_cooldown / followup_same_scene / followup_score_too_low / stale_epoch`

## 一、策略层（纯函数，必须单测）

新建 `web/proactive_policy.go`，把 Cyrene 的策略翻成 Go，**全部是纯函数**：

```go
type ProactiveRuntimeSnapshot struct {
    Now              time.Time
    LocalHour        int
    IdleSeconds      int
    Enabled          bool
    ConversationBusy bool
    GenerationBusy   bool
}

type ProactiveCandidate struct {
    SceneID         string
    Score           int
    SceneCooldownMs int64
}

type ProactiveState struct {
    Epoch                    int64
    UnansweredCount          int   // 0~2
    LastProactiveAt          *time.Time
    LastProactiveScene       string
    LastNormalConversationAt *time.Time
    LastFiredAt              map[string]time.Time
}

func CanStartProactive(snap, state, candidate) (bool, string)  // 第二个返回值是拦截原因
func MarkUserActivity(state *ProactiveState)                  // epoch++、未回复清零
func MarkNormalConversationStarted(state *ProactiveState)     // epoch++
func MarkNormalConversationEnded(state *ProactiveState, now)  // epoch++、记录时间
func MarkProactiveCommitted(state *ProactiveState, c, now)
```

**`epoch` 的作用（照抄 Cyrene，别省）**：生成是异步的。用户在生成期间说话了 → `epoch++` →
生成完要提交时发现 `generationEpoch != state.Epoch` → 判 `stale_epoch` 丢弃。
**没有这个就会出现"用户刚说完话，一条过期的主动消息又冒出来"。**

单测要覆盖：每一个拦截原因、epoch 失效、夜间判定（22:00–08:00）、未回复数到 2 后不再发。

## 二、状态持久化

`model/proactive.go`：表 `agent_proactive_state`，按 user 一行：

| 列 | 说明 |
|---|---|
| user_id | 主键 |
| epoch | int64 |
| unanswered_count | int，0~2 |
| last_proactive_at / last_normal_conversation_at | *time.Time |
| last_proactive_scene | string |
| last_fired_json | text（sceneID → 时间，JSON） |
| enabled | bool，默认 true |
| updated_at | |

## 三、服务与调度

`web/proactive.go`：

- **调度**：一个后台 goroutine，`time.Ticker` 每 **5 分钟**跑一次（间隔从
  `AGENT_PROACTIVE_INTERVAL_MINUTES` 读，默认 5，<=0 关掉整个功能）。
  进程退出要能停（用 context）。
- 每轮对每个开启了主动聊天的用户：
  1. 组装 snapshot（时间、小时、空闲秒数——空闲用"距上次任何消息的时间"近似、
     `ConversationBusy`/`GenerationBusy` 从内存状态判断，拿不到就当 false）
  2. 生成候选：**先做一个简单的场景来源**——从用户的长期记忆里挑"值得追问"的
     （比如 7 天前提到过、且 importance 高、且没在最近聊过的），
     没有合适候选就**不发**（宁可安静）。score 先简单算（相关性/重要性/时间），
     预留成函数，后续可换。
  3. 过策略 → 生成一句话 → 落库 → 通知前端
- **生成**：用一次模型调用，提示词要求"以助手身份主动说一句话，20~40 字，自然、不客套、
  不要重复以前的问候，不要提'根据记忆'"。失败就**不发**（不要发一条降级的假消息）。
- **落库**：写进一个**专门的会话**（标题固定如「主动消息」，每个用户一条，不存在就建），
  role=`assistant`，并在消息上打标记便于前端区分（可以用 `role` 之外的字段吗？——
  不能改 message 表就**用 content 前缀或新增一张轻量表**，你自己选并在注释里写明理由）。
- **对外接口**（只写 handler，**不要加路由**）：
  - `GET  /api/proactive/status` → `{enabled, last_at, next_allowed_at, block_reason}`
  - `PUT  /api/proactive/settings` body `{enabled}` → `{ok:true}`
  - `POST /api/proactive/run-now` → 手动触发一次（**只用于调试/自测**，仍要过策略，
    可以额外允许绕过冷却但必须在响应里说明绕过了哪些）
  - `GET  /api/proactive/messages` → 最近的主动消息列表
  函数名：`proactiveStatus`、`proactiveSettings`、`proactiveRunNow`、`proactiveMessages`。

## 四、前端（独立 JS 文件）

新建 `templates/static/js/proactive.js`，暴露**幂等**的 `window.PROACTIVE = { init: function(){...} }`。
主 agent 会在 `templates/chat/index.html` 里加：

```html
<div id="proactive-banner" hidden></div>
```

要求：
- `init()` 后每 60 秒轮询一次 `/api/proactive/messages`；有新消息就显示横幅
  （「助手有话想跟你说」+ 点击跳到那个会话 + 一个「知道了」按钮）。
- 横幅样式**动态注入**（`proactive-` 前缀，复用页面已有的 CSS 变量），不要改 style.css。
- 不要用系统通知（Electron 权限链路复杂），就用页内横幅。
- 所有按钮带 `title` / `aria-label`。

## 硬性要求

- `model.DB == nil` 不 panic。
- 中文注释，写清每个常量的来历（"照抄 Cyrene 的默认值，理由：…"）。
- 策略层**必须**是纯函数且单测覆盖全部拦截分支。
- 验收：
  ```
  $env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'
  go build ./... ; go test ./... -count=1
  node --check templates/static/js/proactive.js
  ```

## 完成后报告

- 改了/新建了哪些文件
- 4 个 handler 的准确函数签名 + 表名 + 模型类型名
- 调度间隔与开关的环境变量名
- 你选的"主动消息怎么标记"的方案与理由
- 测试结果 + 你没做/不确定的地方
