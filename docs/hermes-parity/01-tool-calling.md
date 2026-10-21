# 01 工具调用：Hermes 逐机制对照

> 路径相对 `references/hermes-agent-main/`。每个机制四段：**Hermes 做法（引用）→ 关键算法 → 我们的 Go 对应实现（契约 §3）→ 差异与风险**。
> Go 侧新包 = `internal/agent/toolsys`；接入点 = `internal/agent/loop.go`、`internal/agent/eino.go`、`internal/web/agent_tool_policy.go`。

## 0. 总览：机制与取舍

| # | 机制 | Hermes | 我们 | 本轮判断 |
|---|---|---|---|---|
| 1 | 工具编组 | 声明式 `TOOLSETS` 数据表 | 声明式 `Toolset` 结构 | 对齐，去掉 build/tag/composite |
| 2 | 渐进披露 | 三桥 `tool_search`/`tool_describe`/`tool_call` + BM25 + listing 三级降级 | 单工具 `tool_search`（名字/前缀/别名/描述打分） | 降级实现，保留「下一轮生效」语义 |
| 3 | schema 净化 | `sanitize_tool_schemas`（含属性名重命名与反解） | `Sanitize` 装饰器（只去非标准键） | 最小可用版 |
| 4 | 参数纠偏 | `coerce_tool_args`（schema 驱动） | `Coerce` 装饰器（同思路，类型更少） | 对齐 |
| 5 | 预校验 | 循环层批次校验 + deferred 局部 jsonschema | 工具级 `Validate` 装饰器 | 位置不同，效果等价 |
| 6 | 大结果 | 落盘 + 预览 + `read_file` 分页 + 每轮总预算 | 落盘 + `read_tool_result` | 去掉总预算 |
| 7 | 并行 | 有序段规划（路径冲突切段）+ 8 worker | `Plan`/`RunBatch`（Risk 分批）+ 4 worker | 简化：不按路径切段 |
| 8 | 审批 | 命令内容级 floors + manual/smart/off + yolo | 工具名级 `Risk` + 前端确认 | **方向不同，只借 Risk 概念** |

## 1. Toolset：把「暴露哪些工具」变成数据

### 1.1 Hermes 做法

- 单一字典 `toolsets.py:TOOLSETS`（第 74 行起），每个条目由 `_ts()` / `_bundle()` / `_core_without()` 三种工厂造出：`_ts("文件读写", ["read_file", ...], includes=[...])`、`_bundle(...)` 把多个 toolset 合成复合集、`_core_without(...)` 在核心集上做减法。
- `toolsets.py:_HERMES_CORE_TOOLS` 是 51 个工具名的默认集；`CLIENT_SURFACE_TOOLSETS` 区分「终端/桌面专用」的集合，避免把 GUI 工具发给消息平台。
- 每个平台的启用集由 `model_tools.py:get_tool_definitions(enabled_toolsets, disabled_toolsets)` 决定：`enabled=None` 表示全量；`disabled` **最后**减，所以复合集重新开出来的工具也会被减掉（源码注释引 issue #17309）。
- 解析顺序在 `toolsets.py:resolve_toolset`（`includes` 递归展开、去重、保序），`get_toolset_info` 暴露 direct/includes/resolved/count/is_composite 给 UI/`hermes tools` 用。
- 可用性（有没有 API key、连没连 MCP）不是 toolset 管的，是 `tools/registry.py:get_definitions`（第 896 行）用每个工具的 `check_fn` 过滤，结果 TTL 缓存；`tools/AGENTS.md` 明确写死：**toolset 是「表面」的闸门，`check_fn` 只回答「可达/已开启」，不回答「谁在用」**。
- 另外有一张跟「工具集选择」正交的表 `toolset_distributions.py:DISTRIBUTIONS`：批量数据生成时按概率采样工具集（键可以是 `"browser+search"` 复合，一次掷骰决定整组，避免两次独立 97% 只有 ~94% 共现）。

### 1.2 关键算法（简化）

```
def defs(enabled, disabled):
    names = set()
    for ts in (enabled or all_toolsets()):
        names |= resolve_toolset(ts)        # includes 递归展开
    if disabled: names -= union(resolve_toolset(t) for t in disabled)
    return [schema(t) for t in names if check_fn_ok(t)]   # check_fn TTL 缓存
```

### 1.3 我们的 Go 对应（契约 §3）

