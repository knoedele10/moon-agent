package web

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
	"blog-my/internal/agent/rules"
	"blog-my/internal/agent/runstore"
	"blog-my/internal/agent/toolkit"
	"blog-my/internal/agent/trigger"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

func getSetupStatus(c *gin.Context) {
	driver := model.Driver()
	root := ""
	mode := "confirm"
	if model.DB != nil {
		userID := currentUserID(c)
		root = agentFSRootFor(userID)
		var setting model.AgentSetting
		if err := model.DB.Where("user_id = ?", userID).First(&setting).Error; err == nil && strings.TrimSpace(setting.Mode) != "" {
			mode = setting.Mode
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"driver":          driver,
		"sqlite_path":     model.DefaultSQLitePath(),
		"has_workspace":   strings.TrimSpace(root) != "",
		"workspace":       root,
		"permission_mode": mode,
		"ready":           driver != "" && strings.TrimSpace(os.Getenv("APP_SECRET")) != "",
	})
}

func getAgentUsage(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	var convID uint
	if raw := strings.TrimSpace(c.Query("conversation_id")); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id 不合法"})
			return
		}
		convID = uint(n)
	}
	totals, err := model.SumUsage(currentUserID(c), convID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取用量失败"})
		return
	}
	c.JSON(http.StatusOK, totals)
}

func getAgentRuns(c *gin.Context) {
	st := agentRunStore()
	if st == nil {
		c.JSON(http.StatusOK, gin.H{"runs": []any{}})
		return
	}
	rows, err := st.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []runstore.Summary{}
	}
	c.JSON(http.StatusOK, gin.H{"runs": rows})
}

func postResolveRun(c *gin.Context) {
	st := agentRunStore()
	if st == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run journal 不可用"})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 JSON"})
		return
	}
	status := runstore.StatusAborted
	switch strings.ToLower(strings.TrimSpace(body.Action)) {
	case "abandon":
		status = runstore.StatusAborted
	case "ack", "recovered":
		status = runstore.StatusRecovered
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action 只能是 ack 或 abandon"})
		return
	}
	id := c.Param("id")
	if err := st.Append(id, "", "run_resolved", status, map[string]any{"action": body.Action, "replay": false}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": status, "replay": false})
}

func getRunArtifacts(c *gin.Context) {
	st := agentRunStore()
	if st == nil {
		c.JSON(http.StatusOK, gin.H{"artifacts": []any{}})
		return
	}
	events, err := st.Read(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"artifacts": collectArtifacts(events)})
}

func collectArtifacts(events []runstore.Event) []map[string]any {
	var out []map[string]any
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.Type != "tool_finished" || len(ev.Payload) == 0 {
			continue
		}
		var body struct {
			Tool string `json:"tool"`
			Path string `json:"path"`
			OK   bool   `json:"ok"`
		}
		if err := json.Unmarshal(ev.Payload, &body); err != nil || strings.TrimSpace(body.Path) == "" {
			continue
		}
		key := body.Tool + "\x00" + body.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{"tool": body.Tool, "path": body.Path, "ok": body.OK})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func postInitRules(c *gin.Context) {
	root := agentFSRootFor(currentUserID(c))
	if strings.TrimSpace(root) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "还没有工作区"})
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = c.ShouldBindJSON(&body)
	path, created, err := rules.Scaffold(root, body.Force)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": path, "created": created})
}

func getWorkspaceDiff(c *gin.Context) {
	root := agentFSRootFor(currentUserID(c))
	if strings.TrimSpace(root) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "还没有工作区"})
		return
	}
	status := toolkit.DirtyWorkspaceNote(root)
	diff, diffErr := toolkit.UnifiedDiff(c.Request.Context(), root)
	payload := gin.H{
		"root":  root,
		"dirty": strings.TrimSpace(status) != "",
		"note":  status,
		"diff":  diff,
	}
	if diffErr != nil {
		payload["diff_error"] = diffErr.Error()
	}
	c.JSON(http.StatusOK, payload)
}

func postRestoreFile(c *gin.Context) {
	root := agentFSRootFor(currentUserID(c))
	if strings.TrimSpace(root) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "还没有工作区"})
		return
	}
	var body struct {
		Path      string `json:"path"`
		SessionID string `json:"session_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Path) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 path"})
		return
	}
	raw, err := json.Marshal(map[string]string{"file_path": body.Path})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := (agentfs.RestoreTool{}).Execute(c.Request.Context(), raw, &agent.Exec{
		Root:      root,
		SessionID: strings.TrimSpace(body.SessionID),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	code := http.StatusOK
	if res.IsError {
		code = http.StatusConflict
	}
	c.JSON(code, gin.H{"content": res.Content, "is_error": res.IsError, "display": res.Display})
}

func getExportData(c *gin.Context) {
	payload, err := model.ExportUser(currentUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="blog-my-export.json"`)
	c.Data(http.StatusOK, "application/json", payload)
}

func postImportData(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取导入内容失败"})
		return
	}
	n, err := model.ImportUser(currentUserID(c), body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "imported_conversations": n})
		return
	}
	c.JSON(http.StatusOK, gin.H{"imported_conversations": n})
}

var (
	triggerMu   sync.Mutex
	triggerPrev = map[string][]trigger.Entry{}
)

// getFileTrigger 对比上一次快照，只报告变化。不会启动 agent。
func getFileTrigger(c *gin.Context) {
	root := agentFSRootFor(currentUserID(c))
	if strings.TrimSpace(root) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "还没有工作区"})
		return
	}
	cur, err := trigger.Snapshot(root, 2000)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	triggerMu.Lock()
	prev, had := triggerPrev[root]
	triggerPrev[root] = cur
	triggerMu.Unlock()
	var changed []string
	if had {
		changed = trigger.Changed(prev, cur)
	}
	if changed == nil {
		changed = []string{}
	}
	c.JSON(http.StatusOK, gin.H{
		"root":     root,
		"files":    len(cur),
		"baseline": !had,
		"changed":  changed,
		"auto_run": false,
	})
}
