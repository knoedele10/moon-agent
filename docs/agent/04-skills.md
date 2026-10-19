# 技能：渐进式披露与自动检索（04-skills）

> **依据**：实读代码 `model/skill.go`、`agent/skill/parse.go`、`agent/skill/scan.go`、`web/skill.go`、
> `web/skill_import.go`、`web/skill_retrieve.go`，以及 `web/agent.go` 里 `runAgentChat` 的技能注入几行。
> 采样时间 **2026-10-12（Asia/Shanghai）**。本文的路径、字段名、默认值以代码为准；
> 接口的权威定义见 [../api-contract.md](../api-contract.md) 第 13 节，两个给模型用的工具见 [02-tools.md](02-tools.md) 第 13.7 节。

---

## 1. 技能是什么

技能是一段**写给模型的"怎么做某件事"的说明书**：模型读完之后按它描述的流程执行。
它**不是可执行代码**——服务端不解析、也不执行技能正文里的任何指令，正文对服务端来说就是一坨文本，
真正"执行"的是读到它之后的模型。

一条技能由这些字段组成（表 `agent_skill`，见 `model/skill.go`）：

| 字段 | 说明 |
|---|---|
| `name` | 技能名，**模型找技能的钥匙**，在同一个用户内唯一（`(user_id, name)` 唯一索引） |
| `description` | 一句话简介，进提示词清单、参与检索 |
| `body` | Markdown 正文，可能很长（几千到几万字） |
| `enabled` | 开关；关掉的技能不进提示词清单、不参与自动检索，`skill_read` 也会拒绝读 |
| `source` | 从哪个文件导入的，便于溯源；手工新建时可为空 |
| `embedding` / `embed_dim` | `name：简介` 的向量（JSON float32），语义召回用；**惰性补算**，没算过就是空 |

**为什么存数据库、按用户，而不是直接读文件系统**（`model/skill.go:15-21` 的设计说明）：

1. 桌面应用要能在界面上开关技能，不该让用户去翻目录；
2. 技能是**按用户**的，不同账号可以用不同的技能集；
3. 列技能、检索技能不该每次都递归扫磁盘（技能库可能上千个文件）。

技能正文里的内容只影响"模型怎么做"；技能库里的文件本身不会被本服务执行。

---

## 2. 渐进式披露：提示词只放清单，正文按需取

`skillPromptBlock`（`web/skill.go:208`）拼系统提示词里的技能清单，规则实测如下：

- 只取当前用户 `enabled = true` 的技能，按 `name asc` 排列。
- 前 **30** 条（`skillPromptLimit`）**带简介**，简介截断到 **160** 字。
- 第 31 条起**只列名字**，用「、」连接，最多 **400** 个名字（`skillNameLimit`）；
  名字也超了会再补一句「另有 N 个技能连名字都没列出，用 skill_search 按关键词找」。
- 末尾给一行「（当前共 N 个已启用技能）」。
- SQL 只 `Select("id, name, description")`：正文是 `longtext`，单条可达几十 KB，
  **不能为了拼提示词把正文读进内存**。

**为什么不能把全文塞进提示词**：本机技能库盘上有 **579 个 `SKILL.md`、约 28 MB**（含隐藏目录里的同步副本，默认扫描收下 111 个）
（`C:\Users\woshiss\.agents\skills`）。全量正文进系统提示词是几十万 token 量级，直接撑爆上下文，
而且其中绝大多数与当前这句话毫无关系。只列名字很小（几 KB），
模型知道"有这么个技能"就够，需要细节时自己去取——这就是**渐进式披露**。

模型侧有两个只读工具（`web/skill.go`，所有档位都注册，plan 档也给）：

| 工具 | 参数 | 行为 |
|---|---|---|
| `skill_search` | `query`（必填）、`limit`（默认 8，最大 20） | 在**已启用**技能的 `name` / `description` / `body` 上做 LIKE，按 `name` 升序返回「名字：简介」（简介截 200 字）；没命中会明说"没有匹配的已启用技能"。 |
| `skill_read` | `name`（必填，精确匹配） | 返回简介 + 正文；正文超过 **6000** 字（`skillBodyMaxRunes`）截断并附提示。技能不存在或已关闭都会报错。 |

提示词的「行为准则」第 8 条把用法写死了：要干专门的事（写文档/做设计/处理表格/写代码等）时，
**先 `skill_search` 找、再 `skill_read` 读、然后按技能执行**。

边界：`skill_search` 是 SQL LIKE，对中文子串召回很弱（没有分词）；
真正做中文召回的是第 3 节的自动检索。

---

## 3. 自动检索链路