```go
type Toolset struct {
    Name, Desc string
    Depends    []string   // 替代 includes：显式依赖而非包含同义反复
    Risk       Risk       // read/write/exec/network/self，全集取最严
    Tools      []Entry
}
type Catalog struct{ /* mu, reg *agent.Registry, entries map[string]Entry, sets []Toolset, active map[string]bool */ }
func NewCatalog(sets ...Toolset) *Catalog
func (c *Catalog) Defs() []llm.ToolDef        // 只含已激活工具，稳定排序
func (c *Catalog) Activate(names ...string) (activated []string)
func (c *Catalog) ActivateToolset(names ...string) []string
func (c *Catalog) Toolsets() []ToolsetInfo
```

- 接入：`internal/web/agent_tool_policy.go:buildAgentTools` 现在是一串 `tools = append(tools, ...)`（第 56–120 行），改造后按 kind/mode 选 toolset 名 → `NewCatalog` → 交给循环；`internal/agent/loop.go:140/185` 已经每轮 `source.Defs()`，`agent.ToolSource`（`internal/agent/tool.go:39`）就是为动态目录预留的接口，**循环一行不用改**。
- `AGENT_TOOLSETS` 为空 = 现状全量（契约 §6），因此默认路径下 `Catalog.Defs()` 必须与 `Registry.Defs()` 逐字节一致（同为 llm.ToolDef 切片，同序）。

### 1.4 差异与风险

| 点 | Hermes | 我们 | 风险 |
|---|---|---|---|
| 依赖表达 | `includes` 包含关系（可多层组合） | `Depends` 单向依赖 | 复合 toolset 表达力弱；先够用 |
| 平台裁剪 | `CLIENT_SURFACE_TOOLSETS` + per-platform 配置 | 无 | 若将来接第二个入口，需要补 |
| 可用性过滤 | `check_fn` TTL 缓存，与 toolset 正交 | 构造期直接不给（`buildAgentTools` 里 `if root != ""`） | 简单但不可动态；`riskOf` 需对未知工具返回 false 保守值 |
| 顺序 | resolve 后按注册顺序 | `Defs` 必须稳定：先激活顺序再声明顺序 | 顺序抖动 = 提示词缓存全废（Hermes 核心不变量） |

## 2. tool_search：渐进披露

### 2.1 Hermes 做法

- 三个桥接工具名在 `tools/tool_search_catalog.py:TOOL_SEARCH_NAME/TOOL_DESCRIBE_NAME/TOOL_CALL_NAME`，集合 `BRIDGE_TOOL_NAMES`。`tool_search` 只给「名字 + 一句话」，`tool_describe` 才给完整 schema，`tool_call` 负责真正调用。
- 判定哪些工具被藏起来：`tools/tool_search.py:is_deferrable_tool_name` —— MCP 工具（`toolset` 以 `mcp-` 开头）一律可藏；内置工具只有在 toolset 不在 `_DIRECT_SURFACE_TOOLSETS` 里（或用户在 `tools.tool_search.defer` 里显式点名）才可藏。核心工具（`_HERMES_CORE_TOOLS`）默认保持 eager。
- 触发：`tool_search.py:should_activate` —— `enabled == "off"` 永不激活；只要存在**任意**可 defer 的工具就激活（`"auto"` 与 `"on"` 今天等价，源码注释明确说别区分）。`deferrable_tokens` 用 `estimate_tokens_from_schemas`（chars/4）估算。
- 装配：`tool_search.py:assemble_tool_defs` —— 先剥掉已有桥接工具（幂等）；无可 defer 工具则原样返回（tier 0）；有则按 listing 预算决定 tier：**tier 1** = 可见工具 + 三桥 + 名称清单；**tier 2** = 只有三桥（清单装不下）。`AssemblyResult` 记录 activated/deferred_count/deferred_tokens/threshold_tokens/tier/listing_form。
- listing 预算：`listing_token_budget` = `min(listing_max_tokens, context_length * threshold_pct%)`，默认 `threshold_pct=5`、`listing_max_tokens=4000`（`hermes_cli/config_defaults.py` 第 2005–2040 行）；上下文未知时用 10K 作为百分比腿。
- listing 降级：`tool_search_catalog.py:build_catalog_listing_with_form` 依次尝试 full（名字+首句）→ names（仅名字）→ groups（按 server/toolset 归并 + 计数），每步都 `math.ceil(len/4) <= max_tokens` 才接受，降级时**合并最大的一组**而不是整体截断。
- 检索：`tool_search_catalog.py:search_catalog` 是 BM25（`_bm25_score`），语料是每次装配时从当前 tool defs **重建**的无状态目录（模块 docstring：不持有会话态）；`_entry_search_text` 把工具名按 `_`/`-` 切词、加 source 标签、描述、顶层参数名，去掉 `mcp__` 前缀；Snowball 词干化 `_stem`（per-thread lru 16384）；门控 `_gate_token` + `MIN_QUERY_TERM_COVERAGE=0.5`、`MIN_ANSWERABLE_TERMS_FOR_COVERAGE=4`，避免「全覆盖才算命中」把长查询打死。
- 调用约束：`dispatch_tool_search` 每次最多 `_MAX_QUERIES_PER_CALL=7` 条 query，`limit` 默认 5、上限 25（`_clamped_int` 夹取）；`dispatch_tool_describe` 每次最多 10 个名字。
- 桥接描述即提示词：`_search_description` 会按 listing_form 追加不同话术（groups 时说「这里列出的域先搜这里，别声称不可用」），并提示「已在工具列表里的工具不用搜」。
- 下一轮才生效：装配发生在请求前，激活写回会话的工具集在**下次**装配时体现；`AssemblyResult.tier` 只是可观测字段。

