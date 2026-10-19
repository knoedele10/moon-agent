# Day 23 验收记录：本地多模态 RAG 的前端接入与端到端验收

> **一句话结论**：后端 `api/kb/*` 一行未改；前端已经把知识库真正接进应用（侧栏「知识库」→ 上传 / 检索 / 问答 / 删除）；
> 三条验收运行全部 **FAIL 0、退出码 0**（`PASS 55` + `PASS 59` + `PASS 28`）。
>
> 本文件里的每一条结论都标了来源：**实测**（本次真跑出来的）、**设计如此**（读代码/按实现得出，没单独跑）、**待验证**（没跑过、也不该假装跑过）。

---

## 1. 交付了什么

| # | 交付物 | 位置 |
| --- | --- | --- |
| 1 | 前端知识库入口与交互（上传文本/图片、文档列表、删除、检索、基于知识库的 SSE 问答） | `templates/chat/index.html`、`templates/static/js/kb.js`、`templates/static/css/style.css`、`templates/static/js/chat.js` |
| 2 | 端到端验收（后端 HTTP 契约 2 次 + 真实浏览器前端 1 次） | `scripts/verify-rag.mjs`、`docs/day23/verify-ui.mjs`、完整输出 `docs/day23/verify-log.txt` |
| 3 | 验收记录（本文件） | `docs/day23/notes.md` |
| 4 | 接口契约增补 `api/kb/*` | `docs/api-contract.md` 第 9 节 |
| 5 | 证据留档 | `docs/day23/rag-transcript.json`、`docs/day23/rag-transcript-ask.json`、`docs/day23/ui-transcript.json`、`docs/day23/screenshots/*.png` |

**后端零改动**：`web/kb.go`、`rag/**`、`model/kb.go` 本次未修改（`git status` 里它们是「后端先写好、未提交」的 `????` 状态，不是本次的改动）。
没有发现后端 bug；下面第 7 节列出的是**验收脚本自身**的两个缺陷和**我自己引入**的一个前端 UX 缺口。

---

## 2. 环境事实：8080 在本机不可用（重要，先说）

本题给的默认端口是 8080，但**本机绑不上 8080**：

```
2026/10/11 13:10:30 监听失败: listen tcp 127.0.0.1:8080: bind: An attempt was made to access a socket in a way forbidden by its access permissions.
```

原因不是端口被占用，而是 8080 落在 **Windows 保留端口段**里（Hyper-V/WSL 动态端口预留）：

```
> netsh interface ipv4 show excludedportrange protocol=tcp
Start Port    End Port
      8015        8114        <-- 8080 在这里面
```

**处理方式**：服务改在 8300 起（`$env:APP_PORT="8300"`），验收脚本用 `--base http://127.0.0.1:8300`。
这不是代码问题，换任何一台没有该保留段的机器都能用 8080 直接跑（脚本默认值仍是 8080，没改）。
如果一定要在 8080 跑，需要管理员执行 `net stop winnat` / `net start winnat` 或调整保留段 —— 本次没做（超出子任务权限）。

> 实测：改 8300 后 `GET /api/health` 返回 `200 {"service":"blog-my","status":"ok","time":"...","version":"go-1"}`。

---

## 3. 改动清单（逐文件 + 为什么）

### 3.1 前端（真正把知识库接进应用）