入口是 `autoSkillContext`（`web/skill_retrieve.go:408`），在 `runAgentChat` 拼系统提示词时调用
（`web/agent.go:321`）。它**只在 `tools=true` 走 agent 循环的路径上生效**；
不带 `tools` 的老路径既不做技能检索，也不注入技能清单。

链路四步：

**第一步 理解任务**（`skillUnderstandQuery`，开关 `AGENT_SKILL_UNDERSTAND`，默认开）：
把用户这一句话交给同一个模型，要求输出 3~8 个**中英混合**的检索关键词（Temperature 0、MaxTokens 80），
再用「原句 + 关键词」去召回。

**为什么需要这一步**：技能名与简介大多是英文，用户却常用中文提问（"解释一下这段报错"）。
没有本地嵌入模型时词面召回跨不了语言，这句话在库里几乎什么都命中不了。
实测：不加这一步，`解释一下这段报错` 召回的前 5 名里一个调试类技能都没有；
加上之后检索串变成 `解释一下这段报错 报错 错误 异常 error exception 日志 log 堆栈 stack trace 排查 debug`，
`investigate` 这类技能才浮上来。
改写失败（模型报错、输出为空）一律退回原句——这一步只影响召回质量，绝不影响能不能聊天。

**第二步 混合召回 top 20**（`skillRecallTopK`）：
`loadEnabledSkills` 一次取该用户**全部已启用**技能，在内存里逐条打分——
不做 SQL 预筛，是因为技能是几百条量级，而 MySQL 的 LIKE 对中文二元组召回很弱。

每条技能算三路信号：

- **词面**（分两路，`rankSkills`）：`rag.LexicalScore(query, name + " " + description)` 与
  `rag.LexicalScore(query, body) * 0.6` 取较大者。查询词在文本里的覆盖率（BM25 简化版；
  中文按字符二元组切，超长文本 ×0.85 惩罚）。
  **正文一路要打六折**：技能正文动辄几万字，单看正文覆盖率会让"顺带提过这个词"的长技能
  压过真正对口的短技能（本机有个技能正文 9 万字，随便什么查询都能在它里面找到词）。
- **实体** `rag.EntityBoost(rag.QueryEntities(query), rag.QueryEntities(name + " " + description))`：
  从**名字与简介**里抽实体（引号内容、含数字串、≥3 字母英文词、2~6 字中文串）。
  正文里的实体太杂，会把噪声当成"精确命中"，所以不进实体这一路。
- **语义**：技能向量与查询向量的余弦。向量是 `name：简介（截 500 字）` 的 bge-m3 嵌入；
  **正文不进向量**——正文太长，嵌入既慢又会把简介的信号稀释掉。

三路按**自适应分母**融合（`fuseSkillScores`）：

```
score = (lexical + semantic + entity) / maxPossible
maxPossible = 1.0（词面恒在） + 1.0（有语义时） + 0.5（有实体时）
```

这样缺一路信号不会把总分整体压低——与 `rag.FuseScores` 同思路，
但技能检索**不要求语义分**，必须在没开 Ollama 的机器上照常工作。

另外：查询里**直接包含技能名**（大小写不敏感，如"用 caveman 模式"）时，
`score = min(1, score + 0.5)`。用户的明确指令不该被模型的偏好覆盖。

排序：score 降序 → name 升序，取前 20。

**第三步 大模型重排 top 5**（`skillRerankTopN`）：
只有候选**多于 5 条**且重排开关打开时才做。把 query（截 1000 字）与候选清单
（`序号. 名字：简介`，简介截 200 字）交给模型，system prompt 要求**只输出 JSON**：

```json
{"picked":["技能名1","技能名2"]}
```

Temperature 0、MaxTokens 300。解析（`parsePickedNames`）容忍三种常见写法：
`{"picked":[...]}`、`{"skills":[...]}`、裸 JSON 数组，并剥掉围栏与前后解释。
**清单以外的名字一律忽略**（不让模型凭空造技能）；一个都没解析出来就报错并降级。

**第四步 注入本轮系统提示词**：`buildSkillBlock` 拼一段
「【本轮自动匹配到的技能】按相关度排序，请优先按它们的说明执行；不够用时再用 skill_search 找更多」，
每条给出：`--- 技能 i：名字 ---`、简介（截 300 字）、正文（**截断到 3000 字**，`skillInjectRunes`，
截断时附「要完整说明用 skill_read」）。5 条 × 3000 字上限，上下文可控。

**前端可见性**：有命中或降级时发 SSE `skills` 事件：

```
event: skills
data: {"picked":[{"id":3,"name":"officecli-pptx","description":"…","score":0.42,"lexical":0.5,"semantic":0.0,"entity":0.0,"name_hit":false}],"source":"llm","degrade":""}
```