### 2.2 关键算法（简化）

```
def assemble(tool_defs, ctx_len):
    visible, deferrable = classify(tool_defs)      # 按 toolset 白名单 + 显式 defer
    if not active(config) or not deferrable: return passthrough
    budget = min(listing_max_tokens, ctx_len * threshold_pct / 100)
    listing, form = build_listing(deferrable, budget)   # full → names → groups
    bridge = [search_schema, describe_schema, call_schema(listing, form)]
    return visible + bridge                        # 被藏掉的工具从数组里消失
```

### 2.3 我们的 Go 对应

```go
func (c *Catalog) Search(query string, limit int) []Match  // 名字精确 > 名字前缀 > 别名命中 > 描述子串；同分按 toolset 顺序
func SearchTool(c *Catalog) agent.Tool                     // 工具名 "tool_search"
```
- 参数 `query`(必)、`limit`(默认 5，上限 12)、`activate`(默认 true)；`activate=true` 时调用 `Catalog.Activate`，并在结果文案里明说「下一轮可直接调用」。
- 接入：`AGENT_TOOL_DISCLOSURE=search` 时，`Catalog.Defs()` 只返回核心集 + `tool_search`；`loop.go` 每轮重新取定义（第 183–185 行注释已经写明这是为动态目录准备的）。

### 2.4 差异与风险

| 点 | Hermes | 我们 | 风险/对策 |
|---|---|---|---|
| 桥接件数 | 3 个（搜索/看 schema/调用） | 1 个（搜索 + 激活） | 模型看不到 schema 就「盲调」；对策：`Search` 结果带必填参数名，`Validate` 兜住缺参（Hermes 为此专门加了 `tools/tool_search_validation.py:validate_deferred_call_args`） |
| 检索算法 | BM25 + 词干 + 覆盖率门控 | 四档加权 | 中文无词干问题，反而更稳；但模糊查询（"看下服务器日志"）命不中 `read_log`，需要 Tags 别名表（契约 §3 `Entry.Tags`） |
| listing | 三级降级 + token 预算 | 无（工具数少） | 若工具涨到 100+ 必须补，否则 bridge 描述自己就撑爆上下文 |
| 检索范围 | 全量 deferred（含 MCP、connector） | 当前 Catalog 里的非核心工具 | 无跨 toolset 搜索；`AGENT_TOOLSETS` 裁剪后不可见的工具不该被搜到，这点必须显式实现 |
| 激活副作用 | 无状态、每轮重算 | `active map` 有状态 | 状态必须挂在**本次运行**的 Catalog 上，不能进程级共享，否则并发会话互相污染 |

## 3. Schema 净化

### 3.1 Hermes 做法

`tools/schema_sanitizer.py` 的模块 docstring 就是一份后端兼容清单（Anthropic 的 `^[a-zA-Z0-9_-]{1,64}$` key 限制、llama.cpp 不支持 `anyOf` 的 `object` 分支、部分后端拒绝 `pattern`/`format`、OpenAI 系要求 `type` 是字符串而不是数组）。对外只有两三个入口：

- `sanitize_tool_schemas(schemas)`：递归走 `_sanitize_node`。
- `sanitize_property_key`（正则 `_PROP_KEY_RE = ^[a-zA-Z0-9_.-]{1,64}$`）+ `_rename_property_keys`：不合法属性名按确定性规则改名，冲突追加后缀；**同时登记反向映射**，由 `unrename_tool_args` 在分发前还原，否则模型按新名字传参、工具按旧名字读不到。
- `_strip_top_level_combinators`：顶层 `anyOf`/`oneOf`/`allOf` 拍平成 `properties` 并集（llama.cpp 系）。
- `strip_nullable_unions` / `collapse_const_unions`：`type: ["string","null"]` → `type: string`；`anyOf: [{const: "x"}]` → `enum: ["x"]`。
- `strip_pattern_and_format`、`strip_slash_enum`：是可选的「恢复模式」清洗，只在目标后端已知会拒时开。
- `resolve_local_schema_ref`：只解析本地 `#/...`（RFC6901），**不碰外部 ref**（与 §5 的 `_schema_has_external_ref` 呼应：本地校验绝不触发网络/文件读取）。

