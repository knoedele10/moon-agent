# Day 22 交付说明（PATCH / DELETE，数据操作闭环）

- 日期：2026-10-10
- 今日范围：PATCH 实现 → DELETE 实现 → 增删改查四类操作闭环验证
- 今日不做：批量操作、用户系统

## 做了什么

在公网 mock 版（`deploy/server.js`，即 Day 17/18/20 一直在用的那套收藏接口）上补齐了单条记录的改与删：

- `PATCH /api/favorites/{id}`：只改请求体里出现的字段（`title` / `url`），响应带回 `before` 原值，方便做「改之前 vs 改之后」对比。
- `DELETE /api/favorites/{id}`：**软删除** —— 只写 `is_deleted: true`，不真删数据；`GET /api/favorites` 查询时跳过它。
- 顺手把 `Access-Control-Allow-Methods` 从 `GET,POST,OPTIONS` 扩到含 `PATCH,DELETE`，否则浏览器跨域发 PATCH/DELETE 会被预检拦下。

## 完成标准对照

| 标准 | 结果 | 证据 |
|---|---|---|
| PATCH 修改一条数据生效 | 通过 | `screenshots/day22-1-patch-before-after.png`；`verify-log.txt` 第 2–4 步 |
| DELETE 删除后 GET 不再返回 | 通过 | `screenshots/day22-2-delete-gone.png`；`verify-log.txt` 第 6 步 |
| 增删改查四类操作闭环 | 通过 | POST 造数 → GET 读 → PATCH 改 → DELETE 删，一条链走完，18 项断言全 PASS |

## 一条命令复现

```bash
# 1) 起本地实例（在 deploy 目录下，端口随环境变量）
PORT=8123 node deploy/server.js
# 2) 跑验收（另开一个终端）
node scripts/verify-day22.mjs --base http://127.0.0.1:8123 \
  --log docs/day22/verify-log.txt --transcript /tmp/day22-transcript.json \
  --data deploy/data/favorites.json
```

为什么用 Node 脚本而不是 PowerShell：Day 21 踩过「Windows PowerShell 5.1 把中文请求体按 ASCII 发送」的坑，
Node 的请求默认 UTF-8，从根上避开。脚本只读接口，不改业务代码。

## 余力加练：软删除（已做）

DELETE 不真删数据，只打 `is_deleted` 标记，查询时跳过。删错的记录仍完整留在 `deploy/data/favorites.json` 里，
随时能人工找回。截图 2 最后一栏贴出了该条记录在文件里的完整内容（含 `is_deleted: true`）。
未做「一键恢复」接口 —— 今日范围没要求，留作后续。

## 还没做 / 待确认

- **公网部署**：本次验证跑在本机实例（`http://127.0.0.1:8123`）。把改动推到公网 `blog-my-check-10974.app.workbuddy.host`
  需要在 WorkBuddy 里重新部署 `deploy/` 目录，这一步由你确认后执行。
- 本地 git 仓库没有配置 remote，`git push` 无目标；本次只做本地提交。