`picked` 是 `skillHit` 数组（**不含正文**），`source` 为 `llm` / `hybrid`，`degrade` 是降级原因。
前端把它渲染成"本轮自动匹配技能（大模型重排 / 混合检索）"，降级原因也一并显示——
否则用户只看到模型忽然变得很专业，不知道是自动检索在起作用。

**与主动工具的分工**：自动检索解决"顺带想起"（用户没点名，模型也该用上相关技能）；
`skill_search` / `skill_read` 解决"主动翻找"（注入的 5 条不够用，或要读完整正文）。

---

## 4. 三级降级与环境变量

技能检索**任何一级坏了都不阻断对话**（与记忆注入同一个原则）：

1. **Ollama 没开 → 少一路语义信号**：`skillSemanticScores` 返回 `nil` + 降级原因，
   词面 + 实体照常工作，融合分母自动少 1.0。检索**仍然可用**，只是少了"意思相近但用词不同"的召回。
2. **重排模型不可用 → 退回混合排序**：注册表未初始化、本轮没有模型或密钥、调用失败、
   或输出里没有清单内的技能名，都会退回混合检索顺序（`source=hybrid`），降级原因写进 `degrade`。
3. **全失败 → 不注入、对话照常**：读库失败等会让 `autoSkillContext` 返回 error，
   `runAgentChat` 把它并进 `degrade`，不注入任何技能，对话正常继续。

**语义一路的细节**：

- 读 `OLLAMA_BASE_URL`（默认 `http://127.0.0.1:11434`）与 `KB_EMBED_MODEL`（默认 `bge-m3:latest`），
  与知识库**共用同一个嵌入模型**。
- 查询向量化超时 **3 秒**（`skillEmbedTimeout`）：本地 Ollama 没开时连接会被立刻拒绝；
  真在跑但很慢时，也不能让"检索技能"把整轮对话卡住。
- **惰性补算**：召回时谁还没有向量，就把它和 query 一起嵌入并落库（`SaveSkillEmbedding`），
  下次召回就能用上语义。所以**导入技能不依赖 Ollama**（没装也能导），
  语义能力"什么时候有就什么时候生效"。补算失败只忽略——这是优化，不是必需路径。
- 维度不一致的向量（换过嵌入模型）余弦直接返回 0，不让新旧向量算出一个假分数。

**三个环境变量**（`skillEnvBool`：空串取默认值；`1` / `true` / `on` / `yes` 为真；其它一律为假）：

| 变量 | 默认 | 作用 |
|---|---|---|
| `AGENT_SKILL_UNDERSTAND` | **true** | 控制第一步"理解任务"（查询改写）。关掉就直接用原句召回：中文提问 + 英文技能名时召回会明显变差，但少一次模型调用（省约 1 秒）。 |
| `AGENT_SKILL_AUTO` | **true** | 总开关。关掉后 `autoSkillContext` 直接返回空，**完全不做自动检索**。注意：提示词里的技能清单（`skillPromptBlock`）与 `skill_search` / `skill_read` 工具**不受它控制**，仍然可用。 |
| `AGENT_SKILL_RERANK` | **true** | 只控制大模型重排。关掉后退回混合排序（`source=hybrid`），不是不做检索。候选 ≤ 5 条时本来就不重排，这个开关不起作用。 |

---

## 5. 导入：一个目录里的 SKILL.md 怎么进来

接口与失败文案见 [../api-contract.md](../api-contract.md) 第 13.6 节。这里讲扫描与幂等语义。

**`skill.ScanDir` 的规则**（`agent/skill/scan.go`）：

- 递归找名为 `SKILL.md` 的文件（大小写不敏感）。
- 目录名 `node_modules` 一律跳过；`include_hidden=false`（默认）时跳过所有以 `.` 开头的目录。
  技能库里 `.gbrain` / `.openclaw` 这类目录是工具自己同步出来的副本，扫进去只会用旧副本覆盖同名技能。
- **同名技能只保留路径层级最浅的一份**（并列时取路径字典序小的），其余记进 `duplicates`；
  名字比较大小写不敏感。
- 技能名的优先级：frontmatter `name:` → 第一个 markdown 标题 → 第一个不像 `key: value` 的非空行
  → 文件所在目录名（保证名字不会是空的）。

**解析**（`agent/skill/parse.go`）：只认最简 frontmatter（`---` 包起来的 `name:` / `description:`），
不引 YAML 解析器；但正确处理 **YAML 块标量** `>` / `|`（gstack 那批技能写的是 `description: >`，
早期版本把它显示成一个竖线）。名字截 120 字、简介截 1000 字。