### 3.2 关键算法（简化）

```
def sanitize(node):
    if node.type is list:    node.type = first non-"null"
    if not node.properties and node.type == "object": node.properties = {}
    for k in keys(node.properties):
        new = sanitize_property_key(k)
        if new != k: rename_map[new] = k; 重建 properties
    strip top-level anyOf/oneOf/allOf → properties
    recurse
```

### 3.3 我们的 Go 对应

```go
func Sanitize(t agent.Tool) agent.Tool  // Parameters() 去掉 $schema/$id/$defs/$comment/invariant 等非标准键
```
- 位置：装饰器套在 `Entry` 装配处（`Catalog` 注册时），不改 `agent.Tool` 接口；`internal/agent/tool.go:156 Registry.Defs` 已处理 `Parameters()==nil` 的补空 schema，Sanitize 只做减法。
- 因为 Go 侧工具自己解析 `json.RawMessage`（`internal/agent/tool.go:24` 的注释即契约），本轮**不做属性名重命名**（我们的工具没有非法 key），只做「去非标准键 + 顶层 object 兜底」。

### 3.4 差异与风险

- 最大差异：Hermes 的净化是**为不同后端准备多套开关**（`strip_pattern_and_format` 等按需开）。我们只面对少数几家 OpenAI 兼容后端，去掉 `$schema`/`$defs` 就能解决 400；**不要提前实现**重命名那套，否则要同时维护反向映射，得不偿失。
- 风险：净化必须幂等且不动 `required`/`properties` 的语义；`Sanitize` 覆盖 `Parameters()` 返回的 map 时要深拷贝，别就地改工具持有的 schema（并发会话会互相污染）。
- 若某天后端要求 `additionalProperties:false`，届时再加开关，不要现在写死。

## 4. 参数纠偏（coercion）

### 4.1 Hermes 做法

`tools/arg_coercion.py:coerce_tool_args(tool_name, arguments)`：从 `registry.get_schema(name)` 取 schema，先 `unrename_tool_args`（还原 §3 的属性改名），再按 schema 递归 `_coerce_value`：

- 期望 `array` 但收到裸标量 → 包成一元素数组；收到 JSON 字符串 `"[1,2]"` → 先 `json.loads` 再递归。
- `_SCALAR_COERCERS` 覆盖 integer/number/boolean/array/object：`"3"`→3、`"true"`→true、`"1.5"`→1.5；`_coerce_number` 遇到 `NaN`/`inf` 明确**不转**（保持原值让 schema 校验报错，而不是悄悄变成 0）。
- 对象的嵌套值里若出现「看起来是 JSON 的字符串」也做一次规范化，避免模型把对象序列化成字符串塞进字段。
- 只在**明确可修**时改；歧义（如 `"01/02/03"` 到底是不是日期）保留原值，交给校验层给可读错误。

### 4.2 关键算法（简化）

```
def coerce(args, schema):
    if schema.type == "array" and not is_list(args): args = [args] if scalar else json.loads(args)
    for k, sub in schema.properties:
        v = args.get(k)
        if isinstance(v, str) and sub.type in (object, array): v = try_json(v)
        if sub.type in SCALARS: v = coercer[sub.type](v) or v
    return args
```

### 4.3 我们的 Go 对应

```go
func Coerce(t agent.Tool) agent.Tool
```
- 在 `Execute` 之前按 `Parameters()` 走一遍 `json.RawMessage` → `map[string]any` 的规范化，再序列化回去；失败（不是 object）原样透传，让工具自己报错。
- 与 `Validate` 的顺序：**Coerce 先，Validate 后**（Hermes 的 `validate_deferred_call_args` 也是「先 probe required → 再 coerce → 最后 jsonschema」，见 §5.1）。

### 4.4 差异与风险

- Go 的 `json.RawMessage` 没有类型信息，纠偏必须由 schema 驱动；schema 里 `type` 是数组（`["string","null"]`）时按第一个非 null 类型纠偏，并且在 `Sanitize` 之后执行（顺序反了会拿到数组 type 而无法决策）。
- 风险：`"null"`/`"~"`/`""` 三种「空」的语义不同——Hermes 的做法是只在类型明确时不抛错，其余留给校验；我们沿用，**不要**把 `""` 纠成 0。
- 风险：整数溢出与浮点精度（`json.Unmarshal` 到 `float64`），纠偏 number 时用 `json.Number` 而不是 float64，避免大整数被改写。

