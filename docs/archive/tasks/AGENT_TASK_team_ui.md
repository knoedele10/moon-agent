# 任务：团队面板（前端）

先读 `E:\blog-my\docs\agent\07-team.md` 的 **1.3**。
再读 `templates/static/js/taskcard.js`（右上角「当前任务」卡片，v1 的渲染器）与 `templates/static/js/chat.js` 里
子 agent 轮询那段（`subagentPoll`，≈1600-1700）。

## 你负责的文件（只有三个）

1. `templates/static/js/taskcard.js`
2. `templates/static/js/chat.js`
3. `templates/static/css/style.css`（只追加）+ `scripts/verify-ui.cjs`（只追加断言）

**不要动** Go 代码。

## 要实现的东西

1. `taskcard.js` 新增 `window.TASKCARD.setTeam(team)`：把 `{members:[], tasks:[]}` 渲染成卡片里的一段「团队」区域。
   - 成员一行：名字 + 状态点（running 绿、inactive 灰、failed 红）+ 角色（lead 特殊标记）。
   - 任务一行：`task-1` + 标题 + 状态（pending / 进行中 / 完成）+ owner。
   - **空数据（两个数组都空）时整段隐藏**（不能留一个空标题）。
   - 用 createElement 拼 DOM，不用 innerHTML 拼数据。
2. `chat.js`：已有的子 agent 轮询里**顺便**拉 `GET /api/conversations/:id/team`（别新增定时器），
   把结果交给 `window.TASKCARD.setTeam`；切会话 / 没有 running 子任务时也要至少拉一次（否则面板永远是空的）。
3. CSS 追加 `.task-card-team` / `.team-member-row` / `.team-task-row` 系列，沿用现有变量，不写死颜色。
4. `verify-ui.cjs` 加 2 条断言：喂 roster+task 数据后卡片里出现成员行与任务行；喂空数据后团队区域隐藏。

## 验收

- `node --check templates/static/js/taskcard.js`、`node --check templates/static/js/chat.js`。
- 跑 `node scripts/verify-ui.cjs`（服务端先起：`Start-Process -FilePath "E:\blog-my\bin\blog-my.exe" -WorkingDirectory "E:\blog-my" -WindowStyle Hidden`）
  ——**现有 33 条断言必须全过**，新加的也要过；后端接口可能还没上，新断言用 fetch 桩喂数据，不依赖真接口。

完成后回报：文件清单 + 验证输出 + 新断言原文。