**幂等**：`UpsertSkill` 按 `(user_id, name)` 先查后写。同一条技能再次导入走"更新"，
只刷新 `description` / `body`（`source` 非空才更新），不产生重复行；
统计上体现为 `updated` 而不是 `created`。

**`enabled` 是三态**（`UpsertSkill(userID, item, *bool)`）：

| 请求 | 新建的技能 | 已有的同名技能 |
|---|---|---|
| 不传 `enabled` | **启用**（导入的意图就是"能用"） | **保持用户当前开关** |
| `enabled: true` | 启用 | 强制启用 |
| `enabled: false` | 停用 | 强制停用 |

不传时"保留已有开关"是刻意的：重跑导入（技能库更新后刷一遍）不该把用户手工关掉的技能又打开。
响应里的 `preserve_existing` 说明本次到底动没动已有开关。
命令行导入器同理：默认保留，`-enable` / `-disable` 才强制覆盖。

**实测数字**（`C:\Users\woshiss\.agents\skills`，2026-10-12，按 `ScanDir` 规则复算）：

| 场景 | `files` | `imported` | `duplicates` |
|---|---|---|---|
| 默认（`include_hidden=false`） | 111 | 110 | 1 |
| `include_hidden=true` | 579 | 110 | 469 |

盘上确实有 **579** 个 `SKILL.md`（约 28 MB），但那把隐藏目录里的同步副本也算进去了；
默认扫描只看得到 111 个文件，去重后导入 110 条。`files` 统计的是**扫描阶段遇到的文件数**，
`imported` 才是真正写库的技能数——两个数分开报，就是为了让用户看懂这个差。

单条技能写库失败不会中断整批：记进 `errors` 继续下一条。只有扫描阶段就失败（目录不存在等）才整体报错。

---

## 6. 能力边界与未覆盖项

- **语义一路依赖本机 Ollama + bge-m3**（`KB_EMBED_MODEL`，默认 `bge-m3:latest`）。
  未启动时语义分**恒为 0**，`recall` 响应里的 `semantic` 全是 0，只剩词面 + 实体。
- **向量是惰性补算的**：第一次召回才把缺的向量写库（`agent_skill.embedding` 在此之前为空）。
  首次召回可能略慢，因为要顺带嵌入所有还没向量的技能。
- **重排依赖大模型输出 JSON**：解析失败就降级成混合排序，不会报错给用户，只在 `degrade` 里说明。
- **没有技能的版本管理 / 冲突合并**：同名就是覆盖，看不到历史版本；
  两个来源的同名技能也无法并存（`ScanDir` 只保留层级最浅的一份，导入是 last-write-wins）。
- **超长正文读不全**：`skill_read` 只返回前 6000 字，工具**没有 offset / 分页参数**，
  超过 6000 字的正文后半部分模型拿不到；自动注入又只截 3000 字。
  写技能时应把关键流程放在前部。
- **`skill_search` 无分词**：SQL LIKE 对中文子串召回弱；只有自动检索的词面分做了二元组切分。
- **`POST /api/skills/recall` 没有前端入口**：`skills.js` 只用了列表 / 详情 / 新建 / 改 / 删 / 导入，
  检索预演目前只能手调接口，或在对话里看 SSE 的 `skills` 事件。
- **`AGENT_SKILL_AUTO` 关掉不等于看不到技能**：提示词里的技能清单与两个工具不受该开关控制。
- **召回质量只做了抽查，不是评测**：在真实模型下试了 6 条典型中文任务
  （中文润色 → `humanizer-zh`；Word → `officecli-docx`；PPT → `officecli-pptx`；
  Excel 报表 → `officecli-xlsx`；审代码 → `review`；报错 → `investigate`），结果都对；
  但没有标注数据集、没有算命中率，换一批说法就可能翻车。
  可复跑的脚本是 `scripts/verify-skills.ps1`（需服务在跑 + 可用模型 Key）。
- **每一步模型调用都是可选的**：理解任务与重排各是一次额外的模型调用（实测各约 1 秒，
  总计 1.4~1.8 秒）；任一失败都降级，聊天本身不受影响。
- **自动检索只在 `tools=true` 的 agent 路径上跑**；老路径（`tools` 省略 / false）
  不做技能检索、也不注入清单。

---

## 7. 参考

- [../api-contract.md](../api-contract.md) 第 13 节 —— `/api/skills` 全部接口的字段、状态码与失败文案
- [02-tools.md](02-tools.md) 第 13.7 节 —— `skill_search` / `skill_read` 的参数与返回
- [03-usage.md](03-usage.md) —— agent 的开启方式与安全边界
- [00-contract.md](00-contract.md) —— agent 接口契约 v1（冻结）
