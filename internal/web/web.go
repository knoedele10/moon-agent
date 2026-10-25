package web

import (
	"blog-my/internal/agent"
	"blog-my/internal/agent/toolkit"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// registry 保存 Setup 注入的端点/适配器注册表，供各 handler 使用。
var registry *llm.Registry

func Setup(r *gin.Engine, reg *llm.Registry) {
	registry = reg

	// 把 agent 主循环的模型调用缝装到真实注册表上（契约 v1.1 §A）。
	//
	// **必须在 Setup 里装，不能靠调用方记得装**：agent.Streamer() 未安装时主循环直接
	// 返回 ErrNoStreamer，而那条错误只有真跑一次 tools=true 才会暴露——
	// 单测里测试自己会装，所以"测试全绿但生产走不通"这种缺口单测抓不到
	// （实际上就漏过一次，由 cua-ui 的真机端到端发现）。
	agent.SetStreamer(agent.RegistryStreamer(reg))

	// 只读 SQL 工具需要数据库句柄；取不到就保持未配置（工具会明确报错，而不是 panic）。
	if model.DB != nil {
		if sqlDB, err := model.DB.DB(); err == nil {
			toolkit.SetSQLDB(sqlDB)
		}
	}

	// 测试缝：默认关闭，只有设了 CUA_LLM_UPSTREAM 才把代理上游指到桩。
	cuaApplyUpstreamOverride(reg)
	// 把 CUA 子进程的取舍写进启动日志（sidecar 优先，其次 python 回落）。
	cuaLogCommandChoice()

	r.GET("/", authRequired, pageIndex)
	r.GET("/login", pageLogin)
	r.POST("/login", postLogin)
	r.GET("/register", pageRegister)
	r.POST("/register", postRegister)
	r.GET("/logout", logout)
	r.GET("/settings", authRequired, pageSettings)

	// 探活不鉴权：健康检查要在未登录时也能被探到（与 docs/api-contract.md 一致）。
	r.GET("/api/health", health)

	// CUA 的模型代理：子进程没有 session cookie，改用它自己的每 run Bearer token（契约 3）。
	r.POST("/internal/cua/llm/v1/chat/completions", cuaLLMProxy)

	api := r.Group("/api", authRequired)
	{
		api.GET("/models", listModels)

		api.GET("/conversations", listConversations)
		api.POST("/conversations", createConversation)
		api.GET("/conversations/:id/messages", listMessages)
		// 子 agent 的运行状态：前端靠它轮询刷新子任务卡片（SSE 只负责实时那一段）。
		api.GET("/conversations/:id/subagents", listSubagents)
		// 团队面板（roster + 任务板）：前端跟着子 agent 轮询一起拉。
		api.GET("/conversations/:id/team", listTeam)
		api.DELETE("/conversations/:id", deleteConversation)

		// 会话/消息的编辑能力（对应界面上的重命名、重新生成、导出、编辑与删除）：
		//   PATCH  会话           -> 重命名
		//   POST   :id/regenerate -> 摸掉上一次回答并重新生成（SSE，与 /api/chat 同事件）
		//   GET    :id/export     -> 导出 Markdown
		//   PATCH  消息           -> 改这条 user 消息并截断其后所有消息
		//   DELETE 消息           -> 只删这一条
		api.PATCH("/conversations/:id", patchConversation)
		api.POST("/conversations/:id/regenerate", regenerate)
		api.GET("/conversations/:id/export", exportConversation)
		api.PATCH("/messages/:id", patchMessage)
		api.DELETE("/messages/:id", deleteMessage)

		api.POST("/chat", postChat)

		// 工作区设置：用户自己指定文件工具的沙箱根目录（契约外的新增能力）。
		api.GET("/agent/settings", getAgentSettings)
		api.PUT("/agent/settings", putAgentSettings)
		// 当前会话的任务计划（内存态）：前端在打开会话时恢复计划面板。
		api.GET("/agent/plan", getAgentPlan)
		// 关闭页面时补齐记忆（sendBeacon 调用，幂等）。
		api.POST("/agent/flush-memory", postFlushMemory)

		// 技能管理：渐进式披露（提示词只放名字+简介，正文用 skill_read 按需取）。
		api.GET("/skills", listSkills)
		api.POST("/skills", createSkill)
		// 批量导入：把一个目录下的 SKILL.md 全部导进来（幂等，可重复跑）。
		api.POST("/skills/import", importSkills)
		// 技能检索预演：看一句话会命中哪些技能（召回 top20 → 重排 top5）。
		api.POST("/skills/recall", recallSkillsAPI)
		api.GET("/skills/:id", getSkill)
		api.PATCH("/skills/:id", updateSkill)
		api.DELETE("/skills/:id", deleteSkill)

		// 本地多模态 RAG 知识库（Day 23）：文本与图片走同一条检索链。
		api.GET("/kb/documents", kbListDocuments)
		api.POST("/kb/documents", kbUploadDocument)
		api.POST("/kb/text", kbCreateText)
		api.GET("/kb/documents/:id", kbGetDocument)
		api.DELETE("/kb/documents/:id", kbDeleteDocument)
		api.GET("/kb/stats", kbStats)
		api.POST("/kb/search", kbSearch)
		api.POST("/kb/ask", kbAsk)

		// CUA 电脑操控：进程管家 + 事件转发（契约 2）。
		api.GET("/cua/status", cuaStatus)
		api.GET("/cua/runs", cuaListRuns)
		api.POST("/cua/runs", cuaCreateRun)
		api.GET("/cua/runs/:id/events", cuaRunEvents)
		api.POST("/cua/runs/:id/stop", cuaStopRun)

		api.GET("/keys", listKeys)
		// 自我进化（对标 Hermes）：总览 / 技能账本 / 手动固化 / 工具目录。
		// AGENT_EVOLVE 未启用时 read 接口如实返回 enabled=false，curate 返回 409。
		api.GET("/agent/evolution", getEvolutionStatus)
		api.GET("/agent/evolution/skills", getEvolutionSkills)
		api.POST("/agent/evolution/curate", postEvolutionCurate)
		api.GET("/agent/evolution/journal", getEvolutionJournal)
		api.POST("/agent/evolution/rollback", postEvolutionRollback)
		api.GET("/agent/tool-catalog", getToolCatalog)
		// 自定义助手：一个助手 = 一套「人格 + 默认模型 + 档位 + 工作区」。
		// 字段留空表示继承用户全局设置（见 model/assistant.go 的语义说明）。
		api.GET("/assistants", listAssistants)
		api.POST("/assistants", createAssistant)
		api.PATCH("/assistants/:id", updateAssistant)
		api.DELETE("/assistants/:id", deleteAssistant)

		// 提示词库：把常用指令存成可复用模板。
		api.GET("/prompts", listPrompts)
		api.POST("/prompts", createPrompt)
		api.PATCH("/prompts/:id", updatePrompt)
		api.DELETE("/prompts/:id", deletePrompt)
		// 主动聊天：后台调度按策略决定要不要主动找用户说话（借鉴 Cyrene 的策略状态机）。
		// 调度器由 proactive 包自己幂等挂起，这里只负责暴露接口。
		api.GET("/proactive/status", proactiveStatus)
		api.PUT("/proactive/settings", proactiveSettings)
		api.POST("/proactive/run-now", proactiveRunNow)
		api.GET("/proactive/messages", proactiveMessages)
		// 自媒体内容：排期到点自动导出成品包并推进台账（不代替用户发平台）。
		// 调度器由 app/server.go 启动，这里只暴露「看状态 / 立刻跑一轮」。
		api.GET("/content/jobs", contentJobsStatus)
		api.POST("/content/jobs/run", contentJobsRun)
		// 定时唤醒：到点让 agent 在这个会话里自己跑一轮（一次性、可取消、有台账）。
		api.GET("/wakeups", listWakeupsAPI)
		api.POST("/wakeups", createWakeupAPI)
		api.DELETE("/wakeups/:id", cancelWakeupAPI)
		api.POST("/keys", upsertKey)
		api.DELETE("/keys/:id", deleteKey)
	}
}
