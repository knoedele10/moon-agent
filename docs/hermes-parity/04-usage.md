# 工具调用与学习能力使用指南

本轮参考 [NousResearch/Hermes Agent](https://github.com/NousResearch/hermes-agent) 的工具目录、学习提示、轨迹与技能管理机制，按本项目 Go/Electron 结构实现。

## 启用

在实际使用的 `.env` 中设置并重启服务：

```dotenv
AGENT_TOOL_DISCLOSURE=search
AGENT_TOOL_SPILL_BYTES=32768
AGENT_SKILL_LINT=block
AGENT_EVOLVE=auto
```

开发版读取项目 `.env`；已安装桌面版读取 Electron 用户配置目录中的 `.env`。保留原来的 `APP_SECRET` 和数据库配置。

`record` 仅记录，`suggest` 生成建议，`auto` 在本轮权限允许时自动固化。新安装不配置时默认关闭。当前 Eino 执行器支持按需发现；只读并发仅 legacy 支持，可设 `AGENT_TOOL_PARALLEL=read`。

## 日常使用

- 在辅助工作或代码协作用途中处理实际任务，自主档位可在证据充分时积累技能。计划、待确认及受限子级不自动修改技能。
- 在「技能 → 学习记录与工具能力」查看状态和报告，分析最近任务，采用建议或撤销最近一次自动学习。页面打开本身不修改技能。
- 问「你上次怎么完成这个任务」，助手可调用 `session_search` 搜索本人会话原文，再通过会话编号继续查看。结果有界且保留消息定位信息。
- `tool_search` 搜索并激活可用工具，下一次模型请求即获得其参数。工具选择仍受当前用途与权限控制。
- 大结果超过阈值后保存在用户目录；`read_tool_result` 可按行读取，长单行用返回的 `next_char_offset` 继续，同一会话内读回完整内容。

技能自动复用沿用原来的混合检索、可选重排和正文注入。它提供任务说明，不会偷偷执行技能里附带的脚本；脚本执行继续走 `skill_run` 和原有权限。

## 外部工具

MCP 默认关闭，配置 `AGENT_MCP_CONFIG` 或 `AGENT_MCP_JSON` 后才启动指定服务器。仅支持已实现的 stdio 传输；HTTP/SSE MCP 不在本轮范围。服务器配置由本机主人管理。

工具集示例：

```dotenv
AGENT_TOOLSETS=fs,memory,skill,exec,agent,team,evolve,mcp
```

不配置则保留全部可用工具集；移除 `mcp` 可禁止装配外部服务。基础工具始终保留。

## 数据和接口

默认数据位于用户配置目录下的 `blog-my/evolve`；可用 `AGENT_EVOLVE_DIR` 指定基目录。其下 `users/<用户哈希>/` 保存记忆、轨迹、技能账本、学习报告和变更日志；`spill/` 里的结果还校验会话归属。停止学习将 `AGENT_EVOLVE=off`，已保存的技能仍可在原技能管理页启停。

接口均需要登录：

| 方法与路径 | 作用 |
| --- | --- |
| `GET /api/agent/evolution` | 当前用户的状态与最近报告 |
| `GET /api/agent/evolution/skills` | 技能体检与使用统计 |
| `GET /api/agent/evolution/journal` | 最近修改前后记录 |
| `POST /api/agent/evolution/curate` | `{"apply":false}` 分析；主账号可显式设为 `true` 采用 |
| `POST /api/agent/evolution/rollback` | 主账号撤销最近一批自动改动，遇后续编辑冲突则拒绝覆盖 |
| `GET /api/agent/tool-catalog` | 工具集、发现、结果存储和 MCP 状态 |

最终验证范围见 [本轮集成验收](../verify/hermes-integration-2026-10-21.md)。