| 文件 | 改了什么 | 为什么 |
| --- | --- | --- |
| `templates/chat/index.html` | 侧栏导航加第 4 个 tab「知识库」（`href="#/kb"`）；`<main>` 里加 `<section class="view" id="view-kb">`：统计条 + 上传文件卡片 + 粘贴文本卡片 + 检索卡片 + 知识库问答卡片 + 文档列表；末尾引入 `/static/js/kb.js`（在 `chat.js` **之前**） | 让知识库成为应用内的一个视图，而不是一个孤立页面；kb.js 先加载，chat.js 的路由里才能安全调用 `window.KB` |
| `templates/static/js/kb.js`（新增） | 知识库视图的全部行为：统计、文档列表与删除、multipart 上传、粘贴文本入库、检索渲染、`/api/kb/ask` 的 SSE 流式问答与出处渲染；导出 `window.KB = { onShow, refreshAskModel }` | 与 chat.js 的既有风格保持一致（同样的 `api()/apiJSON()` 401 跳登录、同样的 toast），不引入框架、不新增依赖 |
| `templates/static/js/chat.js` | ① `VIEW_TITLES` 加 `kb: '知识库'`；② `showView()` 里切到 kb 时调 `window.KB.onShow()`；③ 模型列表加载完成后调一次 `window.KB.refreshAskModel()` | 复用既有的 hash 路由与视图切换机制，不另起一套 |
| `templates/static/css/style.css` | ① `@[data-view="kb"]` 的显示/隐藏规则（复用 models/keys 那套）；② 末尾追加约 420 行 `.kb-*` 组件样式 + 窄屏媒体查询 | 风格沿用既有设计变量（`--surface`/`--border`/`--accent` 等），不引入新配色 |

**交互契约（前端只调既有的 `api/kb/*`，没有新增任何接口字段）**：
- 上传：`POST /api/kb/documents`（`FormData`，字段名 `file`）
- 粘贴文本：`POST /api/kb/text`（JSON `{title,text}`）
- 列表/删除：`GET /api/kb/documents`、`DELETE /api/kb/documents/:id`
- 统计：`GET /api/kb/stats`
- 检索：`POST /api/kb/search`
- 问答：`POST /api/kb/ask`，按 `sources` → `delta*` → `done` / `error` 解析 SSE

### 3.2 验收脚本

| 文件 | 改了什么 |
| --- | --- |
| `scripts/verify-rag.mjs` | ① 新增 `--upload-timeout`（默认 300000ms）；② 上传素材改为**逐运行唯一**（给 PNG 加一个 tEXt 辅助块）；③ 新增「图片去重契约」检查；④ 新增第 9 步「契约边界」（19 项错误路径检查） |
| `docs/day23/verify-ui.mjs`（新增） | 真实 Chromium（Playwright）打开页面、点真实按钮，覆盖 上传 / 检索 / 问答 / 图片入库 / 删除 五条路径，产出截图与 JSON 证据 |
| `docs/day23/check-screenshots.py`（新增） | 截图非空洞检查（灰度标准差阈值），把"机器能判的那部分"自动化 |

### 3.3 文档

| 文件 | 改了什么 |
| --- | --- |
| `docs/api-contract.md` | 新增第 9 节 `api/kb/*`（9.1–9.8，逐字段），顶部加一句 Day 23 说明。第 9 节里写的状态码与错误文案，全部与实现核对过，并在第 9 步验收里自动断言 |
| `docs/day23/notes.md` | 本文件 |

---

## 4. 验收怎么跑、跑出了什么

三段运行，完整原始输出见 `docs/day23/verify-log.txt`（未删改拼接）。

### 段一：后端 HTTP 契约（不带 `--ask`）

```bash
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 \
     --log docs/day23/verify-log-run1.txt \
     --transcript docs/day23/rag-transcript.json
```

**实测结果：PASS 55 / WARN 0 / FAIL 0，退出码 0。**

覆盖：登录 → 文本入库（中文长文 1505 字）→ 列表可见 → 检索命中 → 详情含块 → 图片入库（multipart，真跑视觉模型）→ 图片去重契约 → 图片检索 → 删除闭环 → 契约边界。
关键数字（来自 `rag-transcript.json`）：文本文档 `id=16`、切 3 块；检索首位 `document_id=16`、`score=0.450`；图片文档 `id=17`、1 块、素材 213 字节；
第二次上传同一份字节 → `200 {"duplicated":true,"id":17}`（同一 id，未新建）。

### 段二：后端 HTTP 契约（带 `--ask`，多验一条 SSE）

```bash
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 --ask \
     --log docs/day23/verify-log-run2-ask.txt \
     --transcript docs/day23/rag-transcript-ask.json
```

**实测结果：PASS 59 / WARN 0 / FAIL 0，退出码 0。**