## 5. 预校验

### 5.1 Hermes 做法

分两处，职责不同：

1. **循环层批次校验** `agent/turn_tool_validation.py:validate_tool_calls`：对模型返回的 `tool_calls` 做「结构修复 + 局部拒绝」。要点：id 去重、名字修复（大小写/下划线变体）、`arguments` 为 dict 或 `""` 时规范成 JSON 字符串；**只有当一个批次里没有任何合法调用时**，才对连续非法工具名走「三振」退出（第 3 次直接结束本轮，而不是无限喂错误）；参数 JSON 坏 → 先给一次重试机会，仍坏才产出 `is_error=true` 的 tool 结果（`_json_error_result`），且**必须**为每个调用保留结果，否则角色交替被破坏。
2. **延迟工具的参数校验** `tools/tool_search_validation.py:validate_deferred_call_args`：模型通过 `tool_call` 桥调 deferred 工具时看不到 schema，容易漏必填。做法是「required 字段探针 → 跑一遍和正常分发相同的 schema 纠偏 → 用 jsonschema 校验修好的副本」，错误里带 `path`/`constraint`/`parameters` 和一句 `hint`；schema 缺失/无校验器/含外部 `$ref` 一律**fail open**（不能因为校验器问题把调用卡死）。

### 5.2 关键算法（简化）

```
def validate(calls):
    fix ids/names/args-shape
    valid = [c for c in calls if known(c.name)]
    if not valid:
        if strikes[name] >= 3: return partial exit
        return error_results(calls)          # 每个调用都有 result
    for c in invalid: error_results.append(...)
    return valid
```

### 5.3 我们的 Go 对应

```go
func Validate(t agent.Tool) agent.Tool  // 按 schema 预校验 required/type/enum；失败返回可修复文案（is_error=true）
```
- 文案契约（契约 §3.1）：说清**哪个字段、期望什么、收到什么、怎么改**，无堆栈。这条比「实现校验」更重要——模型是唯一读者。
- 循环层不做三振（`loop.go` 目前对未知工具直接返回 `TOOL_UNKNOWN` 结果并继续，第 371–379 行），保持简单：坏参数 → 可修复错误 → 模型自己重试。

### 5.4 差异与风险

- Hermes 的「三振」是为了挡住弱模型对不存在工具的执念；我们用「可用工具清单」文案（`availableToolNames`，`internal/agent/tool.go:192`）已达到类似效果，**本轮不引入进程级计数**（计数挂哪都是状态，得不偿失）。
- 风险：`Validate` 之后工具内部还会再 unmarshal 一次，两套错误文案可能不一致；约定「工具内部错误只覆盖 IO/业务错，schema 层错误由 Validate 统一出口」。
- 风险：校验不能拒绝「schema 没写但工具接受」的额外字段（Go 的 `json.Unmarshal` 默认忽略），否则会误伤现有多传参的调用。

## 6. 大结果落盘与截断

### 6.1 Hermes 做法

四个文件分工：

| 文件 | 职责 | 关键常量 |
|---|---|---|
| `tools/budget_config.py` | 决策「多大算大」 | `DEFAULT_RESULT_SIZE_CHARS=100_000`、`DEFAULT_TURN_BUDGET_CHARS=200_000`、`DEFAULT_PREVIEW_SIZE_CHARS=1_500`、`DEFAULT_MCP_RESULT_SIZE_CHARS=50_000`；`BudgetConfig.resolve_threshold` 优先级 pinned → overrides → `mcp_` 前缀 → registry → 默认；`budget_for_context_window` 用窗口的 15%/30% 并按 8K/16K 兜底；`PINNED_THRESHOLDS={"read_file": inf}` |
| `tools/tool_result_storage.py` | 落盘 + 预览 + 分页 | `PERSISTED_OUTPUT_TAG`、`STORAGE_DIR=<tmp>/hermes-results`、`generate_preview`（按行对齐、默认 1500 字符）、`maybe_persist_tool_result`、`enforce_turn_budget`（从最大开始逐个落盘直到总量低于预算） |
| `tools/tool_output_truncate.py` | 纯文本头尾截断 | `HEAD_RATIO=0.4`（留头 40%、尾 60%），`truncation_notice` 形如 `... [LABEL TRUNCATED - N unit omitted out of T total] ...` |
| `tools/hook_output_spill.py` | 钩子/插件输出的旁路落盘 | `DEFAULT_MAX_CHARS=10_000`、头尾预览各 500；I/O 失败仍返回预览，不抛 |

