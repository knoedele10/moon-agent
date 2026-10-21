# 03 MCP 客户端（工具侧扩展）

> 对应 `internal/agent/mcps/**` + `internal/web/hermes_mcp.go`。
> 目的：让 Moon Agent 能用**外部 MCP 服务器**提供的工具，而不改动主循环、工具集、落盘、并发、审批任何一层。

## 1. 定位

Hermes 的扩展性有两半：内置工具 registry（`tools/registry.py`）与外部连接器（`tools/connectors/`、`tools/mcp_tool_*.py`）。
本仓库已经对齐了前半（`internal/agent/toolsys`），MCP 客户端补的是后半：

| 层 | 现状 |
| --- | --- |
| 工具发现 | `tools/list`（含 `nextCursor` 分页，最多 20 页） |
| 工具调用 | `tools/call`，`content[]` 折成文本；`image` 折成 `Result.Image`；`isError` 直接透传 |
| 命名 | `mcp__<server>__<tool>`，非法字符归一化成 `_`，超长截断 |
| 风险 | 一律 `riskyTool = true`：外部进程能力边界不在我们手里，`confirm` 档先问 |
| 并发 | **不进** `readOnlyToolNames`：默认串行，避免「第三方工具被当只读」 |
| 落盘 | 走既有 `toolsys` spill 包装：MCP 大结果同样落盘 + `read_tool_result` 分段读 |
| 生命周期 | 进程级一个 Manager：首次对话连接、之后复用、进程退出时关闭 |
| 传输 | 只实现 stdio（newline-delimited JSON-RPC 2.0）；HTTP 留 `Transport` 接口 |

## 2. 配置

两条来源，二选一，都不配就**不启动任何进程**：

```powershell
# 文件
$env:AGENT_MCP_CONFIG = 'E:\blog-my\mcp.json'
# 或内联 JSON（适合容器/临时试验）
$env:AGENT_MCP_JSON = '{"servers":[{"name":"fs","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","E:\\tmp"]}]}'
```

文件格式：

```json
{
  "servers": [
    {
      "name": "fs",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "E:\\tmp"],
      "env": { "DEBUG": "0" },
      "dir": "E:\\blog-my",
      "disabled": false,
      "initTimeoutSeconds": 20,
      "callTimeoutSeconds": 120
    }
  ]
}
```

校验规则（`mcps.ParseConfig`）：`name` 必填且唯一；启用项必须有 `command`；`disabled: true` 的项不启动但保留在状态里。

## 3. 运行时行为

- 连接：`initialize` → `notifications/initialized` → `tools/list`。单个服务器失败**不致命**，记录进状态后继续连下一个。
- 调用：`tools/call`；失败返回 `IsError=true` 的结果（与内置工具同一契约：失败不中断循环，模型自己决定下一步）。
- 超时：握手默认 20s、调用默认 120s；超时后该次调用立即返回错误，不挂死对话。
- 进程：`Close()` 先关 stdin（MCP 的正常退出信号），3s 内没退再 kill；stderr 保留最后 8KB，连不上时带进错误信息。
- 观测：`GET /api/agent/evolution` 的 `mcp` 字段返回 `summary` 与逐服务器状态（名字/版本/工具数/错误）。该接口**不触发连接**。

## 4. 验收

```powershell
# 1) 单测（不依赖 node/python，用 net.Pipe 假服务器）
go test ./internal/agent/mcps -count=1 -v

# 2) 接线：默认关闭
go test ./internal/web -run MCP -count=1 -v

# 3) 手工冒烟（需要 node）：
#    写一个 mcp.json，起服务，问一句「用 mcp__demo__echo 回显 hello」，确认：
#    - 工具出现在工具清单里
#    - confirm 档会先问一次（风险工具）
#    - 返回文本进入 tool 消息，事件顺序不变
```

## 5. 未做（明确边界）

- **HTTP/SSE 传输**：接口已留（`Transport`），实现与测试未写。
- **resources / prompts / sampling**：只做 tools，不做 MCP 的另外三件套。
- **工具描述动态刷新**：`notifications/tools/list_changed` 已能收到并回调，但当前没有重载工具表的策略（需要重启进程）。
- **`tool_describe` 第二桥接**：MCP 工具数量少时不需要；工具过百再加（见 01-tool-calling.md §建议）。