`/api/kb/ask` 实测事件序列：`sources` → 52 个 `delta` → `done`；`sources.hits` 5 条；
用 `qwen2.5:7b` 生成，回答原文：`紫罗兰-20261011133831是Day 23端到端验收的暗号词，只应出现在特定的文本里，用于验证检索结果。[2][4][5]`
—— 出处编号 `[2][4][5]` 与 `sources` 的编号对得上，说明「带出处的回答」这条链路是通的。

### 段三：前端页面（真实浏览器）

```bash
node docs/day23/verify-ui.mjs --base http://127.0.0.1:8300 \
     --log docs/day23/ui-verify-log.txt \
     --transcript docs/day23/ui-transcript.json \
     --screenshots docs/day23/screenshots
```

**实测结果：PASS 28 / WARN 0 / FAIL 0，退出码 0。** 五条路径都是**在页面上点出来的**，不是直接打接口：

| 路径 | 实测证据（来自 `ui-transcript.json` / `ui-verify-log.txt`） |
| --- | --- |
| 入口 | 侧栏出现「知识库」tab；点击后 `hash=#/kb`、`#view-kb` 可见、`#messages` 隐藏、统计数字加载出真实值 |
| 上传（文本） | 选 `ui-verify-20261011132050.md` → 点「上传入库」→ 提示 `已入库：id=14，kind=text，切出 2 个块`→ 列表出现该行 |
| 检索 | 输入暗号词 → 2 个命中块，`document_id=14,14`（都是刚上传的文档），首位正文包含暗号词，相似度 0.432 / 0.411 |
| 上传（图片） | 选 PNG → 点「上传入库」→ **13.3s** 后提示 `已入库：id=15，kind=image，切出 1 个块` → 列表出现图片行 |
| 问答 | 顶栏选 `qwen2.5:7b` → 提问 → **4.6s** 后出处 3 条、回答 48 字：`本次前端验收文本里的暗号词是 UIVERIFY-20261011132050。[1][2][3]`（暗号词逐字命中） |
| 删除 | 点行的「删除」→ 确认 → 列表行数 2 → 1，该行消失；随后把本次创建的图片文档也删掉，知识库还原 |
| 资源健康 | 页面加载的资源 0 个 4xx/5xx（`/favicon.ico` 除外，后端本就没有该路由） |

### 段三的截图证据

7 张，`docs/day23/screenshots/`：`kb-1-view.png`（入口）、`kb-2-upload.png`、`kb-3-search.png`、`kb-4-image-upload.png`、`kb-5-ask.png`、`kb-6-delete.png`、`kb-7-cleanup.png`。
机器可判的部分（非空洞）**实测**通过：

```bash
python docs/day23/check-screenshots.py
# 7 张全部 1440x1180，灰度均值 29.3–32.7，标准差 24.5–26.7，灰阶 223–225
# FLAT(possibly blank): none
# RESULT: PASS (threshold: grayscale stddev >= 3.0)
```

> **我（AI）没能亲眼看出截图好不好看**：当前模型不接受图片输入，`read_image` 直接被拒。
> 所以"排版是否错位、配色是否协调"这部分是**未自动验证**的，只能由人眼看这 7 张图（见第 10 节人工步骤）。

---

## 5. 实测结论（跑了才知道的）

