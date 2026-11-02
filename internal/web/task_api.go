package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 长任务 HTTP 接口：所有查询都带 user_id 过滤，不能跨用户访问。
//
//	GET  /api/tasks?conversation_id=     列表（主对话或执行对话均可过滤）
//	POST /api/tasks                      新建（界面表单；用户本人填写，允许命令验收）
//	GET  /api/tasks/:id                  详情：任务 + 轮次 + 事件
//	POST /api/tasks/:id/:action          pause / resume / cancel / retry / input / review

func taskView(t model.AgentTask, full bool) gin.H {
	h := gin.H{
		"id": t.Id, "title": t.Title, "status": t.Status, "status_label": taskStatusLabel(t.Status),
		"stop_reason": t.StopReason, "goal": t.Goal, "constraints": t.Constraints, "criteria": taskCriteria(t),
		"progress": t.Progress, "done_steps": taskDoneSteps(t), "blocker": t.Blocker, "last_error": t.LastError,
		"main_conversation_id": t.MainConversationID, "exec_conversation_id": t.ExecConversationID,
		"model": t.Model, "mode": t.Mode,
		"budget": gin.H{
			"rounds": t.RoundsUsed, "max_rounds": t.MaxRounds, "tool_calls": t.ToolCallsUsed, "max_tool_calls": t.MaxToolCalls,
			"tokens": t.TokensUsed, "max_tokens": t.MaxTokens, "deadline": t.Deadline,
		},
		"verify_attempts": t.VerifyAttempts, "consecutive_failures": t.ConsecutiveFailures,
		"verification": taskLastVerification(t), "submission": taskLastSubmission(t),
		"revision": t.Revision, "next_run_at": t.NextRunAt, "needs_reconcile": t.NeedsReconcile,
		"created_at": t.CreatedAt, "updated_at": t.UpdatedAt, "finished_at": t.FinishedAt,
		"actions": taskAllowedActions(t.Status),
	}
	if full {
		var rounds []model.AgentTaskRound
		model.DB.Where("task_id = ?", t.Id).Order("id desc").Limit(30).Find(&rounds)
		rv := make([]gin.H, 0, len(rounds))
		for _, r := range rounds {
			rv = append(rv, gin.H{"no": r.No, "trigger": r.Trigger, "status": r.Status, "outcome": r.Outcome,
				"tool_calls": r.ToolCalls, "tokens": r.Tokens, "compacted": r.Compacted, "error": r.ErrText,
				"reply": clipRunes(r.Reply, 600), "started_at": r.StartedAt, "finished_at": r.FinishedAt})
		}
		var events []model.AgentTaskEvent
		model.DB.Where("task_id = ?", t.Id).Order("id desc").Limit(60).Find(&events)
		ev := make([]gin.H, 0, len(events))
		for _, e := range events {
			ev = append(ev, gin.H{"kind": e.Kind, "message": e.Message, "at": e.CreatedAt})
		}
		h["rounds"], h["events"] = rv, ev
		h["pending_messages"] = pendingTaskOutbox(t.Id)
	}
	return h
}

// taskAllowedActions 告诉界面当前可以点哪些按钮。
func taskAllowedActions(status string) []string {
	switch status {
	case model.TaskRunning, model.TaskVerifying:
		return []string{"pause", "cancel"}
	case model.TaskWaitingHuman:
		return []string{"input", "pause", "cancel"}
	case model.TaskReview:
		return []string{"review", "pause", "cancel"}
	case model.TaskPaused:
		return []string{"resume", "input", "cancel"}
	case model.TaskFailed, model.TaskBudgetExhausted:
		return []string{"retry"}
	}
	return []string{}
}

func listTasksAPI(c *gin.Context) {
	userID := currentUserID(c)
	q := model.DB.Where("user_id = ?", userID)
	if raw := strings.TrimSpace(c.Query("conversation_id")); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id 不合法"})
			return
		}
		q = q.Where("main_conversation_id = ? OR exec_conversation_id = ?", id, id)
	}
	var rows []model.AgentTask
	if err := q.Order("id desc").Limit(50).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取任务失败"})
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, t := range rows {
		items = append(items, taskView(t, false))
	}
	c.JSON(http.StatusOK, gin.H{"tasks": items})
}

func createTaskAPI(c *gin.Context) {
	userID := currentUserID(c)
	var body struct {
		taskSpec
		ConversationID uint   `json:"conversation_id"`
		Model          string `json:"model"`
		Kind           string `json:"kind"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式不对"})
		return
	}
	roots := kindWorkspaceRootsFor(userID, body.Kind, agentFSRootsFor(userID))
	task, err := createTask(body.taskSpec, taskCreateOptions{
		UserID: userID, MainConversationID: body.ConversationID, Model: body.Model, Kind: body.Kind,
		Mode: agentModeFor(userID), Roots: roots, AllowCommandChecks: true,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": taskView(task, false)})
}

func taskFromParam(c *gin.Context) (model.AgentTask, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return model.AgentTask{}, false
	}
	t, err := loadUserTask(currentUserID(c), uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return model.AgentTask{}, false
	}
	return t, true
}

func getTaskAPI(c *gin.Context) {
	t, ok := taskFromParam(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": taskView(t, true), "now": time.Now()})
}

func taskActionAPI(c *gin.Context) {
	t, ok := taskFromParam(c)
	if !ok {
		return
	}
	var in taskActionInput
	_ = c.ShouldBindJSON(&in)
	updated, err := applyTaskAction(t, c.Param("action"), in)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, errTaskConflict) {
			code = http.StatusPreconditionFailed
		}
		c.JSON(code, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": taskView(updated, true)})
}
