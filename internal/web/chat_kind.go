package web

import (
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 会话用途（四个模式）。设计参考 Cyrene-Agent 的 Chat / Work / Code / Learn：
// **模式决定"在干什么"**（给不给工具、用什么语气、记忆算哪一类），
// 与 agent_mode（plan/confirm/auto，决定"能不能放手做"）是两个正交维度。
//
// 为什么做成表而不是散在各处的 if：四个模式 × （工具集 / 提示词 / 记忆 scope）是
// 3 个维度，写散了以后加第五个模式必然漏改。这里一处定义，别处只查表。

const (
	KindChat  = "chat"  // 日常聊天 / 陪伴
	KindWork  = "work"  // 辅助工作（默认）
	KindCode  = "code"  // 代码协作
	KindLearn = "learn" // 学习陪伴
)

// normalizeKind 把任意输入规整成四个合法值之一。
// **空串按 work 处理**——升级前建的会话都是工作型，行为必须不变。
func normalizeKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case KindChat:
		return KindChat
	case KindCode:
		return KindCode
	case KindLearn:
		return KindLearn
	default:
		return KindWork
	}
}

// kindProfile 是一个会话类型的完整行为描述。
type kindProfile struct {
	Label string
	// Tools 是**工具白名单**；nil 表示"全部允许"（工作模式）。
	Tools map[string]bool
	// MemoryScope 决定这个模式下的记忆算哪一类（rag 的 scope 字段）。
	MemoryScope string
	// Prompt 追加在系统提示词里，说明这个模式该怎么做。
	Prompt string
}

// 记忆 scope 的取值与 rag 的约定一致：life（生活/个人）、work（项目/技术）、""（未分类）。
const (
	scopeLife = "life"
	scopeWork = "work"
)

var kindProfiles = map[string]kindProfile{
	// 陪伴：**不给文件、网页、SQL、Office 工具**（对应 Cyrene 的"不暴露任何工具"）。
	// 只留三样：记忆（陪你就是记住你）、时间、计算器——这三个不碰用户的文件与外部服务。
	KindChat: {
		Label:       "日常聊天",
		MemoryScope: scopeLife,
		Tools: map[string]bool{
			"memory_search": true, "memory_write": true, "soul_evolve": true,
			"datetime": true, "calculator": true, "ask_user": true,
			// 「明早提醒我」在陪伴场景里最常见，所以聊天模式也给唤醒三件套。
			"schedule_wakeup": true, "list_wakeups": true, "cancel_wakeup": true,
		},
		Prompt: "\n【当前模式：日常聊天】这是一次陪伴性质的对话，不是工作任务。" +
			"不要主动把话题引向工作，也不要提议去改文件或跑命令；语气自然、简短、像熟人聊天。" +
			"你这次没有文件和联网工具，需要那些能力时请提醒用户切到「辅助工作」模式。\n",
	},

	// 工作：全套工具，行为与改造前一致（Tools 为 nil = 全部允许）。
	KindWork: {
		Label:       "辅助工作",
		MemoryScope: scopeWork,
		Tools:       nil,
		Prompt: "\n【当前模式：辅助工作】这是一次通用任务会话，你可以调用全部工具把事情做完。" +
			"动手前先把要改什么说清楚；完成之后告诉用户你做了什么、结果如何。\n",
	},

	// 代码协作：给文件读写改 + 技能 + 记忆；**不给**联网搜索、SQL、Office。
	// 安全边界仍然是工作区沙箱（文件工具只能在工作区根目录里动），
	// 所以这里不需要额外收紧写权限——用户把代码目录设成工作区即可。
	KindCode: {
		Label:       "代码协作",
		MemoryScope: scopeWork,
		Tools: map[string]bool{
			"read": true, "list": true, "read_image": true, "grep": true, "glob": true,
			"restore": true, "restore_turn": true,
			"write": true, "edit": true, "move": true, "delete": true,
			"json_format": true, "calculator": true, "datetime": true, "ask_user": true, "update_plan": true,
			"memory_search": true, "memory_write": true, "soul_evolve": true,
			"skill_search": true, "skill_read": true, "skill_run": true, "skill_write": true,
			"run_command": true,
			"git_status":  true, "git_diff": true, "git_log": true, "git_show": true, "git_add": true, "git_commit": true,
			"init_agents_md": true,
			"subagent": true, "list_subagents": true, "spawn_teammate": true,
			"send_message": true, "list_agents": true, "wait_agent": true,
			"team_task_create": true, "team_task_list": true, "team_task_get": true, "team_task_update": true,
			"schedule_wakeup": true, "list_wakeups": true, "cancel_wakeup": true,
		},
		Prompt: "\n【当前模式：代码协作】这是改代码的会话。" +
			"规矩：**改之前先读**（不读就改会被工具拒绝）；优先用 edit 做定点修改，只有整体重写才用 write；" +
			"可在工作区用 run_command 运行 npm/npx/go/node/python 构建命令；改完要告诉用户**怎么验证**（跑什么命令、看什么现象），不要只说「改好了」。\n",
	},

	// 学习陪伴：能读、能写笔记（写仍然受工作区沙箱约束），但**不给删除/移动**——
	// 学习时不该有删东西的能力。
	KindLearn: {
		Label:       "学习陪伴",
		MemoryScope: scopeLife,
		Tools: map[string]bool{
			"read": true, "list": true, "read_image": true, "grep": true, "glob": true,
			"restore": true, "restore_turn": true,
			"write": true, "edit": true,
			"json_format": true, "calculator": true, "datetime": true, "ask_user": true, "update_plan": true,
			"memory_search": true, "memory_write": true, "soul_evolve": true,
			"skill_search": true, "skill_read": true,
			"subagent": true, "list_subagents": true, "spawn_teammate": true,
			"send_message": true, "list_agents": true, "wait_agent": true,
			"team_task_create": true, "team_task_list": true, "team_task_get": true, "team_task_update": true,
			"schedule_wakeup": true, "list_wakeups": true, "cancel_wakeup": true,
		},
		Prompt: "\n【当前模式：学习陪伴】这是陪用户学东西的会话，不是替他做完。" +
			"流程：先弄清材料在讲什么 → 用提问检查他是否真的懂了 → 帮他把要点整理成笔记 → 记住他的进度。" +
			"不要直接给答案，先给线索；他卡住再逐步展开。你这次没有删除和移动文件的能力。\n",
	},
}