1. **视觉模型是整条链路里最慢的一环，冷启动约 56 秒。** 用 166 字节的 8x8 PNG 直接打 `POST http://127.0.0.1:11434/api/chat`（`qwen2.5vl:7b`）实测 **56491ms**（模型首次加载进显存）；热态下同样一张图走前端上传只要 **13.3s**。后端的视觉超时是 5 分钟，所以冷启动不会把接口打挂，但**前端点「上传入库」后可能要等一分钟**（页面只显示 "上传中…"，没有进度条）。
2. **去重是按 sha256 内容做的**，实测：同一份 213 字节 PNG 传两次，第二次返回 `200 {"duplicated":true,"id":17}`，复用原 id、不新建行、不重跑视觉模型。
3. **检索是纯向量余弦 topK，没有词法保证。** 实测中，唯一包含暗号词的文档**并不总是排第一**：当库里还有别的图片描述块时，暗号词文档掉到第 3 位（相似度 0.426，前面两条是 0.485/0.481）。所以验收脚本里"命中集合里有暗号词"是**硬断言**，"排第一的是它"只记 WARN —— 这是检索器的性质，不是前端或接口的缺陷。
4. **中文全链路无损。** 1505 字中文正文经 `POST /api/kb/text` 入库后标题逐字回显一致（没有 `?` 之类的编码损坏）；SSE `delta` 拼出来的是完整中文。
5. **`/api/kb/ask` 的 400 与 200 边界是干净的**：空 query 在写 SSE 头之前就被拦下，返回 `400 application/json {"error":"query 不能为空"}`；未知模型则已是 `200 text/event-stream`，先给 `sources`（可能为空）再用 `error` 事件收尾 `{"error":"llm: 端点不存在"}`，且不会出现 `done`。
6. **未登录访问 `api/kb/*` 是 JSON 401 `{"error":"未登录"}`**，不是 302 跳登录（页面请求才跳）。
7. **8080 在本机被系统保留段挡住**（见第 2 节），实测 WSAEACCES。

---

## 6. 证据文件索引

| 文件 | 内容 |
| --- | --- |
| `docs/day23/verify-log.txt` | **主证据**：三次运行的完整原始输出（拼接，未删改），合计 PASS 142 / WARN 0 / FAIL 0 |
| `docs/day23/verify-log-run1.txt` | 段一原始日志 |
| `docs/day23/verify-log-run2-ask.txt` | 段二原始日志（含 SSE 事件序列与回答原文） |
| `docs/day23/ui-verify-log.txt` | 段三原始日志（前端逐条断言） |
| `docs/day23/rag-transcript.json` | 段一证据 JSON（每个请求的请求体/响应体、全部检查项） |
| `docs/day23/rag-transcript-ask.json` | 段二证据 JSON（含 ask 的完整事件与回答） |
| `docs/day23/ui-transcript.json` | 段三证据 JSON（上传/检索/问答/删除的结构化证据 + console 错误 + 坏响应） |
| `docs/day23/screenshots/` | 7 张页面截图 |
| `docs/day23/verify-ui.mjs` | 前端验收脚本本体（可重跑） |
| `docs/day23/check-screenshots.py` | 截图非空洞检查（可重跑） |
| `docs/api-contract.md` 第 9 节 | `api/kb/*` 接口契约（逐字段） |

---

## 7. 过程中发现并修掉的 3 个问题（都不是后端 bug）

### 7.1 验收脚本的 20 秒超时对图片上传太短（脚本缺陷）

第一次跑段一，图片上传记了 4 条 FAIL，报"请求超时（20000ms）"。排查：不是接口慢，是脚本的 `TIMEOUT_MS = 20000` 对"现场调用视觉模型"这个动作根本不合适 —— **实测冷启动 56.5 秒**。
**修法**：`scripts/verify-rag.mjs` 增加 `--upload-timeout`，默认 `300000ms`，与后端自己的 5 分钟视觉超时对齐（`rag/embed.go` 的 `withDefaults()`）。默认值给小了只会让脚本先于后端放弃，把一个正常但慢的请求记成 FAIL。

### 7.2 固定 PNG 素材让第二次运行命中上一轮的 sha256 去重（脚本缺陷）

段二第一次跑出 `FAIL 图片上传返回 201（实际 200）`：因为段一留下的图片文档和段二用的是**同一份字节**，被去重命中，第二次返回 `200 duplicated=true` —— 也就是说"本次图片入库"其实没有发生。
**修法**：素材改为逐运行唯一 —— 给底图追加一个 PNG `tEXt` 辅助块（标准块，解码器会忽略，图片本身不变），把本次 runTag 写进字节；同时**顺手补一条去重契约断言**（同一份字节再传一次必须 `200 + duplicated=true + 同一个 id`），把原来那个"意外"变成显式契约。

### 7.3 我自己引入的前端缺口：知识库视图把模型选择器藏了（前端缺陷）

