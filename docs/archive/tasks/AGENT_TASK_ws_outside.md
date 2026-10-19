# 任务：工作区从「硬沙箱」改成「首选目录 + 外部需确认」

项目根 `E:\blog-my`。用户要求：删掉「越出工作区一律拒绝」，改成工作区是**首选**，
使用其它目录时：`auto` 档直接做，`plan`/`confirm` 档**每次都问用户**（不记住、不放行下一次）。

## 你负责的文件（只这四个）

1. `agent/fs/tools.go`（+ 同目录 `guard` 相关实现）——路径校验改成可放行外部路径
2. `web/agent_confirm.go`——新增「外部路径确认壳」
3. `web/agent.go`——`agentToolsFor` 里接上壳；系统提示词里写清楚「首选工作区 + 外部需确认」
4. 对应测试：`agent/fs/*_test.go`、`web/agent_confirm_test.go`（追加用例，不改旧断言）

## 冻结设计

- `agent.Exec` 加一个字段：`AllowOutside bool`（true = 允许工作区外的绝对路径）。默认 false，保持现有行为。
- `agent/fs` 的 guard：`AllowOutside` 为 false 时行为**一字不改**；为 true 时放行外部路径，
  但仍必须（a）是绝对路径、（b）做 `filepath.Clean` 归一化、（c）**拒绝**写入 Windows 系统目录
  （`C:\Windows`、`C:\Program Files*`、盘符根目录）——这些永远不给，跟档位无关。
- 新增 `outsideWorkspaceTool` 壳（放 `web/agent_confirm.go`）：包住写类工具（write/edit/move/delete/office_edit），
  在 `Execute` 里解析参数里的路径：只要**有任何一个路径落在工作区外**，且当前档位不是 `auto`，
  就返回 `Result{AwaitUser: "<确认文案>", Display: {"confirm": true, "paths": [...]}}`（走现有确认流程，
  用户点「确认执行」后重放这一次调用，沿用 `web/agent_pending.go` 的机制）。
- `agentToolsFor`：把 `AllowOutside: true` 传给 `Exec`；写类工具先用 `outsideWorkspaceTool` 包一层，
  再交给现有的 `confirmTool`（顺序：外部确认在外，风险确认在内，不要叠加两层弹窗——如果已经要问外部，
  就不用再问风险）。
- 提示词：在 `agentSystemPrompt` 里加一段：「工作区是首选目录；需要碰工作区外的文件时，说明原因并等用户确认；
  系统目录永远不要碰。」

## 验收

- `go test ./agent/... ./web/...` 全绿（构建：`$env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'`）。
- 新增单测至少覆盖：AllowOutside=false 时外部路径仍被拒；=true 时放行且归一化；系统目录始终拒绝；
  outsideWorkspaceTool 在 auto 档不拦、在 confirm 档返回 AwaitUser 且不执行内层。

完成后回报：文件清单 + 测试结果 + 已知缺口。