// profileFor 取某个模式的行为描述；未知值回落 work。
func profileFor(kind string) kindProfile {
	if p, ok := kindProfiles[normalizeKind(kind)]; ok {
		return p
	}
	return kindProfiles[KindWork]
}

// filterToolsByKind 按模式白名单过滤工具集。
// 工作模式（Tools 为 nil）原样返回，行为与改造前**逐字节一致**。
func filterToolsByKind(tools []agent.Tool, kind string) []agent.Tool {
	p := profileFor(kind)
	if p.Tools == nil {
		return tools
	}
	out := make([]agent.Tool, 0, len(tools))
	for _, t := range tools {
		if p.Tools[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 请求级传递（与助手、对话参数同一套做法：走 gin.Context，不改函数签名）
// ---------------------------------------------------------------------------

// kindRootFor 取某个模式的**专属工作区**（代码目录 / 笔记目录）；没设返回空串。
// 优先级：模式专属目录 > 通用工作区（FSRoot）> 环境变量 AGENT_FS_ROOT。
// 两个专属字段都为空时回落，所以默认行为与升级前一致。
func kindRootFor(userID, kind string) string {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return ""
	}
	var s model.AgentSetting
	if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err != nil {
		return ""
	}
	switch normalizeKind(kind) {
	case KindCode:
		return strings.TrimSpace(s.CodeRoot)
	case KindLearn:
		return strings.TrimSpace(s.LearnRoot)
	}
	return ""
}

func kindWorkspaceRootsFor(userID, kind string, base []string) []string {
	primary := kindRootFor(userID, kind)
	if primary == "" {
		return cleanWorkspaceRoots(base)
	}
	out := []string{primary}
	for _, root := range base {
		if !strings.EqualFold(strings.TrimRight(root, `\\/`), strings.TrimRight(primary, `\\/`)) {
			out = append(out, root)
		}
	}
	return cleanWorkspaceRoots(out)
}

const ctxConversationKindKey = "ctxConversationKind"

func setConversationKind(c *gin.Context, kind string) {
	if c == nil {
		return
	}
	c.Set(ctxConversationKindKey, normalizeKind(kind))
}

// conversationKindFromContext 取本轮会话模式；没设置就是 work。
func conversationKindFromContext(c *gin.Context) string {
	if c == nil {
		return KindWork
	}
	v, ok := c.Get(ctxConversationKindKey)
	if !ok {
		return KindWork
	}
	s, _ := v.(string)
	return normalizeKind(s)
}