第一版 CSS 把 `.model-wrap` 在知识库视图也隐藏了，结果「基于知识库问答」拿不到模型 —— 前端验收当场暴露（`page.selectOption('#model-select')` 超时，元素不可见）。
**修法**：知识库视图保留顶栏的模型选择器（它本来就该决定用哪个模型生成回答），并在问答卡片里显式写出「将使用顶栏选中的模型 qwen2.5:7b 生成回答」，避免用户误以为问答用的是"知识库内置模型"。

### 7.4（附）前端验收脚本自身的两个坑

- **读回答读得太早**：只等"回答有内容"是不够的（第一个 `delta` 就满足了），结果读到半截回答，而且后续删除会被页面的 busy 守卫挡掉 → 误报 FAIL。改成**等提问按钮重新可用**（流真正结束）再读。
- **404 归因**：页面有一条 `/favicon.ico` 的 404（后端本就没有该路由，是既有行为）。现在脚本把它单独标注排除，同时对"其他资源 4xx/5xx"做**硬断言**（这样 kb.js / style.css 万一 404 会被抓住）。

---

## 8. 设计如此 / 待验证

### 8.1 设计如此（读代码或按实现得出，未单独实测）

- **切块策略**：长文按长度切成交叠小块，每块单独嵌入；具体窗口/重叠参数在 `rag/chunk.go`，本次没有单独测边界。
- **top_k 收敛**：`clampTopK()` 读代码确认 `<=0 → 5`、`>20 → 20`（本次库里块数少，没能观察到 20 的上限行为）。
- **上传上限 20 MiB**（`kbMaxUploadBytes = 20 << 20`）：读代码确认，未实测超大文件；超限时因 `MaxBytesReader` 在 `FormFile` 阶段报错，返回的是 `400 缺少文件字段 file` 这条文案（不够精确，但**设计如此**）。
- **列表最多 200 条、按 id 倒序**：读代码确认（`Order("id desc").Limit(200)`），未实测 200 条以上的截断。
- **检索范围含公共库**：SQL 是 `user_id = ? OR user_id = ''`，即"我的 + 无主的"，读代码确认；**多用户隔离场景未实测**（当前只有一个验收账号）。
- **图片召回依赖视觉描述**：图片本身不产生向量，靠 `qwen2.5vl:7b` 生成的「描述 + OCR」参与召回 —— 实测能看到入库块文本确实是 `【图片】<文件名>【描述】… 【文字】…` 的形式，但**召回质量的好坏未做系统评测**。
- **多模态回喂**：仅当命中 `image_caption` 且模型名含 `vl/vision/gemma3` 时最多附带 3 张原图；本次用的是纯文本 `qwen2.5:7b`，**未实测附图的问答**。

### 8.2 待验证（明确没跑过）

- **界面视觉**：截图有了，但"好不好看 / 有没有错位"需要人眼看（我这边模型不接受图片输入，见第 4 节末的说明）。
- **窄屏 / 移动端布局**：只写了 `max-width: 720px` 的媒体查询，**未实测**。
- **Electron 桌面壳内的表现**：本次只在 Chromium 无头浏览器里验过；`npm start` 走的是 Electron 壳，**未实测**。
- **并发与性能**：入库/检索的吞吐、大文档耗时，**未做基准**。
- **大图 / 多图文档**、**增量索引**（改文档 = 重算全量）：与 `docs/rag-kb.md` 第 6 节的已知限制一致，**待验证**。
- **多用户权限隔离**：见上。

---

## 9. 遗留问题

1. **端口**：本机 8080 被系统保留段占用，验收一律用 `--base http://127.0.0.1:8300`。要长期跑在 8080，需要管理员调整保留段；或者干脆把 `.env` 的 `APP_PORT` 改成一个空闲端口（代码无需改）。
2. **前端验收脚本有环境依赖**：`docs/day23/verify-ui.mjs` 里的 `playwright-core` 走的是本机固定路径（`openclaw` 自带，版本 1.60.0）+ `ms-playwright/chromium-1223`；浏览器可执行文件路径也在脚本顶部的候选数组里。换机器要么装同等版本，要么改这两处候选路径（脚本会在启动时明确报"找不到"，不会假装通过）。
3. **`/favicon.ico` 无路由**，浏览器控制台会有一条 404（既有行为，不是本次引入；验收脚本已单独排除并注明）。
4. **图片上传没有进度反馈**：冷启动可能要等 1 分钟，页面只有"上传中…"文案。**设计如此**，后续可加进度条/分阶段提示。
5. **删除确认用的是浏览器原生 `confirm()`**，样式与页面不一致（功能正常）。后续可替换为页面内确认。
6. **检索没有重排**：语义相近但无关的块会混进上下文，这也是"暗号词文档不一定排第一"的原因之一（`docs/rag-kb.md` 已列为已知限制）。

