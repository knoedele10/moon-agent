package web

import (
	"strings"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 助手（Assistant）与本轮对话的接线。
//
// 一个助手 = 用户预设的一套「人格 + 默认模型 + 档位 + 工作区」。它**只覆盖自己显式填了的字段**：
// 留空表示「继承用户全局设置」，所以用户改全局设置时，没覆盖过的助手会跟着变。
// 这条语义必须在三个地方保持一致：前端表单、这里、以及 model/assistant.go 的注释。

// ctxAssistantIDKey 是本轮请求携带的助手 id 在 gin.Context 里的键。
//
// 为什么走 Context 而不是给 runAgentChat 加参数：那条函数已经有 7 个参数，且被
// chat.go / chat_edit.go / 多个测试同时调用；加一个参数要改一圈签名，而助手只影响
// 「这轮用哪套配置」，放在请求上下文里更贴切，也不会改动既有调用点。
const ctxAssistantIDKey = "ctxAssistantID"

// assistantWorkspaceOverride makes an assistant's explicit directory primary
// while retaining the user's other active directories for @workspaceN access.
func assistantWorkspaceOverride(primary string, current assistantContext) assistantContext {
	all := cleanWorkspaceRoots(append([]string{primary, current.Root}, current.Roots...))
	if len(all) == 0 {
		return current
	}
	current.Root = all[0]
	current.Roots = append([]string(nil), all[1:]...)
	return current
}

// assistantContext 是本轮真正生效的 agent 配置（全局设置 + 助手覆盖之后的结果）。
type assistantContext struct {
	Root            string   // 文件工具的工作区根目录
	Roots           []string // 其余已启用工作区目录
	Mode            string   // plan / confirm / auto
	SoulPersonality string   // 人格
	SoulGrowth      string   // 助手通过自省形成的有限人格调整
	SoulVoice       string   // 说话方式
	SoulWork        string   // 做事方式
}

// resolveAssistantContext 算出本轮生效的配置。
//
// assistantID 为 0、助手不存在、或助手不属于这个用户时，一律回落成「用户全局设置」——
// 助手是增强，绝不能因为一个坏 id 让整轮对话失败。
func resolveAssistantContext(userID string, assistantID uint) assistantContext {
	ctx := assistantContext{
		Root: agentFSRootFor(userID),
		Roots: func() []string {
			all := agentFSRootsFor(userID)
			if len(all) > 1 {
				return append([]string(nil), all[1:]...)
			}
			return nil
		}(),
		Mode: agentModeFor(userID),
	}

	if model.DB != nil && strings.TrimSpace(userID) != "" {
		var s model.AgentSetting
		if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err == nil {
			ctx.SoulPersonality = strings.TrimSpace(s.SoulPersonality)
			ctx.SoulGrowth = s.SoulGrowth
			ctx.SoulVoice = strings.TrimSpace(s.SoulVoice)
			ctx.SoulWork = strings.TrimSpace(s.SoulWork)
		}
	}

	if assistantID == 0 || model.DB == nil || strings.TrimSpace(userID) == "" {
		return ctx
	}

	// 查询带上 user_id：否则填一个别人的助手 id 就能套用别人的配置。
	var a model.AgentAssistant
	if err := model.DB.Where("id = ? AND user_id = ?", assistantID, userID).First(&a).Error; err != nil {
		return ctx
	}

	// 只覆盖「助手里真填了」的字段。空串 = 继承，不动全局值。
	if v := strings.TrimSpace(a.FSRoot); v != "" {
		// 助手的专属目录成为 workspace1，但不能因此丢掉用户全局启用的
		// 其他活动工作区；否则模型仍会被告知可用 @workspace2，却在工具侧
		// 只拿到一个根目录，最终把该路径误当成技能目录里的普通路径。
		ctx = assistantWorkspaceOverride(v, ctx)
	}
	if v := strings.TrimSpace(a.Mode); v != "" {
		ctx.Mode = normalizeMode(v)
	}
	if v := strings.TrimSpace(a.SoulPersonality); v != "" {
		ctx.SoulPersonality = v
	}
	if strings.TrimSpace(a.SoulGrowth) != "" {
		ctx.SoulGrowth = a.SoulGrowth
	}
	if v := strings.TrimSpace(a.SoulVoice); v != "" {
		ctx.SoulVoice = v
	}
	if v := strings.TrimSpace(a.SoulWork); v != "" {
		ctx.SoulWork = v
	}
	return ctx
}

// setAssistantID 由 handler 在进入 runAgentChat 之前调用，把本轮选中的助手记进请求上下文。
func setAssistantID(c *gin.Context, id uint) {
	if c == nil {
		return
	}
	c.Set(ctxAssistantIDKey, id)
}

// assistantIDFromContext 取本轮请求携带的助手 id；没有就是 0（不使用助手）。
func assistantIDFromContext(c *gin.Context) uint {
	if c == nil {
		return 0
	}
	v, ok := c.Get(ctxAssistantIDKey)
	if !ok {
		return 0
	}
	id, _ := v.(uint)
	return id
}
