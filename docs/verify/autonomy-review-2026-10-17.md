# 自主执行复查（2026-10-17）

本次检查覆盖自主模式、自动续行、触顶收尾、模式切换和中文流式输出。

## 发现与修复

1. 旧循环的 `halted` 状态定义在单次工具调用内部，导致上一步非幂等操作超时、结果未知时，后续移动等操作仍可能执行。新增跨批次、跨续行测试先复现 `delete=1 move=2 read=2`，再将状态保持到本次运行结束。修复后两个引擎均只执行一次删除尝试、两次只读检查，移动执行次数为零。
2. 前端忽略 `done` 中的 `incomplete` 字段。新增真实页面脚本的 jsdom 检查先复现该字段丢失，再传递 SSE 收尾数据；未完成的活动摘要显示“执行已暂停”，之后正常运行清除暂停标记。

## 已通过的验证

- `go test ./agent ./agent/fs ./web -count=1 -timeout 90s`：三个包全部通过，临时目录和 Go 缓存均设在项目 `build/` 下。
- Agent 用例：两个引擎在阶段上限后继续完成；最终上限不重复执行工具；一批多个工具仍保持请求与结果一一对应；收尾模型不可用时保留实际执行统计；用户取消仍生效；结果未知的副作用保护跨续行保留。
- `TestAutonomousHTTPExecution`：真实 MySQL 与模拟模型通过四个接口场景：拒绝普通提问后自行写入并读回中文文件、最终触顶输出进度、显式确认模式覆盖自动助手预设、必要的登录提问暂停等待真实用户。
- `TestPermHTTPMatrix`：三种模式的 11 项权限场景全部通过，包括工作区内外写入、确认重放、外部相对路径拒绝和无工作区。
- `node scripts/verify-autonomy-ui.cjs`：真实页面脚本在 jsdom 中验证底部模式随发送和重新生成提交、七字节切片的 UTF-8 SSE 保留中文和未完成标记、重新生成后清除暂停状态。
- `node scripts/verify-agent-activity.cjs`：处理过程折叠、工具结果与记忆展开通过。

## 本机真实模型证据及范围

启动本机 Ollama 后，`TestAutonomousLocalModel` 使用实际安装的 `qwen3:4b`、Eino 运行器和项目的自主工具策略执行文件任务。测试仅提供 `read/write/list/ask_user` 四个工具和简化提示词，不加载完整画像、灵魂与其他工具。

结果：`write ok=true`、`read ok=true`，文件内容精确匹配“自主执行验证：你好，世界。”，没有等待用户输入，正常结束。用例约 66 秒，包含本次模型加载与推理。该结果证明本地模型在此文件任务上的实际执行能力，不代表复杂任务或完整工具集合的成功率。

当前 Ollama 模型列表没有 `qwen3.5:4b`，本次未验证该模型。

## 可复跑入口

常规接口检查需要本机 MySQL，可设置 `AGENT_IT=1`，执行 `go test ./web -run 'TestAutonomousHTTPExecution|TestPermHTTPMatrix' -count=1 -v`。关闭测试自动记忆可用 `AGENT_AUTO_MEMORY=0`。

本机模型检查默认跳过，需显式设置 `AGENT_LIVE_MODEL=qwen3:4b`、`OLLAMA_BASE_URL=http://127.0.0.1:11434`，执行 `go test ./web -run '^TestAutonomousLocalModel$' -count=1 -v -timeout 170s`。先确认 Ollama 已启动且该模型已安装。

Windows 受限环境请设置 `TEMP`、`TMP` 为项目的 `build/tmp`，`GOCACHE` 为 `build/gocache`，`GOTMPDIR` 为 `build/gotmp`。未设置时，系统沙箱临时目录可能导致写文件验证出现访问拒绝。
