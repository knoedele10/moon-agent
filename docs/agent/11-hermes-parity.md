# 11 Hermes 对标：工具调用与自我进化 v1（本轮契约）

> Lead 裁定，2026-10-21。参考源码：`references/hermes-agent-main/`（NousResearch/hermes-agent 主分支快照，只读）。
> 本文件冻结后各写域并行；改动需 Lead 复核并升版本。

## 0. 本轮目标

把 Moon Agent 已有的「平铺工具表 + 技能库 + soul_evolve」升级成 hermes 那套**可扩展工具编排 + 可度量的自我进化**：

| 能力 | 现状 | 本轮目标 |
|---|---|---|
| 工具编组 | `buildAgentTools` 一长串 append，30+ 工具全量暴露 | 声明式 Toolset + 预设 + 环境变量选择 |
| 工具发现 | 全量塞进每次请求 | Catalog + `tool_search` 渐进披露（运行时激活） |
| 参数健壮性 | 工具各自 `json.Unmarshal` | 统一 schema 净化 + 参数纠偏 + 预校验 |
| 大结果 | 循环里截断丢内容 | 落盘 + 头部摘要 + `read_tool_result` 分段读 |
| 并行 | 严格串行 | 只读工具并发执行、事件顺序稳定 |
| 轨迹 | 无 | 每次运行写 JSONL 轨迹（工具/耗时/成败/预算） |
| 技能进化 | 模型能 `skill_write`，无使用度量 | 使用账本 + 体检(lint) + 去重 + 从轨迹提炼候选 + 后台固化报告 |
| 记忆 | RAG 检索 | 补结构化记忆条目（topic + 去重 + forget），与 RAG 并存 |
| 度量 | 无 | 轨迹回放评测（工具调用质量分），证明进化是否有效 |

**参考映射**（只读源码时对照这几个文件）：

- 工具集：`toolsets.py`、`toolset_distributions.py`、`tools/registry.py`
- 渐进披露：`tools/tool_search.py`、`tools/tool_search_catalog.py`
- schema/参数：`tools/schema_sanitizer.py`、`tools/arg_coercion.py`、`agent/turn_tool_validation.py`
- 大输出：`tools/tool_result_storage.py`、`tools/tool_output_truncate.py`、`tools/hook_output_spill.py`
- 并行：`agent/tool_call_batches.py`、`agent/tool_executor.py`
- 技能：`tools/skill_manager_tool.py`、`tools/skills_tool.py`、`tools/skill_linter.py`、`tools/skill_usage.py`、`tools/skills_guard.py`
- 记忆：`tools/memory_tool.py`、`tools/memory_tool_store.py`、`agent/memory_manager.py`
- 学习回路：`agent/curator.py`、`agent/learn_prompt.py`、`agent/learning_graph.py`、`agent/learning_mutations.py`、`agent/background_review.py`、`agent/insights.py`
- 轨迹/压缩：`trajectory_compressor.py`、`agent/trajectory.py`、`agent/context_compressor*.py`
- soul/自模型：`SOUL.md`、`agent/runtime_self_protection.py`

## 1. 铁律（违反即打回）

1. **向后兼容**：所有新能力默认关闭。`AGENT_*` 环境变量未配时，`/api/chat` 的 SSE 序列、工具清单、提示词与改造前**逐字节等价**。
2. **CUA 冻结**：`internal/web/cua*.go`、`cua/**`、`packaging/**`、`electron/**`、`templates/static/js/cua.js` 一行不改。
3. **`llm` 契约不动**：只允许追加字段/方法，`Registry.Stream`/`Generate` 行为不变（CUA 依赖 `Generate`）。
4. **沙箱红线不动**：`internal/agent/fs/**` 的路径校验与观察策略不改；新工具的写盘路径必须走 `fs` 沙箱或独立数据目录。
5. **质量门**：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`gofmt -l .` 全清；每个新包必须有确定性单测（不连真实模型/MySQL）。
6. **中文注释与文案**：本仓库注释、错误文案、工具描述一律中文（错误码保留英文大写）。
7. **不新增数据库迁移**：能落文件就不动表；确需表时先找 Lead。