- `maybe_persist_tool_result` 的行为：超阈值 → 写文件（`_write_to_sandbox` 用 stdin 写入并 `wc -c` 校验字节数）→ 返回 `<persisted-output>` 包裹的「预览 + 文件路径 + 用 read_file 分页读」；`_pageable_text` 会把 MCP 的 JSON 信封拆开，让分页读到正文而不是 envelope。
- 为什么 `read_file` 阈值是 `inf`：**防止「读大文件 → 落盘 → 再读落盘文件」的无限套娃**（源码里就这一行常量 + 注释）。

### 6.2 关键算法（简化）

```
def maybe_persist(name, text):
    limit = threshold(name)                    # 默认 100K，read_file 无限
    if len(text) <= limit: return text
    path = write(tmp, text)                    # 内容哈希去重
    return f"<persisted-output>\n{preview(text, 1500)}\n→ {path}\n</persisted-output>"
```

### 6.3 我们的 Go 对应

```go
type SpillPolicy struct {
    Dir       string // 空 → 默认数据目录/spill
    MaxBytes  int    // 默认 32KB
    HeadRunes int    // 默认 2000
}
func (p SpillPolicy) Wrap(t agent.Tool) agent.Tool // 超限写盘，Content 换成"头部+落盘路径+继续读方法"
func ReadResultTool(p SpillPolicy) agent.Tool      // read_tool_result(file_path, offset?, limit?)
```
- 现状：`internal/agent/guard.go:34 toolOutputCapDefault = 16000`，在 `loop.go:396` 统一出口 `capToolOutput` 硬砍并附一句「已截断，请分段重试」（`guard.go:49-56`）。问题是**内容真丢了**，模型没有回读手段。
- 改造后：`capToolOutput` 保留为最后一道闸（防超大字符串进内存），`SpillPolicy.Wrap` 在工具层先判断落盘；`AGENT_TOOL_SPILL_BYTES=0` 时 `Wrap` 必须直接返回原工具（契约 §1.1 字节等价）。
- 落盘要求（契约 §3.2）：目录 0700、文件不可执行、文件名含 session 与内容哈希、同内容复用；`read_tool_result` 的路径必须限制在 spill 目录内（复用 `agent.Exec.Root` 的沙箱判定思路，别让它变成任意文件读取）。

### 6.4 差异与风险

| 点 | Hermes | 我们 | 风险 |
|---|---|---|---|
| 阈值来源 | 每工具可配 + 上下文窗口自适应 | 单一 32KB | 小模型窗口下 32KB 仍偏大；后续可加窗口系数 |
| 每轮总预算 | 有（200K，按大小淘汰） | 无 | 单轮 6 个 30KB 结果仍可能撑爆；先观察再补 |
| 预览 | 头尾各 500 + 1500 字符 | 头 2000 rune | 尾部信息丢失（日志类结果尾部最有用）；建议改为头 60%/尾 40% |
| 循环防护 | `read_file` 阈值 pin 成 inf | 需要显式：spill 目录内的文件再 `read_tool_result` 不能再落盘 | 否则套娃；把 spill 目录里的读结果标记 `Spilled=false` 不参与落盘判定 |
| 字节校验 | 写后用 `wc -c` 核对 | Go `os.WriteFile` 返回值即字节数 | 可直接用返回值，无需二次校验 |

## 7. 并行执行

### 7.1 Hermes 做法

- 准入是**白名单**，不是风险枚举：`agent/tool_dispatch_helpers.py:_NEVER_PARALLEL_TOOLS`（clarify/manage_connections/manage_catalog/set_theme 等）一律屏障；`_PARALLEL_SAFE_TOOLS`（read_file/search_files/session_search/skill_view/web_search/web_extract/...）直接放行；MCP 工具要服务端声明 `supports_parallel_tool_calls: true`（`_is_mcp_tool_parallel_safe`）；`tool_search`/`tool_describe` 因为是无状态目录读取，单独列在 `_PARALLEL_SAFE_BRIDGE_LOOKUPS`。**其余一律串行**（`_batch_admission` 返回 None = barrier，含参数不是 object/JSON 坏的情况）。
- 路径冲突：只有 `_PATH_SCOPED_TOOLS`（`read_file`/`search_files`/`write_file`/`patch`）参与路径预留。`_plan_tool_batch_segments` 把批次切成有序段：`reserved_paths=[(canonical_path, is_writer)]`，新调用的路径只要与已预留的路径重叠且**任一方是写**就关段，新调用在下一次「run」里开始；读读重叠保持并行；单段少于 2 个调用降级为串行（串行路径有更完整的内联处理）；相邻串行段合并。最终**调用顺序严格保持**，后一个调用不会跨过前一个屏障。
- 执行：`agent/tool_executor.py:execute_tool_calls_segmented`（段级）+ `execute_tool_calls_concurrent`（段内），`_MAX_TOOL_WORKERS=8`；并发超时 `_DEFAULT_CONCURRENT_TOOL_TIMEOUT_S=420.0`；审批提示必须串行（`_ConcurrentToolAuthorizationGate`，锁超时 360s），否则多个确认弹窗会互相打架；`_START_ORDER_GATE_TIMEOUT_S=120.0` 保证结果按输入顺序返回。
- 桥接解包：`_peel_bridge_call` 把 `tool_call` 桥的解包成真实工具名再判定准入，否则一旦 tool_search 激活，所有 deferred 工具都变成「叫 tool_call」而丢失并发；纯 connector 批量才算并行安全，混入本地调用就退回屏障（避免绕过路径预留）。
- 另一种批量：`agent/tool_call_batches.py:expand_local_tool_batches` 把 `tool_call` 桥的批量展开成独立调用，交给正常持久化/执行；connector-only 批量保留给 dispatcher。

