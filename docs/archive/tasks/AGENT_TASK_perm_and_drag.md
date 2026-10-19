# 任务 A：把「模式 × 位置 × 路径形态」的写入权限矩阵查清并修好

用户反馈：**写入权限依然没开**。上一轮只做到「工作区外可用、需确认」，但用户实测还是写不进去。
你要做的是**端到端排查**，不要只读代码就下结论。

## 背景

- 相关代码：`agent/fs/tools.go`、`agent/fs/outside.go`（`AllowOutside` 策略）、`web/agent_confirm.go`
  （`outsideWriteTool` / `confirmTool`）、`web/agent.go`（`agentToolsFor`、`agentSystemPrompt`）、
  `web/agent_pending.go`（确认后重放）。
- 用户的报错原文（改版前）：`write：失败 — io error on "blog/DESIGN.md": Access is denied.`
  出现过的路径：`blog/DESIGN.md`、`6-文档/权限测试.txt`、根目录 `权限测试.txt`（都是**相对路径**）。
- 档位：plan / confirm / auto（`web/agent_settings.go`），工作区来自 `AGENT_FS_ROOT` 或用户设置。

## 必须回答并验证的矩阵（每条都要有实测或单测证据）

| 档位 | 路径 | 期望 |
| --- | --- | --- |
| plan | 工作区内 `.md` | 允许（计划存档是默认操作） |
| plan | 工作区内非 `.md` | 拒绝 |
| plan | 工作区外 | 拒绝 |
| confirm | 工作区内写 | 弹一次风险确认 → 确认后成功 |
| confirm | 工作区外写（**绝对路径**） | 弹外部确认 → 确认后成功 |
| confirm | 工作区外写（**相对路径**） | **必须给出人能看懂的拒绝理由**，不能只报 `Access is denied` |
| auto | 工作区内写 | 直接成功 |
| auto | 工作区外写（绝对路径） | 直接成功 |
| 任意档 | 工作区为空/未设置 | 文件工具是否注册？报错文案是什么？ |

重点怀疑点（逐个排除，不要跳过）：
1. **相对路径**在工作区外一定被拒（`resolveOutside` 只收绝对路径）——用户报的三个路径全是相对路径，
   很可能就是这一条。要求：拒绝时的文案必须明确说「工作区外只接受绝对路径，请给出完整路径」，
   并且**系统提示词里也要写这一条**，否则模型会一直用相对路径撞墙。
2. `confirm` 档下外部写是否**只弹一次窗**（外部壳在外、风险壳在内），确认后是否真的重放并成功。
3. `agent_pending.go` 重放时的 `Exec` 是否带 `AllowOutside: true`、`Root` 是否是当轮工作区。
4. 工作区路径本身不可写时（不存在 / 只读 / 需要管理员）报的错是否可区分——如果是这种，
   文案要提示「换一个可写目录」，而不是让用户以为是权限没开。
5. 子 agent（`web/subagent.go`）的 `Exec` 目前是 `AllowOutside=false`；确认这是否也是用户踩到的路径。

## 交付

1. 修掉上面查出的真问题（改哪个文件你自己定，但**不要**动 `templates/`，那是任务 B 的文件）。
2. 新增/补测试：至少覆盖「工作区外相对路径的拒绝文案」「confirm 档外部写确认后成功」「auto 档外部写直接成功」。
3. 在 `E:\blog-my\build\perm-matrix.md` 写一张**实测表**：每格写「结果 + 证据（测试名或命令输出）」。
4. `go build ./...` 与 `go test ./agent/... ./web/... -count=1` 必须全绿
   （`$env:GOCACHE='E:\blog-my\build\gocache'; $env:GOTMPDIR='E:\blog-my\build\gotmp'`）。

完成后回报：改了哪些文件、矩阵表结论、明确没查清的点。

---

# 任务 B：右上角「当前任务」卡片做成可拖动

用户反馈：**还是不能拖动**。

## 要做的

- `templates/static/js/taskcard.js`：卡片支持鼠标拖动（按住卡片头部拖动，`.task-card-head` 作为把手；
  拖动时不要触发里面的收起按钮）。位置用 `position: fixed` 或改现有的 `absolute` 定位方案都行，
  但**不能**影响 `#task-card` 现有的收起/展开、内容渲染与轮询更新。
- 位置持久化到 `localStorage['blogmy:taskcard:pos']`（`{"x":..,"y":..}`），刷新后回到原位；
  拖出视口要夹回可见范围（至少留 40px 在窗口内）。
- 窗口 resize 时也要夹一次，避免拖到边缘后窗口变小就再也点不到。
- 触摸事件可选（能加就加，别为它引入依赖）。
- `scripts/verify-ui.cjs` 追加 2 条断言：模拟 pointer/mouse 拖动后卡片位置变化且写入 localStorage；
  重新 init 后位置被还原。
- 只改 `templates/static/js/taskcard.js`、`templates/static/css/style.css`（追加）、`scripts/verify-ui.cjs`（追加）。

## 验收

- `node --check templates/static/js/taskcard.js`；`node scripts/verify-ui.cjs` 现有 37 条全过 + 新增 2 条。
- 服务端先起：`Start-Process -FilePath "E:\blog-my\bin\blog-my.exe" -WorkingDirectory "E:\blog-my" -WindowStyle Hidden`。

完成后回报：文件清单 + 验证输出 + 新断言原文。
