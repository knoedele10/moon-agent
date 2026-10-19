# 独立示例

- `mock-web/`：原 deploy，独立 Node mock 站点。从此目录运行 `npm start`，不连接 Go Agent 后端和桌面 MySQL；端口由其 server.js 与环境配置决定。
- `cloudbase/`：原 cloudbase，课程用健康检查云函数。

示例保留原数据与资源，未接入桌面打包。真实应用从仓库根运行 `npm run dev`。旧文档中 deploy/ 与 cloudbase/ 的位置对应这里的两个子目录。