## 2. 模块布局与写域

| 写域 | 目录/文件 | 负责人 |
|---|---|---|
| A 工具编排 | `internal/agent/toolsys/**`（新包） | tools-engineer |
| B 自我进化 | `internal/agent/evolve/**`（新包） | evolve-engineer |
| C 接线 | `internal/agent/loop.go`、`eino.go`、`internal/web/agent_tool_policy.go`、`internal/web/agent.go`、`internal/web/evolve.go`（新）、`internal/web/web.go` 路由行 | Lead |
| D 文档 | `docs/hermes-parity/**`（新）、`docs/agent/11-hermes-parity.md` | hermes-analyst |
| E 验证 | `docs/verify/**`、跨包集成测试 `internal/web/hermes_*_test.go` | verifier |

跨域只提 PR 式说明，不改别人的文件；需要对方改的，`send_message` 给对方 Lead。

## 3. A 包：`internal/agent/toolsys`

```go
package toolsys

// Risk 是工具风险等级，决定 confirm 档是否拦、并行是否可以开。
type Risk string
const (
    RiskRead    Risk = "read"    // 只读：可并行
    RiskWrite   Risk = "write"   // 写用户文件：confirm 档拦
    RiskExec    Risk = "exec"    // 执行命令/代码
    RiskNetwork Risk = "network" // 出网
    RiskSelf    Risk = "self"    // 改变自身行为（技能/记忆/人格）
)

// Toolset 是一组工具的声明式分组。
type Toolset struct {
    Name    string       // 如 "fs"、"memory"、"evolve"
    Desc    string       // 一句话，进提示词的工具集清单
    Depends []string     // 依赖的其它 toolset（自动带上）
    Risk    Risk         // 该组默认风险（单个工具可覆盖）
    Tools   []agent.Tool
}

// Entry 是目录里的一个工具。
type Entry struct {
    Tool    agent.Tool
    Toolset string
    Risk    Risk
    Tags    []string // 检索用别名，如 read→["查看","打开","文件"]
}

// Catalog 是运行期工具目录：全量工具 + 已激活集合。
type Catalog struct {
    /* 内部：mu、reg *agent.Registry、entries map[string]Entry、sets []Toolset、active map[string]bool */
}

func NewCatalog(sets ...Toolset) *Catalog
func (c *Catalog) Registry() *agent.Registry          // 交给循环执行
func (c *Catalog) Defs() []llm.ToolDef                // 只含已激活（含核心集）
func (c *Catalog) Lookup(name string) (agent.Tool, bool)
func (c *Catalog) Activate(names ...string) (activated []string) // 幂等
func (c *Catalog) ActivateToolset(names ...string) []string
func (c *Catalog) Search(query string, limit int) []Match        // 关键词+别名+描述打分
func (c *Catalog) Toolsets() []ToolsetInfo                       // 给提示词/HTTP 用
func (c *Catalog) Len() (total, active int)
```

- `Search` 打分：名字精确 > 名字前缀 > 别名命中 > 描述子串；同分按 toolset 顺序稳定。
- `Defs()` 必须稳定排序（先激活顺序，再 toolset 声明顺序），否则提示词缓存全废。

### 3.1 装饰器（每个工具自动套）

```go
func Sanitize(t agent.Tool) agent.Tool // Parameters() 去掉 $schema/$id/$defs/invariant 等非标准键
func Coerce(t agent.Tool) agent.Tool   // 参数纠偏：JSON 字符串→对象、数字字符串→数字、布尔字符串→布尔
func Validate(t agent.Tool) agent.Tool // 按 schema 预校验 required/type/enum；失败返回可修复文案（is_error=true）
```

失败文案要求：说清**哪个字段、期望什么、收到什么、怎么改**，不带堆栈。

### 3.2 大结果落盘

```go
type SpillPolicy struct {
    Dir       string // 空 → 默认数据目录/spill
    MaxBytes  int    // 默认 32KB
    HeadRunes int    // 默认 2000
}
func (p SpillPolicy) Wrap(t agent.Tool) agent.Tool // 超限写盘，Content 换成"头部+落盘路径+继续读方法"
func ReadResultTool(p SpillPolicy) agent.Tool      // 工具名 read_tool_result(file_path, offset?, limit?)
```
落盘文件必须不可执行、目录 0700、文件名含 session 与哈希；同一份内容重复落盘要复用（按内容哈希）。

