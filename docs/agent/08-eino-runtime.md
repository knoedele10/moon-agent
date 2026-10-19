# Eino ReAct 执行核心

服务进程默认使用 Eino ReAct 执行启用工具的 Agent 对话和子 Agent。`AGENT_ENGINE=eino` 明确启用；`AGENT_ENGINE=legacy` 临时回退旧循环。普通聊天的非工具路径不受这个开关控制。

## 接入边界

- 模型从现有端点注册表构造 Eino `ToolCallingChatModel`。DeepSeek 的 `deepseek-chat`、`deepseek-reasoner` 用原生 Eino DeepSeek 组件；需要图片输入的 `deepseek-flash` 等继续使用 Eino 的 OpenAI 兼容组件。
- 工具通过 `internal/agent/eino.go` 的适配层进入 Eino ToolsNode。原有工具实现及其模式、工作区外路径和确认包装器仍负责权限判断。
- 适配层将工具调用与结果转为现有 `tool_call`、`tool_result` 事件。等待确认或 `ask_user` 时发 `need_input` 并停止本轮；助手流式输出继续发 `delta`，完成时发 `done`。
- 模型上游在任何输出前报暂时性错误时重试一次。收到文本或工具调用分片后不重放本轮，以免重复执行工具。
- 工具调用次数、结果未知时的副作用保护、修改后验证提醒和上下文压缩沿用 Agent 层规则。

## 切换与诊断

自主执行模式先自行选择常规方案、命名与实现细节。`ask_user` 只有提供阻碍类型、原因和已尝试的方法后才可暂停；风险确认模式继续按工具确认。发送与重新生成请求可带 `execution_mode`（plan/confirm/auto），明确覆盖助手预设；前端始终发送底部下拉框的当前选择。

每阶段默认允许 32 轮模型调用、96 次工具调用，分别用 `AGENT_MAX_ITERATIONS` 和 `AGENT_MAX_TOOL_CALLS` 调整。自主模式使用 `AGENT_LIMIT_RECOVERY_ROUNDS=2` 自动续行：达到阶段预算时保留当前工具历史，注入重新规划提醒后继续，最多共 96 轮模型调用、288 次工具调用。设为 0 可关闭续行，最多可设 8。Eino 与 legacy 使用相同预算规则。其它模式仍按配置的严格上限执行。

所有续行预算用完后，运行器保留每个工具请求对应的执行/未执行结果，再给模型一次不带工具的机会整理进度。收尾模型不可用时显示实际工具统计。HTTP 以 `done {incomplete:true, reason:"execution_limit"}` 收尾，后台子任务仍保留触顶失败状态。用户取消、运行超时、权限和结果未知的副作用保护不被续行绕过。续行与提示不保证任何模型都能完成任务，需查看具体工具结果。

修改 `.env` 中的 `AGENT_ENGINE` 后重启后端或桌面应用。出现模型 502/503 时，先核对是否有 `tool_result`；若工具已成功，错误可能来自下一轮模型整理答复，而非工具本身。回退旧循环可用于对照事件轨迹。真正的供应商服务是否可用仍取决于该服务的状态。

## 共享实现

`internal/agent/tool_state.go` 统一两引擎的工具重试、未知副作用保护与修改后验证计账；`model_retry.go` 统一可取消的模型重试等待。各引擎继续负责自身的流式适配、事件、工具图片和预算，避免在共享层引入供应商差异。