### 7.2 关键算法（简化）

```
def plan(calls):
    segs, cur, reserved = [], [], []
    for c in calls:
        adm = admission(c)                  # None = barrier
        if adm is None: close(cur); segs += [("seq", [c])]; continue
        if conflict(adm.paths, reserved): close(cur)   # 写与任何重叠 → 关段
        reserved += adm.paths; cur += [c]
    close(cur)
    return segs                             # 段内并发、段间串行，整体保序
```

### 7.3 我们的 Go 对应

```go
func Plan(calls []llm.ToolCall, riskOf func(name string) (Risk, bool)) [][]int
func RunBatch(ctx context.Context, ex *agent.Exec, reg *agent.Registry, calls []llm.ToolCall, maxParallel int) []agent.Result
```
- 红线（契约 §3.3）：写/执行/网络工具**永不**进同一批；并发上限默认 4；`ctx` 取消后不再启动新调用；结果按输入顺序返回。
- 接入：`loop.go:230` 的 `for _, call := range calls` 逐条执行改为「`Plan` → 段内 `RunBatch` → 按序回灌」；每段结束后再取下一段，事件顺序（`EvToolCall`/`EvToolResult`）保持与输入一致。

### 7.4 差异与风险

| 点 | Hermes | 我们 | 风险 |
|---|---|---|---|
| 准入模型 | 白名单 + 显式风险工具 | `Risk` 枚举（黑名单式） | **反了**：新工具忘了标 Risk 就会被当成可并行。对策：`riskOf` 未知工具返回 `(RiskExec,false)`，`Plan` 只并发 `RiskRead` |
| 路径冲突 | 有（canonical path + 读写区分） | 无 | 两个写工具并发改同一文件 → 内容交错/丢更新；本轮靠「写工具不并发」兜住，跨工具（read 与 write 不同文件）仍安全 |
| 审批串行 | 有专门 gate | 无（确认走前端） | 若一次返回多个需确认调用，前端会连弹；`Plan` 本轮把 non-read 全串行，天然规避 |
| worker 数 | 8 | 4 | 够用；`AGENT_TOOL_PARALLEL=off` 时完全串行（默认） |
| 结果顺序 | 按输入 | 按输入 | 必须显式测：并发完成顺序 ≠ 回灌顺序 |

## 8. 审批分级（**方向不同**，只借概念）

### 8.1 Hermes 做法

- Hermes **没有**「每个工具一个风险等级」的枚举（在 `tools/` 与 `agent/` 中未找到工具级 Risk 表）。审批是**命令/载荷内容级**：
  - `tools/approval.py:check_all_command_guards` / `check_execute_code_guard` / `request_tool_approval` 是三个入口；`check_dangerous_command` 做模式匹配。
  - 模式：`tools/approval_context.py:_VALID_MODES=("manual","smart","off")`，默认 `manual`（`_get_approval_mode`），`smart` 会用 guardian LLM 判断；`off` 相当于全放行。
  - **yolo 之上还有 floors**：`tools/approval_floors.py:_match_user_deny_rule`（fnmatch 用户 deny 规则，**即使 yolo 也拦**）、`_command_matches_permanent_allowlist`、`tools/approval.py:_floor_block`（例如 `agent/runtime_self_protection.py:command_deletes_runtime` 防止删掉自己运行的 Python 解释器）。
  - 会话级状态：`_session_approved` / `_session_yolo` / `_permanent_approved`；拒绝熔断默认 3 次（`_denial_tally` 系列）。
  - 无人值守场景有独立的二值模式：`cron_mode`、`single_query_mode`、`unattended_mode`，默认 `deny`（`_binary_approval_mode`）。

