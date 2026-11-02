# 17 · QQ 接入（OneBot v11）

让 agent 在 QQ 上收发消息。协议用 OneBot v11 **反向 WebSocket**：NapCat / LLOneBot / Lagrange
等实现登录一个 QQ 号，主动连到本服务的 `/onebot/v11/ws`。本机跑即可，不需要公网域名。

> 用个人号当机器人有被风控的风险，建议用小号。

## 快速开始

1. 安装并登录 [NapCat](https://napneko.github.io/)（或其它 OneBot v11 实现）。
2. 在 NapCat 里新建一个 **WebSocket 客户端**（即反向 WS）：
   - URL：`ws://127.0.0.1:8080/onebot/v11/ws`
   - Token：随便一串长随机字符串
   - 消息格式：`array`（`string` 也支持）
3. `.env`：

   ```env
   QQ_ONEBOT_ENABLED=1
   QQ_ONEBOT_TOKEN=上一步的 token
   QQ_ALLOW_USERS=你的QQ号
   # QQ_ALLOW_GROUPS=群号
   # QQ_BOT_MODEL=deepseek-chat
   ```

4. 重启服务，日志出现 `qq: OneBot 已连接 self_id=...` 即接通。

## 行为

| 场景 | 处理 |
| --- | --- |
| 私聊 | 发信人在 `QQ_ALLOW_USERS` 里就回复。 |
| 群聊 | 群在 `QQ_ALLOW_GROUPS` 里，且消息 **@ 了机器人** 才回复；回复引用原消息。 |
| `/new` | 当前 QQ 聊天换一个新会话（旧会话留在网页里可查）。 |
| `/help` | 帮助。 |
| 上一轮没跑完又来消息 | 直接回「还在处理上一条」，不排队。 |

- 每个 QQ 聊天（`private:<QQ号>` / `group:<群号>`）绑一个本地会话（表 `qq_chat_binding`），
  所以在网页侧边栏也能看到「QQ · 昵称」「QQ 群 …」这些会话。
- 群会话共享上下文，每条消息带说话人：`[昵称(QQ号)]: 内容`。
- 一轮与网页 `/api/chat` 走同一条路径：`AGENT_TOOLS` 开着就走 agent 循环，否则直连模型。
- 回复超过 1800 字按行切成多条发送。只处理文本，图片等非文本段忽略。

## 安全

QQ 上的人不是本机主人，所以默认收得很紧：

- **必须**配 `QQ_ONEBOT_TOKEN`，否则拒绝所有连接（`Authorization: Bearer` 或 `?access_token=`）。
- 白名单为空 = 谁都不回；`*` 才是全部放行。
- 会话模式默认 `chat`：不给文件、网页、SQL、命令等工具。改成 `work` / `code` 前请想清楚——
  群里任何能 @ 到机器人的人都能驱动这些工具。
- 风险操作仍按账号的 agent 档位处理；`confirm` 档下会在 QQ 里问「回复确认」。

## 配置一览

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `QQ_ONEBOT_ENABLED` | 关 | 总开关；关闭时 `/onebot/v11/ws` 返回 404。 |
| `QQ_ONEBOT_TOKEN` | — | 必填，与 OneBot 端一致。 |
| `QQ_ALLOW_USERS` | 空 | 私聊白名单（QQ 号，逗号分隔，`*` = 全部）。 |
| `QQ_ALLOW_GROUPS` | 空 | 群白名单（群号）。 |
| `QQ_BOT_USER_ID` | 本机主账号 | QQ 消息记在哪个本地账号下（用它的 key、设置、记忆）。 |
| `QQ_BOT_MODEL` | 该账号最近用过的模型 | 新会话用的模型。 |
| `QQ_BOT_KIND` | `chat` | 新会话的模式。 |
| `QQ_BOT_TIMEOUT_SECONDS` | 600 | 单轮超时。 |

Docker 部署时 NapCat 在另一个容器里：把 URL 换成 `ws://<本服务容器名>:8080/onebot/v11/ws`，
并让本服务监听 `APP_HOST=0.0.0.0`。
