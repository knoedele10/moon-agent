package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"blog-my/internal/agent/evolve"

	"github.com/gin-gonic/gin"
)

// 本文件暴露「自我进化」的 HTTP 接口。全部只读或显式触发，
// 不会因为页面打开就偷偷改技能库。

// getEvolutionStatus 返回进化总览：档位、目录、技能数、风险技能、最近一次固化报告。
func getEvolutionStatus(c *gin.Context) {
	userID := currentUserID(c)
	evo := newHermesEvolve(userID)
	out := hermesEvolveReport(evo)
	if evo != nil && isOwnerUser(userID) {
		out["dir"] = evo.dir
	}
	out["toolset"] = selectedToolsets()
	out["disclosure"] = toolDisclosureMode()
	out["lint"] = skillLintMode()
	out["can_apply"] = evo != nil && isOwnerUser(userID)
	if evo != nil {
		if stats, err := evo.ledger.Stats(); err == nil {
			out["skill_stats"] = len(stats)
		}
		if stats, err := evo.ledger.AtRisk(3, 10); err == nil {
			out["at_risk"] = stats
		}
		if entries, err := evo.memory.All(); err == nil {
			out["memory_entries"] = len(entries)
		}
	}
	c.JSON(http.StatusOK, out)
}

// getEvolutionJournal 只返回当前用户的最近变更；保留修改前后内容以便核对。
func getEvolutionJournal(c *gin.Context) {
	evo := newHermesEvolve(currentUserID(c))
	if evo == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "changes": []any{}})
		return
	}
	entries, err := evolve.NewJournal(evo.dir).List(60)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取学习变更记录失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "changes": entries})
}

func postEvolutionRollback(c *gin.Context) {
	userID := currentUserID(c)
	if !isOwnerUser(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "只有主账号可以撤销自动学习"})
		return
	}
	evo := newHermesEvolve(userID)
	if evo == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "自我进化未启用"})
		return
	}
	actions, err := evolve.RollbackLast(evolve.NewJournal(evo.dir), evo.store, userID)
	if err != nil {
		status := http.StatusConflict
		if !errors.Is(err, evolve.ErrNothingToRollback) && !errors.Is(err, evolve.ErrAlreadyRolledBack) {
			// 冲突或存储错误都应明确提示，不能冒充回滚成功。
			hermesLogf("回滚未完成：%v", err)
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "actions": actions})
}

// getEvolutionSkills 返回技能清单 + 使用账本 + 体检问题（最多 200 条）。
func getEvolutionSkills(c *gin.Context) {
	userID := currentUserID(c)
	evo := newHermesEvolve(userID)
	if evo == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "reason": "AGENT_EVOLVE 未启用"})
		return
	}
	items, err := evo.store.List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取技能库失败：" + err.Error()})
		return
	}
	stats, _ := evo.ledger.Stats()
	statByName := make(map[string]evolve.SkillStat, len(stats))
	for _, st := range stats {
		statByName[st.Name] = st
	}
	type row struct {
		evolve.SkillItem
		Stat   evolve.SkillStat `json:"stat"`
		Issues []evolve.Issue   `json:"issues"`
	}
	if len(items) > 200 {
		items = items[:200]
	}
	rows := make([]row, 0, len(items))
	for _, it := range items {
		rows = append(rows, row{SkillItem: it, Stat: statByName[it.Name], Issues: evolve.Lint(it)})
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": true, "mode": evo.mode,
		"skills":     rows,
		"duplicates": evolve.Duplicates(items, 0.6),
	})
}

// postEvolutionCurate 显式触发一次固化。
//
// 权限：只有主账号可以 apply=true（真的改技能库）；其他人只能拿建议报告。
func postEvolutionCurate(c *gin.Context) {
	userID := currentUserID(c)
	evo := newHermesEvolve(userID)
	if evo == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "AGENT_EVOLVE 未启用，无法固化"})
		return
	}
	var body struct {
		Apply bool `json:"apply"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误，请提供 apply 布尔值"})
		return
	}
	if body.Apply && !isOwnerUser(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "只有主账号可以修改技能库；当前只返回建议报告"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	rep, err := evo.Curate(ctx, body.Apply)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "固化失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "applied": body.Apply, "report": rep})
}

// getToolCatalog 返回工具集清单与环境开关状态（前端「能力」面板用）。
// 它不是某一轮对话的真实工具表：真实工具表还要按会话模式/执行档位过滤。
func getToolCatalog(c *gin.Context) {
	sets := make([]gin.H, 0, len(toolsetNames()))
	for _, name := range toolsetNames() {
		sets = append(sets, gin.H{"name": name, "desc": toolsetDesc(name), "risk": toolsetRisk(name)})
	}
	c.JSON(http.StatusOK, gin.H{
		"selected":   selectedToolsets(),
		"toolsets":   sets,
		"disclosure": toolDisclosureMode(),
		"parallel":   parallelMode(),
		// 配了不等于生效：eino 执行器下强制串行，这里回显实际值。
		"parallel_effective": parallelEffective(),
		"spill":              hermesEnvInt("AGENT_TOOL_SPILL_BYTES"),
		"mcp": gin.H{
			"summary": mcpSummary(),
			"servers": mcpStatus(),
		},
		"env": gin.H{
			"AGENT_ENGINE":          agentEngineName(),
			"AGENT_TOOLSETS":        strings.TrimSpace(os.Getenv("AGENT_TOOLSETS")),
			"AGENT_TOOL_DISCLOSURE": toolDisclosureMode(),
			"AGENT_EVOLVE":          evolveMode(),
			"AGENT_SKILL_LINT":      skillLintMode(),
			"AGENT_MCP_CONFIG":      strings.TrimSpace(os.Getenv("AGENT_MCP_CONFIG")),
		},
	})
}
