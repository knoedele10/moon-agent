# Day 21 同伴交叉验证（三行结论）

- 复验人：AI 同伴（独立复验，不是开发者自述的复述）
- 复验时间：2026-10-09 22:48 / 22:50 / 22:53（Asia/Shanghai）
- 复验对象：https://blog-my-check-10974.app.workbuddy.host
- 复验方式（刻意与开发者不同）：不看"我自己写的页面长什么样"，而是用 HTTP 客户端直接打接口 + 无头 Chrome 渲染真实 DOM 取结果
- 复验脚本：scripts/verify-day21.ps1（可重复执行）；原始输出：verify-log.txt

## 三行结论

1. **可打开**：/、/hot、/chat、/static/css/style.css、/api/health 全部 HTTP 200；无头 Chrome 渲染首页 DOM，健康卡片显示"服务正常"（service: blog-my · version: mock-1），/hot 页渲染出 20 条热搜。
2. **可读写**：GET /api/hot?limit=3 返回 3 条真实热搜；POST /api/favorites 返回 201（id=9，中文标题正确），再次回读命中该标题（收藏 8 → 9 条）；重复提交 409、缺 title 400、非法 JSON 400，全部按契约返回。
3. **无报错**：OPTIONS /api/favorites 返回 204 且带 Access-Control-Allow-Origin: *；渲染后的 DOM 里 20 条热搜 + 9 条收藏全部落到页面上（没有停在"加载中/加载失败"占位）；复验 15 项 PASS 15 / FAIL 0。

## 逐条证据

| # | 检查项 | 证据（可打开/可复现） | 实际返回 | 判定 |
|---|---|---|---|---|
| 1 | 探活 | https://blog-my-check-10974.app.workbuddy.host/api/health | 200 {"status":"ok","service":"blog-my","version":"mock-1","time":"2026-10-09T14:50:20.194Z"} | PASS |
| 2 | 首页可打开 | https://blog-my-check-10974.app.workbuddy.host/ | 200，DOM 中 healthText="服务正常"、hot-item=20、fav-item=9 | PASS |
| 3 | 热搜页可打开 | https://blog-my-check-10974.app.workbuddy.host/hot | 200，<title>今日热搜</title>，渲染 20 条 | PASS |
| 4 | 对话页可打开 | https://blog-my-check-10974.app.workbuddy.host/chat | 200，<title>多模型对话</title> | PASS |
| 5 | 静态资源 | https://blog-my-check-10974.app.workbuddy.host/static/css/style.css | 200 text/css | PASS |
| 6 | 真实数据读 | https://blog-my-check-10974.app.workbuddy.host/api/hot?limit=3 | 200 {"ok":true,"data":[{"platform":"微博","rank":1,"title":"曼城英超"},...],"fetched_at":"2026-10-03"} | PASS |
| 7 | 收藏读 | https://blog-my-check-10974.app.workbuddy.host/api/favorites | 200，9 条真实收藏 | PASS |
| 8 | 收藏写 | POST /api/favorites（脚本第 2 组） | 201 {"ok":true,"id":9,"title":"Day 21 交叉验证：公网真实读写往返 225018"} | PASS |
| 9 | 写入可回读（持久化） | 写完再 GET /api/favorites | 命中本次写入标题，条数 8 → 9 | PASS |
| 10 | 重复提交被拒 | 同标题再 POST | 409 {"ok":false,"error":"该内容已在收藏中，请勿重复收藏"} | PASS |
| 11 | 缺必填字段被拒 | POST {"url":"..."} | 400 {"ok":false,"error":"缺少必填字段 title（收藏内容标题不能为空）"} | PASS |
| 12 | 非法 JSON 被拒 | POST 一个非法 JSON 请求体 | 400 {"ok":false,"error":"请求体不是合法的 JSON"} | PASS |
| 13 | 跨域预检 | OPTIONS /api/favorites | 204 + Access-Control-Allow-Origin: * | PASS |
| 14 | 跨域响应头 | GET /api/health（带 Origin） | 200 + Access-Control-Allow-Origin: * | PASS |
| 15 | 页面无 JS 报错（渲染证据） | 无头 Chrome --dump-dom 首页 | 真实数据已渲染进 DOM，非占位文案 | PASS |

原始日志（逐行时间戳与耗时）：verify-log.txt，汇总行 == 汇总：PASS 15 / FAIL 0 ==。

## 复验发现（不是"全绿"就没事）

- **第一次复验是失败的**：22:48 写入的中文标题变成了 Day 21 ????:???????? 224827。定位结果：问题在复验客户端 —— PowerShell 5.1 的 Invoke-WebRequest 把字符串请求体按 ASCII 发送，服务端收到并如实存下了带 ? 的标题。改成显式 UTF-8 字节后（22:50）通过。教训：验证脚本自己也会骗人，编码要在两端都写死。
- **公网没有 DELETE 接口**，复验写入的 2 条测试记录（id=8 带 ? 的那条、id=9 正常的那条）留在数据里，无法清理，如实记录。
- **平台会重启进程，但数据没丢**：多次请求之间 uptime 在 0 / 109 / 115 之间跳变，说明进程被重启过；而 22:48 写入的数据在 22:50 仍能读到，说明持久化落在磁盘文件 deploy/data/favorites.json，不是内存 —— 这正是"真实读写"要比"接口返回 200"更值得验的地方。
- **没验到的**：Ollama 运行时（本机 11434 未启动）、真人用户测试（仍为空模板）。这两项在 acceptance.md 第二节如实标 FAIL，不在这三行结论里混过去。

## 结论

三行结论全部成立，且每一行都能用一条命令复现：

powershell -ExecutionPolicy Bypass -File scripts/verify-day21.ps1 -LogPath docs/day21/verify-log.txt

附带佐证截图：screenshots/day21-4-live-check.png（公网首页真实渲染）。