### 8.2 我们的现状与契约

- 现状是**工具名级**：`internal/web/agent_confirm.go:41 riskyTool(name)` 给出一组风险工具名；`internal/web/agent_tool_policy.go:93-99` 在 confirm 档给这些工具包一层 `confirmTool`，`wrapOutsideWorkspace` 另外处理「工作区外的绝对路径」。
- 契约 §3 引入 `Risk`（read/write/exec/network/self）不是为了做审批，而是为了 `Plan` 判断「能不能并发 + 要不要串行」；审批仍走现有 confirm 壳。

### 8.3 差异与风险

- **不要照搬 Hermes 的 yolo/floors**：Hermes 的 floors 是为「模型能自由跑 shell」设计的，Moon 的 `run_command` 有白名单 cwd，风险面不同。
- 真正值得借的一条：**deny 规则必须比「用户点过确认」优先级更高**。Moon 现在 `confirmed=true` 时 `internal/web/agent.go:594` 的 `!confirmed` 判断不再拦该次风险调用，一旦前端把 confirmed 状态用错粒度就会越权。
- 风险：`Risk` 标错直接导致并发越权（见 §7.4）。要求：`Risk` 与 `riskyTool` 两份名单必须由同一处生成（`toolsys` 导出 `RiskOf(name)`，web 层复用），禁止两处维护。

## 9. 包/函数映射总表

| 契约 §3 条目 | Hermes 参照 | Go 落点 |
|---|---|---|
| `Toolset` / `Entry` / `Catalog` | `toolsets.py:TOOLSETS`、`tools/tool_search_catalog.py:CatalogEntry` | `internal/agent/toolsys/catalog.go` |
| `Catalog.Defs`（稳定序） | `model_tools.py:get_tool_definitions` | `catalog.go`，顺序 = 激活序 + 声明序 |
| `Catalog.Search` | `tool_search_catalog.py:search_catalog` | `catalog.go:Search` |
| `tool_search` 工具 | `tools/tool_search.py:dispatch_tool_search` | `toolsys/searchtool.go` |
| `Sanitize` | `tools/schema_sanitizer.py:sanitize_tool_schemas` | `toolsys/schema.go` |
| `Coerce` | `tools/arg_coercion.py:coerce_tool_args` | `toolsys/coerce.go` |
| `Validate` | `agent/turn_tool_validation.py:validate_tool_calls` + `tools/tool_search_validation.py` | `toolsys/validate.go` |
| `SpillPolicy.Wrap` / `ReadResultTool` | `tools/tool_result_storage.py:maybe_persist_tool_result` | `toolsys/spill.go` |
| `Plan` / `RunBatch` | `agent/tool_dispatch_helpers.py:_plan_tool_batch_segments`、`agent/tool_executor.py:execute_tool_calls_segmented` | `toolsys/parallel.go` |
| 装配/开关 | `model_tools.py` + `hermes_cli/config_defaults.py` | `internal/web/agent_tool_policy.go`、`internal/agent/loop.go` |

## 10. 本轮不做

1. `tool_describe` / `tool_call` 第二、三桥接工具（没有 MCP/远程目录，收益不成立）。
2. BM25、Snowball 词干化、query 覆盖率门控。
3. listing 的 full→names→groups 三级降级与 token 预算。
4. 并行按路径重叠切段（读读并行、写冲突关段）——本轮只按 `Risk` 分批。
5. per-turn 结果总预算（200K）与「逐个大结果淘汰」。
6. schema 属性重命名 + 反解（`sanitize_property_key`/`unrename_tool_args`）。
7. guardian LLM（`smart` 档）、拒绝熔断、yolo 之上的 floors。
8. toolset 的平台裁剪（`CLIENT_SURFACE_TOOLSETS`）与 `toolset_distributions` 概率采样。

## 11. 后续可做

1. `Plan` 增加路径冲突：`Risk` 之外再给 `PathScope`（`Entry` 加 `Paths func(args) []string`），直接映射 `_plan_tool_batch_segments` 的规则。
2. `tool_describe`：一旦接 MCP 或工具数过百，必须补「先看 schema 再调」，否则盲调失败会抵消渐进披露收益。
3. per-turn 预算：按 `AGENT_TOOL_SPILL_BYTES` 上线后的真实数据决定是否引入。
4. 审批内容级检测：给 exec 类工具加危险模式 deny（不可被 `confirmed` 绕过），向 `approval_floors` 靠拢。
5. 弱模型专项：把「未知工具三振」「坏 JSON 重试一次」搬进循环层，前提是有真实失败样本。