### 3.3 并行执行

```go
// Plan 把一次模型返回的调用序列切成批次：连续只用读工具的调用并成一批。
func Plan(calls []llm.ToolCall, riskOf func(name string) (Risk, bool)) [][]int
// RunBatch 并发执行同批调用，按输入顺序返回结果。
func RunBatch(ctx context.Context, ex *agent.Exec, reg *agent.Registry, calls []llm.ToolCall, maxParallel int) []agent.Result
```
红线：写/执行/网络工具**永不**进同一批；并发上限默认 4；`ctx` 取消后不再启动新调用。

### 3.3.1 落地说明（Lead 补记，2026-10-21）

并行有两层，缺一不可：

1. **包内**：`toolsys.Plan` / `toolsys.RunBatch` 供目录路径复用（按 `RiskOf` 判只读）。
2. **循环内**：`internal/agent/parallel.go` 的 `planCallGroups` / `runReadOnlyBatch` 把同一轮
   模型返回的调用切成「连续只读批 + 单个其它调用」，由 `agent.Config.Parallel` 打开。
   - 白名单来自 web 层 `readOnlyToolNames()`（**具名白名单**，不是风险表推断）：
     `run_command`、`office_edit`、`skill_run`、`memory_note`、`tool_search` 这类
     「看起来无害、实际会改状态」的调用永不进并发批。
   - 事件与 tool 消息仍按模型给出的**输入顺序**回放；并发只影响执行，不影响呈现与历史。
   - 自主模式（`LimitRecoveryRounds > 0`，预算记账串行）下**不并发**，避免绕过触顶检查。
   - Eino ReAct 执行器同样支持：工具节点并发执行，`callGated` 用读写锁保证只读共享、写独占；
     预算/镜像/执行记账已加锁。2026-10-25 第二轮补齐。
   - 默认关闭；`AGENT_TOOL_PARALLEL=read` 打开，`AGENT_TOOL_PARALLEL_MAX` 覆盖并发上限（默认 4）。


### 3.4 工具

```go
func SearchTool(c *Catalog) agent.Tool // 工具名 "tool_search"
```
- 参数：`query`(必)、`limit`(默认 5，上限 12)、`activate`(默认 true)。
- 结果：命中的工具名 + 一句话 + 是否已激活；`activate=true` 时真的激活并在文案里告诉模型「下一轮可直接调用」。
- 描述里要写明：**先用它找工具，别猜参数**。

## 4. B 包：`internal/agent/evolve`

```go
package evolve

// ---- 轨迹 ----
type RunMeta struct {
    RunID string; SessionID, UserID, ConversationID string
    Model, Engine, Mode string
    StartedAt time.Time
}
type RunRecord struct {
    RunMeta
    ToolCalls []ToolCallRecord `json:"tool_calls"`
    Outcome   string           `json:"outcome"` // ok | limit | error | interrupted | await_user
    Note      string           `json:"note"`
    FinishedAt time.Time       `json:"finished_at"`
    Rounds, Tokens int          `json:"rounds,omitempty"`
}
type ToolCallRecord struct {
    Name string; Args string; OK bool; IsError bool
    Summary string; MS int64; Spilled bool
}
type Recorder struct { Dir string; Now func() time.Time }
func NewRecorder(dir string) *Recorder
func (r *Recorder) Start(meta RunMeta) *Run
func (run *Run) Tool(rec ToolCallRecord)
func (run *Run) Finish(outcome, note string) error   // 原子追加到 Dir/trajectories/YYYY-MM-DD.jsonl
func (r *Recorder) List(days int) ([]RunRecord, error)
func (r *Recorder) CompressBefore(cutoff time.Time) (CompressReport, error) // 旧轨迹聚合成 Dir/archive/*.jsonl + 摘要
```
要求：并发安全（run 内加锁）、单行 JSON 不超过 32KB（超长 args/summary 截断并标记）、写失败**不得**影响对话（调用方降级）。

