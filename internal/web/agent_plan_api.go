package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 任务计划查询接口。
//
// 计划本身只活在进程内（internal/agent/plan.go），这里只负责把「当前会话的计划」
// 按用户边界读出来给前端。会话不存在或不属于当前用户时一律 404，不泄露存在性。
//
// GET /api/agent/plan?conversation_id=123
func getAgentPlan(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	raw := strings.TrimSpace(c.Query("conversation_id"))
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id 不合法"})
		return
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", id, userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	p, ok := agent.CurrentPlan(strconv.FormatUint(uint64(conv.Id), 10))
	if !ok || len(p.Steps) == 0 {
		// 空计划不是错误：前端据此隐藏面板。
		c.JSON(http.StatusOK, gin.H{"steps": []agent.PlanStep{}, "explanation": ""})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"explanation": p.Explanation,
		"steps":       p.Steps,
		"updated_at":  p.UpdatedAt.Format(time.RFC3339),
	})
}