---

## 10. 人工复核步骤（约 5 分钟）

```powershell
# 0) 起服务（8080 若仍被保留段占用，就用 8300）
cd E:\blog-my
$env:APP_PORT="8300"; go run .        # 看到 "READY http://127.0.0.1:8300" 即可

# 1) 后端契约复跑（不带 / 带 --ask）
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 `
     --log docs/day23/verify-log-run1.txt --transcript docs/day23/rag-transcript.json
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 --ask `
     --log docs/day23/verify-log-run2-ask.txt --transcript docs/day23/rag-transcript-ask.json

# 2) 前端页面复跑（真实浏览器）
node docs/day23/verify-ui.mjs --base http://127.0.0.1:8300 `
     --log docs/day23/ui-verify-log.txt --transcript docs/day23/ui-transcript.json `
     --screenshots docs/day23/screenshots

# 3) 截图非空洞检查
python docs/day23/check-screenshots.py
```

期望：三条脚本退出码都是 `0`、`FAIL 0`；`check-screenshots.py` 输出 `RESULT: PASS`。

**然后人眼看这 7 张图**（我无法替代这一步）：`docs/day23/screenshots/kb-1-view.png`（入口与整体布局）、`kb-2-upload.png`（上传后列表）、`kb-3-search.png`（检索命中）、`kb-4-image-upload.png`（图片文档）、`kb-5-ask.png`（出处 + 流式回答）、`kb-6-delete.png`、`kb-7-cleanup.png`。

**手工点一遍**（如果不想跑脚本）：浏览器登录 → 侧栏「知识库」→ 粘贴一段带唯一词的中文 → 入库 → 检索该词 → 顶栏选 `qwen2.5:7b` 后提问 → 看是否有 `[1][2]` 式出处 → 删除文档。

---

## 11. 我没有做的事（避免误解）

- **没有 `git add` / `git commit`**（按要求，由 Lead 统一提交）。
- **没有改后端**：`web/kb.go`、`rag/**`、`model/kb.go`、`web/web.go` 一行未动。
- **没有改 `docs/cua/**`、`cua/**`、`docs/verify/**`**。
- **没有改 8080 的系统保留段**（需要管理员权限，超出子任务范围）。
- **没有在 Electron 壳里跑**（只在 Chromium 无头浏览器里验过前端）。

---

## 12. 复现本记录的关键命令

```powershell
# 环境：Windows + Go 1.25.3 + Node v24.16.0；Ollama 127.0.0.1:11434（bge-m3:latest / qwen2.5vl:7b / qwen2.5:7b）；MySQL 127.0.0.1:3306/blog_my
netsh interface ipv4 show excludedportrange protocol=tcp      # 确认 8080 是否落在保留段里
go build -o "$env:TEMPlog-my-day23.exe" .                   # 编译（未改后端，编译通过）
$env:APP_PORT="8300"; & "$env:TEMPlog-my-day23.exe"         # 起服务，看到 READY 行
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 --log docs/day23/verify-log-run1.txt --transcript docs/day23/rag-transcript.json
node scripts/verify-rag.mjs --base http://127.0.0.1:8300 --ask --log docs/day23/verify-log-run2-ask.txt --transcript docs/day23/rag-transcript-ask.json
node docs/day23/verify-ui.mjs --base http://127.0.0.1:8300 --log docs/day23/ui-verify-log.txt --transcript docs/day23/ui-transcript.json --screenshots docs/day23/screenshots
python docs/day23/check-screenshots.py
```