```go
// ---- 技能使用账本 ----
type SkillUse struct { Name string; OK bool; Reason string; At time.Time }
type SkillStat struct { Name string; Uses, OK, Fail int; LastUsed time.Time; SuccessRate float64 }
type Ledger struct { Path string }
func (l *Ledger) Record(u SkillUse) error
func (l *Ledger) Stats() ([]SkillStat, error)
func (l *Ledger) Stat(name string) (SkillStat, bool, error)
func (l *Ledger) AtRisk(minUses int, limit int) ([]SkillStat, error) // 失败率高或久未使用
```

```go
// ---- 技能体检 ----
type SkillStore interface {
    List(userID string) ([]SkillItem, error)
    Get(userID, name string) (SkillItem, bool, error)
    Upsert(userID string, it SkillItem) (created bool, err error)
}
type SkillItem struct { Name, Description, Body, Source string; Enabled bool }
type Issue struct { Code, Message string; Severity string } // warning | error
func Lint(it SkillItem) []Issue
func Duplicates(items []SkillItem, threshold float64) [][2]string // jaccard 相似度
func Dangerous(body string) []Issue  // 明显危险命令/凭证泄漏/超长正文
```
Lint 规则（至少）：名字 slug 合法、描述 20–200 字、正文有「何时使用/怎么做」、正文 ≤ 64KB、含疑似密钥即 error、含 `rm -rf /`、`format`、`curl|sh` 等即 error。

```go
// ---- 从轨迹提炼（纯函数，可单测；LLM 化留给接线层） ----
type Candidate struct {
    Kind string // skill | memory | soul
    Name, Description, Body string
    Evidence []string
    Confidence float64
}
func Candidates(run RunRecord) []Candidate
```
提炼规则（确定性、有据可依）：同一工具序列重复 ≥2 次且最终 ok → 技能候选；一次运行里同工具失败 ≥2 次后成功 → 技能候选（记教训）；用户纠正/预算触顶 → memory 候选（下次注意）。

```go
// ---- 后台固化 ----
type Report struct {
    At time.Time
    Runs, Skills int
    Duplicates [][2]string
    AtRisk []SkillStat
    Candidates []Candidate
    Applied []string
}
type Curator struct {
    Store SkillStore
    Ledger *Ledger
    Recorder *Recorder
    Apply bool   // false = 只报告（suggest），true = 真的落库（auto）
    MaxMutates int // 单次固化最多改几条，默认 3
}
func (c *Curator) Run(ctx context.Context, userID string) (Report, error)
func (c *Curator) LastReport(dir string) (Report, bool, error) // 报告落 Dir/last-report.json
```

```go
// ---- 结构化记忆（与 RAG 并存） ----
type MemoryEntry struct { ID, Topic, Text string; CreatedAt, UpdatedAt time.Time; Hits int }
type MemoryFile struct { Path string; MaxEntries int; MaxRunes int } // 默认 200 条 / 8000 字
func (m *MemoryFile) Add(topic, text string) (created bool, err error) // 去重（规范化后相同视为重复）
func (m *MemoryFile) Search(query string, limit int) []MemoryEntry
func (m *MemoryFile) Remove(id string) (bool, error)
func (m *MemoryFile) All() ([]MemoryEntry, error)
func (m *MemoryFile) Block() string // 注入提示词的 <memory> 块（空则不注入）
```

```go
// ---- 轨迹回放评测 ----
type EvalCase struct { ID, Prompt string; WantTools []string; MaxFailRate float64 }
type EvalResult struct { Case EvalCase; Run RunRecord; Score float64; Notes []string }
func Score(run RunRecord, c EvalCase) float64  // 工具命中率/失败率/是否 aborted
func Aggregate(results []EvalResult) (mean float64, n int)
```

### 4.1 B 包工具（工具名固定，Web 层注册）

