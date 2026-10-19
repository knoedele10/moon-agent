# 任务：ask_user 改成「选项式提问」

项目根 `E:\blog-my`。用户要求：agent 向用户提问时要**给选项**（按钮），而不是让用户在输入框里打。

## 你负责的文件（只这三个）

1. `agent/toolkit/human.go`（`ask_user` 工具）+ `agent/toolkit/human_test.go`
2. `web/agent.go`（`need_input` 事件里把选项带出去）
3. `templates/static/js/chat.js` + `templates/static/css/style.css`（选项按钮的渲染与样式，追加为主）

## 冻结设计

- `ask_user` 参数新增**必填** `options: string[]`（2–4 个，每个 ≤ 40 字），`question` 仍必填。
  参数非法（0 个 / >4 个 / 空串）→ `Result{IsError:true}`，文案告诉模型「必须给 2-4 个选项」。
- `Result.Display` 里带 `{"options": [...], "question": "..."}`（前端 `renderAskOptions` 已经认识这个形状）。
- 前端：选项渲染成按钮（已有 `renderAskOptions`，确认它真的被调用到），每个按钮点击即发送该选项文本；
  最后额外加一个「其他…」按钮 → 打开现有页内对话框（`window.UIASK.prompt`，v1 已实现）让用户自由输入。
- 保留自由输入的可能：如果用户直接在输入框打字发送，也要能回答（不能因为改成选项就把输入框堵死）。
- 系统提示词里加一句：「需要用户补充信息时，用 ask_user 给出 2-4 个具体选项；
  确实无法穷举时把最可能的几个列出来，用户仍可自由输入。」

## 验收

- `go test ./agent/... ./web/...` 全绿；`node --check templates/static/js/chat.js`。
- `node scripts/verify-ui.cjs` 现有断言（目前 35 条）全过，并**追加 1-2 条**：喂一个带 options 的 need_input 事件 →
  断言出现 N 个选项按钮 + 一个「其他…」按钮，点第一个后输入框里出现该选项文本。

完成后回报：文件清单 + 验证输出 + 新断言原文。
