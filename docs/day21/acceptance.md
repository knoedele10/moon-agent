# 第 3 周验收表（Day 15–21）

- 验收人：开发者本人（自查）
- 独立复验：AI 同伴（见 [crosscheck.md](crosscheck.md)，与自查分开做，不复用同一套判断）
- 验收时间：2026-10-09 22:40–23:10（Asia/Shanghai）
- 验收对象：blog-my 公网部署 https://blog-my-check-10974.app.workbuddy.host
- 判定口径：**每一项都必须有能点开/能复现的证据**（链接、命令输出、文件、提交号）。拿不到证据的项一律标 FAIL，不写"应该没问题"。

## 一、逐项验收表

| 天 | 本周交付 | 证据（可直接打开） | 判定 |
|---|---|---|---|
| Day 15 | 公网部署 /api/health 与前端 mock 版，产出接口契约 | 探活 https://blog-my-check-10974.app.workbuddy.host/api/health → 200 {"status":"ok","service":"blog-my","version":"mock-1"}；契约 docs/api-contract.md；提交 a090652 | PASS |
| Day 16 | 建表脚本 + 种子数据（hot / favorite 两张表） | db/schema.sql、db/schema-day17.sql、db/seed-day17.sql；提交 040bf2e（提交信息含"select 验证通过"） | PASS（本地证据） |
| Day 17 | GET /api/hot、GET /api/favorites 读接口 + /hot 展示页 | https://blog-my-check-10974.app.workbuddy.host/api/hot?limit=3 → 200，返回微博/知乎/百度真实热搜；https://blog-my-check-10974.app.workbuddy.host/hot → 200，无头浏览器渲染出 20 条；提交 79710ab、d912cb0 | PASS |
| Day 18 | POST /api/favorites 收藏写入 + 重复/缺字段被拒 | 复验日志 docs/day21/verify-log.txt 第 2 组：201（id=9）、409、400、400 四条边界全部按契约返回；提交 d9b194c | PASS |
| Day 19 | LLM 层工厂化重构，接入 Ollama 本地模型 | 代码层：llm/provider.go（Registry/Backend 工厂）、main.go:27-34 注册 openai/claude/gemini/ollama/mock 五个后端；提交 1f256dd（19 文件 +2644/−1414） | 部分通过（运行时未复验，见第二节） |
| Day 20 | 检查台首页上线 + 跨域配置 + 公网展示真实数据 | https://blog-my-check-10974.app.workbuddy.host/ → 200，无头浏览器渲染出"服务正常" + 20 条热搜 + 9 条收藏；OPTIONS /api/favorites → 204 且带 Access-Control-Allow-Origin: *；截图 docs/day21/screenshots/day21-4-live-check.png；提交 e978b60 | PASS |
| Day 21 | 周验收表 + 同伴交叉验证 + 演示提纲（本日） | 本文件；docs/day21/crosscheck.md；docs/day21/demo-outline.md；三张交付截图；脚本 scripts/verify-day21.ps1（15 项 PASS 15 / FAIL 0） | PASS |

## 二、缺项如实标记（FAIL / 未执行）

| 缺项 | 状态 | 为什么缺 | 补齐动作 |
|---|---|---|---|
| Day 14 真人用户测试 | **FAIL（未执行）** | docs/user-test-day14.md 仍是空模板：测试者画像、9 条反馈栏全空；文档里的降级方案（开发者间隔 10 分钟自测）也没有留下记录 | 找 1 位真人按清单走一遍，或做降级自测并回填；属第 2 周遗留 |
| Day 19 Ollama 运行时复验 | **FAIL（未复验）** | 本机 http://localhost:11434 连不上：D:\Ollama\ollama.exe 已安装，但服务未启动、也没有本地模型目录 | ollama serve + ollama pull qwen2.5:0.5b，再走 /api/models 与一次对话复验 |
| 真实厂商 LLM 调用（DeepSeek / OpenAI / 通义…） | 未执行 | 公网 mock 版不含真实 LLM 调用；需要用户自己的 API Key，本周验收范围未包含 | 下周补：本地起 Go 服务 + 填 1 个 key，跑通一次流式回复 |
| 3 分钟演示视频（余力加练） | 未执行 | 今天的时间用在补齐"公网可访问 + 真实读写"两项硬证据上（卡住降级指令的优先级） | 下周加练，提纲已就绪（见 demo-outline.md） |
| 复验写入的测试数据无法删除 | 未执行（本轮副作用） | 公网只有 POST/GET，没有 DELETE 接口 | 如实记录，见第三节 |

## 三、本轮副作用（如实记录，不藏）

- 22:48 第一次复验：PowerShell 5.1 的 Invoke-WebRequest 把中文请求体按 ASCII 发出，服务端存下一条标题带 ? 的记录（id=8）。
- 22:50 修正为 UTF-8 字节重发：写入 id=9，标题中文正确，回读命中，8 → 9 条。
- 两条测试记录都留在公网数据里（没有删除接口）。这是"客户端编码"坑的真实案例，保留作为证据。

## 四、本周复盘：最花时间的一步 + 值不值

- 按改动量：Day 19 的 LLM 层重构最大（19 文件，+2644 / −1414），是本周代码面最大的一步。
- 按反复试错：Day 20 的"公网打通"最花时间（部署 + 跨域 + 真实数据接线）。它的提交只有 4 文件 207 行，但真正耗时间的是部署和 CORS 调试 —— 改动量小不等于省时间。
- 值不值：**值**。三条理由：
  1. 它把"我本地能跑"变成"别人点开链接就能自己验证"，这是第 3 周唯一能对外交付的东西；
  2. 跨域、编码这类坑只有在真实链路上才会暴露 —— 今天复验时又踩到一次客户端编码坑，本地自测永远测不出来；
  3. 演示提纲第 4 段"验证方式"全靠这条链路才有内容可写。
- 说明：以上是自评，不是计时数据；改动量来自 git 提交统计（git log --shortstat）。

## 五、验收结论

- 两项硬指标（**公网可访问 + 真实读写**）：**通过**。证据见 crosscheck.md 的三行结论与 verify-log.txt（15 项 PASS / 0 FAIL）。
- 本周 7 天：6 天 PASS、1 天部分通过（Day 19 运行时未复验）。
- 另有 4 项如实标 FAIL / 未执行，没有用"大概没问题"糊过去。
- 演示提纲已写出并完整走通一遍：走通记录见 demo-outline.md 末节。