| 工具 | 参数 | 作用 |
|---|---|---|
| `skill_list` | `query?`, `only_at_risk?` | 列出技能 + 使用统计 + lint 摘要 |
| `skill_stats` | `name?` | 单条或全部技能的 uses/成功率/最近使用 |
| `skill_lint` | `name` | 体检一条技能，返回问题清单 |
| `skill_improve` | `name`, `patch`(新的正文或章节替换) | 在**旧正文基础上**改技能，写前先 lint |
| `skill_archive` | `name`, `reason` | 归档（不删除，标记 enabled=false） |
| `memory_note` | `topic`, `text` | 写结构化记忆 |
| `memory_list` | `topic?` | 读结构化记忆 |
| `memory_forget` | `id` 或 `text` | 删一条 |
| `evolution_report` | — | 读最近一次固化报告 |

## 5. C 接线（Lead 负责，别人不动）

- `agent.Config` 追加 `ToolSource ToolSource`（`Defs()` + `Lookup()`）；为空走原路径。
- `loop.go`：每轮用 `ToolSource.Defs()` 重建 `defs`（渐进披露生效）；执行走 `Lookup`。
- `eino.go`：每个 pass 重建 adapters；`MessageRewriter` 注入本轮新激活的工具定义（eino 的静态工具表限制，文档写明）。
- `internal/web/agent_tool_policy.go`：把现有平铺工具改成 Toolset 声明；`AGENT_TOOLSETS` 选择；`AGENT_TOOL_DISCLOSURE` 决定是否只给核心集 + `tool_search`。
- `internal/web/evolve.go`：环境变量、数据目录、Recorder/Ledger/Curator 装配；`AGENT_EVOLVE` 控制 off/record/suggest/auto。
- 系统提示词追加：工具集清单（可选）、技能索引（已有 `skillPromptBlock`）、`<memory>` 块、自进化使用须知（何时 `skill_write`/`memory_note`）。
- 路由：`GET /api/agent/evolution/report`、`GET /api/agent/evolution/skills`、`POST /api/agent/evolution/curate`（仅 owner）。

## 6. 环境变量（全部默认关闭，缺省 = 现状）

| 变量 | 取值 | 缺省 |
|---|---|---|
| `AGENT_TOOLSETS` | 逗号分隔 toolset 名；空=现状全量 | 空 |
| `AGENT_TOOL_DISCLOSURE` | `off` \| `search` | `off` |
| `AGENT_TOOL_PARALLEL` | `off` \| `read` | `off` |
| `AGENT_TOOL_PARALLEL_MAX` | 整数，只读并发上限；0=默认 4 | `0` |
| `AGENT_TOOL_SPILL_BYTES` | 整数，0=关闭 | `0` |
| `AGENT_EVOLVE` | `off` \| `record` \| `suggest` \| `auto` | `off` |
| `AGENT_EVOLVE_DIR` | 目录 | 用户配置目录/blog-my/evolve |
| `AGENT_SKILL_LINT` | `off` \| `warn` \| `block` | `warn` |

## 7. 验收标准

1. `go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`gofmt -l .` 全清。
2. 关掉全部新开关时：`git diff` 里 `/api/chat` 行为无变化，既有测试全过（尤其 `TestAgent*`、CUA 全量）。
3. **工具编排**：给一个 3 toolset 的假目录，断言 `AGENT_TOOLSETS=fs,memory` 时只暴露这两组；`tool_search` 激活后**下一轮**模型请求里出现新工具定义（用 mock 模型断言）。
4. **参数健壮**：字符串化 JSON、数字字符串、缺必填三类输入分别得到纠偏成功/可修复报错。
5. **落盘**：60KB 假结果 → Content 只含头部 + 路径；`read_tool_result` 能读到后面。
6. **并行**：3 个只读调用并发（用 sleep 断言总耗时 < 串行 1/2），写工具与读工具不混批；事件顺序仍是输入顺序。
7. **进化**：写 3 条轨迹 + 账本 → `Curator.Run(Apply=false)` 输出报告且**不**改库；`Apply=true` 最多改 `MaxMutates` 条；重复运行幂等。
8. **记忆**：`memory_note` 去重、`memory_forget` 生效、`Block()` 超限裁剪。
9. **度量**：给定轨迹与用例，`Score` 输出确定值（表驱动）。
10. **负向对照**：危险技能正文必须 `lint` error；`AGENT_EVOLVE=off` 时不得产生任何轨迹文件。
