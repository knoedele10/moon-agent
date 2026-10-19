# task-8 验收报告：RAG 端到端**独立**复验

- 任务：task-8「独立复验 RAG 端到端（不复用实现者脚本作为唯一证据）」
- 日期：2026-10-11
- 复验人：verifier（独立于 rag-eng）
- 被判对象：/api/kb/* 多模态 RAG（bge-m3 嵌入 + qwen2.5vl 图片理解 + MySQL 存向量）
- **独立性声明**：本报告全部结论来自**我自己写的探测脚本**与**直接的 SQL / Ollama 查询**，
  没有把 scripts/verify-rag.mjs 的输出当作证据（它只作为「实现者自述」被引用）。
  实现者的脚本把上传超时从 20s 放宽到 300s —— 这正是本报告要独立测量的点之一。

## 一、结论（先给结论）

| # | 检查项 | 结论 | 证据位置 |
| --- | --- | --- | --- |
| 1 | 未登录访问 /api/kb/* | **PASS** 401 | probe A1/A2 |
| 2 | 注册/登录拿 session | **PASS** | probe B1/B2 |
| 3 | 文本入库 + sha256 去重 | **PASS** 201 / 200 duplicated | probe C1/C2 |
| 4 | **检索是真的语义命中（带诱饵文档）** | **PASS** 目标 Top1，优势 +0.20 | rag-semantic-01 |
| 5 | 相关 vs 无关提问的分数区分度 | **PASS** 0.6124 vs 0.2754 | rag-semantic-01 |
| 6 | 列表/详情/统计字段与契约一致 | **PASS** 不返回 content/embedding | probe D1–D10 |
| 7 | 图片上传走视觉链路 | **PASS** 201，块 kind=image_caption | probe E1/E4/E5 |
| 8 | **入库确实用的是 qwen2.5vl:7b** | **PASS**（三条独立证据） | rag-vision-contrast-01 |
| 9 | 图片块可被检索命中 | **PASS** rank 1，score 0.68 | probe E6 |
| 10 | /api/kb/ask SSE 事件序列 | **PASS** sources → delta×21 → done | probe F2–F7 |
| 11 | **删除文档后块被清干净（SQL 级）** | **PASS** 文档 0 行、块 0 行、孤儿块 0 | rag-db-check-02 |
| 12 | 删除后检索不再命中 | **PASS** 0 命中 | rag-db-check |
| 13 | 库内不变量（dim/字节数/chunk_count/sha256） | **PASS** 全为 0 例外 | rag-db-check |
| 14 | 边界与错误路径（9 类） | **PASS** | probe G1–G6 |
| 15 | 图片上传耗时 | **WARN** 25.7s / 27.2s（超 20s，不超 300s） | probe E2 |
| 16 | kb_document.meta 是否记模型名 | **WARN** 只记 indexed_by: vision | SQL 取证 |
| 17 | 「附带原图给多模态生成模型」 | **待验证** 黑盒无法区分 | 见第六节 |

**总体判定：核心链路 PASS，无 FAIL。** 三条非 PASS 项分别是「验收脚本为何必须放宽超时」
「meta 可审计性」「多模态生成未证实」的真实缺口，如实列出，不做淡化。

## 二、复现方式与原始证据文件

~~~powershell
# 1) 确认服务（本机 8080 实测可绑，但按协作约定仍用 8300）
Invoke-WebRequest http://127.0.0.1:8300/api/health -UseBasicParsing
# 不在跑就先起：go build -o $env:TEMP\verify-blogmy.exe . ; $env:APP_PORT=8300; & $env:TEMP\verify-blogmy.exe

# 2) 主探测（43 项断言）
node docs/verify/rag-probe.mjs --base http://127.0.0.1:8300
# 3) 带诱饵的语义排序（关键项）
node docs/verify/rag-semantic.mjs --base http://127.0.0.1:8300
# 4) 库内一致性 + 级联删除 + 跨语言语义 + 图片 meta（SQL 取证）
powershell -NoProfile -File docs/verify/rag-db-check.ps1 -Base http://127.0.0.1:8300
# 5) 视觉模型身份的因果取证（对照实验 + Ollama /api/ps）
node docs/verify/rag-vision.mjs --base http://127.0.0.1:8300 --keep
node docs/verify/rag-vision-contrast.mjs --good http://127.0.0.1:8300 --bad http://127.0.0.1:8301
~~~

原始输出（全部 UTF-8）：docs/verify/tmp/ 下的
rag-probe-01.log、rag-semantic-01.log、rag-db-check-01.log、rag-db-check-02.log、
rag-vision-01.log、rag-vision-02.log、rag-vision-contrast-01.log、probe-VRF-*.json（机器可读）。

主探测汇总（原文）：

~~~
[INFO] Z3-summary :: PASS=43 FAIL=0 WARN=1
[WARN] E2-image-upload-latency :: 图片上传耗时 25659ms，超过 20s
~~~

## 三、关键项 1：检索是否**真的语义命中**（不是关键词匹配、不是「随便返回一条」）

方法与判据（先写死在脚本里再跑）：库里放 **1 篇目标文档 + 5 篇同语言诱饵**，
目标文档**故意不含**查询里的关键词。

- 目标文档（月季冬剪）：「藤本月季在冬末还处于休眠期的时候动剪刀最合适……下刀的位置要挑朝外的芽上方……」
  ——**不含**「玫瑰」「季节」「修剪」这三个查询实词。
- 诱饵：DNS 排障、炖牛肉、轮胎保养、个税申报、疫苗时间表。
- 查询 Q1：「玫瑰应该在什么季节修剪？」

实测（原文）：

~~~
查询「玫瑰应该在什么季节修剪？」 命中 6 条：
  #1 doc28 seq0 score=0.6124 text="藤本月季在冬末还处于休眠期的时候动剪刀最合适。先去掉枯死"
  #2 doc31 seq0 score=0.4115 text="夏季胎压不要打得太高，跑高速前检查花纹深度与是否有鼓包。"
  #3 doc33 seq0 score=0.2976 text="新生儿出生后按免疫规划接种乙肝疫苗与卡介苗，后续按月龄补"
  #4 doc32 seq0 score=0.2940 text="专项附加扣除包括子女教育、继续教育、住房贷款利息与赡养老"
  #5 doc30 seq0 score=0.2618 text="牛腩冷水下锅焯去血沫，加姜片料酒小火炖两小时，起锅前十分"
  #6 doc29 seq0 score=0.2618 text="浏览器打不开网页时先看本机的域名解析服务是否可达，再检查"
[PASS] 语义命中-月季冬剪：预期文档排第 1（score=0.6124），相对其它文档的最大优势 = 0.2009

查询「怎么让电脑重新连上网？」 命中 6 条：
  #1 doc29 seq0 score=0.6203 text="浏览器打不开网页时先看本机的域名解析服务是否可达，再检查"
[PASS] 语义命中-DNS 排障：预期文档排第 1（score=0.6203），相对其它文档的最大优势 = 0.2273
无关提问最高分 = 0.2754 ；相关问题最高分 = 0.6124 ；差值 = 0.3370
[PASS] 相关与无关提问的分数有可观测差异（不是「一律高分」）
~~~

判定：**PASS**。目标文档在**零关键词重叠**前提下稳定排第一且领先 0.20；
换个问题同样命中各自正确文档；无关提问最高分（0.2754）显著低于相关问题（0.6124），
说明分数有判别力。

跨语言补测（中文提问 → 英文文档）：SEMANTIC_TOP1_OK score=0.2615。
**这条单独看是弱证据**（该步执行时库里只剩这一篇文档，排名无从比较），仅作旁证。

## 四、关键项 2：删库后块是否**真的清干净**（SQL 级，不信接口自述）

脚本 docs/verify/rag-db-check.ps1：入库长文本 → 直接查库 → 调 DELETE → 再直接查库。

删前（原文）：

~~~
id  kind  bytes  chunk_count  has_sha
25  text  3796   8            1
rows_in_kb_chunk  min_dim  max_dim  min_emb_bytes  max_emb_bytes
8                 1024     1024     4096           4096
seq  kind  dim   emb_bytes  text_chars
0..7 text  1024  4096       469 / 551 / 551 / 557 / 557 / 557 / 557 / 557
hits = 25/seq0/0.7413, 25/seq2/0.7367, 25/seq1/0.7212
~~~

删后（原文）：

~~~
delete body: {"deleted":true,"id":25,"ok":true}
document_rows_left          0
chunk_rows_left             0
orphan_chunks_global        0
bad_dim_global              0
bad_emb_bytes_global        0
chunk_count_mismatch_global 0
dup_sha256_global           0
hits pointing at the deleted document = 0
~~~

判定：**PASS**。文档行与**它的 8 个块行**都归零，全库无孤儿块（LEFT JOIN 反查 0），
删除后检索也拿不回任何一块。三条合起来才叫「级联删除真的生效」——
只验接口返回 deleted:true 是不够的。

## 五、关键项 3：图片入库到底走了哪个视觉模型（不能只靠耗时推断）

1. **配置对照实验（因果）**：把 KB_VISION_MODEL 指到不存在的模型，另一实例上传同一张图：

~~~
对照实例上传 -> HTTP 400（14ms）
响应：{"error":"图片理解失败（视觉模型不可用或超时）：视觉模型返回 404：{\"error\":\"model 'definitely-not-a-real-model:1b' not found\"}"}
[PASS] 对照实验：视觉链路确实用的是 config 里的模型（错误文案点名了伪造的模型名）
~~~

   证明：入库**真的读 KB_VISION_MODEL 配置**（不是硬编码），而 .env 里该项是 qwen2.5vl:7b。

2. **Ollama 服务端驻留事实（因果）**：先卸载视觉模型，再上传一张**全新字节**的图片：

~~~
上传前 /api/ps = ["bge-m3:latest"]
正常实例上传 -> HTTP 201（13615ms）id=35 kind=image
上传后 /api/ps = ["bge-m3:latest","qwen2.5vl:7b"]
[PASS] 因果证据：这次入库自己把 qwen2.5vl 拉起来了
~~~

   这是 Ollama 自己报告的驻留模型，不是我方推断；上传前为空、上传后出现，因果成立。

3. **同一张图直接问该模型**：POST /api/chat (qwen2.5vl:7b) 读回 "ZEBRA-7731\nBLACK WHITE STR"，
   与入库后的 image_caption 文本一致（【图片】zebra-verify.png / 描述里含 ZEBRA-7731 与 BLACK WHITE STR）。

4. 辅助：同一次上传耗时 25.7s / 27.2s / 13.6s，而同样大小的**文本**入库仅 206–298ms。

判定：**PASS**。第 1、2 条是独立因果证据，第 3、4 条是一致性佐证。

**WARN（可审计性缺口）**：kb_document.meta 里只有
{"desc_runes":98,"indexed_by":"vision","original_mime":"image/png","vision_prompt":"v1"} ——
indexed_by: vision 能证明走了视觉链路，但**没有记录具体模型名**。
建议（不阻塞验收）：把 vision_model 也写进 meta，这样「这一行是谁算出来的」不必靠外部实验反推。

## 六、我没能验证的部分（明确列出，不猜）

1. **「命中图片块时把原图作为多模态内容一起喂给生成模型」**：SSE 接口是黑盒，
   无论服务端只喂「描述 + OCR 文本」还是额外附上原图，客户端看到的 delta 都无法区分。
   契约 §9.8 自己也写明「实测用的 qwen2.5:7b 是纯文本模型，不会附带图片」。
   我验证到的是：图片块可被检索命中、sources 事件里带该块 —— **原图是否进入生成模型的输入，待验证**。
2. **图片理解失败时的运行时降级/重试语义**：本次环境视觉模型可用，我没有构造
   「Ollama 中途挂掉」的场景（只在对照实例里验了「模型名不存在 → 400」，那是配置错误路径）。
3. **大数据量检索性能**（设计文档里的 1 万 / 10 万块复杂度分析）：本次库内最多 6 篇文档，
   **没有做压力测试**，「几千～几万块是舒适区」仍属设计推算，不是实测。
4. **并发写入下的 sha256 去重竞态**：唯一索引在，但我没有并发上传同一份字节去撞它。
5. **多用户权限隔离**：契约明说「不做权限硬隔离」，我按设计照记，未把它当缺陷。

## 七、环境事实校正（实测，与协作简报不一致）

- 简报说「8080 落在 Windows 保留端口段 8015–8114」。**本机当前实测不成立**：
  netsh interface ipv4 show excludedportrange protocol=tcp 的输出是
  2235-2334 / 5357 / 6038-6137 / 6493-6592 / 7080-7179 / 7377-7476 / 9948-10047 /
  13317-13416 / 13417-13516 / 50000-50059，**没有 8015–8114**；
  并且我用 Node 显式 listen(8080) 成功（输出 BIND_8080_OK）。
  这些保留段由 Hyper-V/WSL 动态占用、重启会变，更可能的解释是**当时**被临时占用，而非固定保留。
  我这轮全部用 8300（按协作约定），两种说法都不影响验收结论。
- 探测前 8300/8080 都是 DOWN（实现者的临时进程已不在），我自己 go build 后以 APP_PORT=8300 起服务取证。
- 机器上另有一个 12:47:52 启动的 blog-my 进程（PID 46032），**不是我起的**，
  监听端口也不是 8300/8080/8301，我未动它。

## 八、边界与错误路径（原文摘录）

| 用例 | 请求 | 实测 | 判定 |
| --- | --- | --- | --- |
| 空正文 | POST /api/kb/text {text:"   "} | 400 | PASS |
| 非法扩展名 | multipart bad.pdf | 400 不支持的文件类型 ".pdf" | PASS |
| 缺 file 字段 | 空 FormData | 400 | PASS |
| 超过 20 MiB | 21 MiB .txt | 400 | PASS |
| 非法 JSON | {不是 JSON | 400 | PASS |
| 空白 query（search） | {"query":"   "} | 400 | PASS |
| 空白 query（ask） | {"query":"   "} | 400（发生在写 SSE 响应头之前） | PASS |
| 不存在的 id | GET /api/kb/documents/999999999 | 404 | PASS |
| 非法 id | GET /api/kb/documents/abc | 400 | PASS |
| 重复 DELETE | 同一 id 删两次 | 第二次 404 | PASS |
| top_k=999 | search | 200（被收敛，不报错） | PASS |
| 服务是否崩 | 以上全部 | 服务全程存活，后续请求正常 | PASS |

**观察（不构成 FAIL）**：超过 20 MiB 的请求返回
{"error":"缺少文件字段 file（multipart/form-data）"} —— 与「缺 file 字段」同文案。
契约 §9.3 确实把两种情况写成同一条文案，所以按契约判 PASS；
但调用方无法区分「文件太大」与「没带文件」，建议后续区分（413 或独立文案）。

## 九、SSE 与去重（原文摘录）

~~~
事件序列（23 个，用时 9192ms）: sources -> delta ×21 -> done
[PASS] delta 21 个、答案 30 字；答案："浇水要避开正午高温，早晨浇水可以把叶面病害概率降下来。[1]"
~~~

答案带 [1] 出处标注，说明「只依据资料回答 + 标注出处」的提示词在生成侧生效。

去重（同一份字节重复入库）：
- 文本：第二次 → 200 duplicated:true，id 与首次相同（probe C2）。
- 图片：同一张 PNG 再传 → 200 duplicated:true，**耗时 3ms**（首次 27.2s）——
  说明去重发生在视觉推理**之前**，重复上传不会重复烧算力。
  这一条是实现者脚本没覆盖、我自己撞上的行为（因为我的脚本第一次超时中断后重传才暴露出来）。

## 十、数据清理（不留测试数据）

- 全部测试文档经接口 DELETE；收尾再用 SQL 确认：documents=0, chunks=0, orphans=0, vrf_users=0
  （rag-db-check-02.log 第 10 节原文）。
- 我自己起的两个服务实例（8300 / 8301）已停止：leftover verify-blogmy processes: 0，
  三个端口全部 down，临时二进制 %TEMP%\verify-blogmy.exe 已删除。
- 保留的产物只有证据：docs/verify/tmp/ 下的日志、JSON，以及 3 张测试图片（zebra*.png，最大 16.7 KB）。

## 十一、给 Lead 的一句话

RAG 端到端**独立复验通过（核心链路无 FAIL）**：语义检索在零关键词重叠下仍能正确排第一、
删除后块在 SQL 层归零且无孤儿块、图片入库确实由 qwen2.5vl:7b 完成（配置对照 + Ollama 驻留双证据）。
需要你知道的三件事：①图片上传实测 25.7–27.2s，实现者把脚本超时放宽到 300s 是**有依据的**，
但这是视觉模型延迟而非接口缺陷；②meta 不记模型名，建议补 vision_model；
③「原图是否真的喂给生成模型」黑盒无法证实，已在报告里标为待验证，未当 PASS。

