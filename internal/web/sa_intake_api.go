package web

import (
	"net/http"
	"strconv"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// The intake APIs are intentionally read-only.  They are useful to the
// Superagent panel and keep the panel from reaching into the sa_* tables.
func listSAThreadsAPI(c *gin.Context) {
	limit := saAPILimit(c)
	var rows []model.SAThread
	q := model.DB.WithContext(c.Request.Context()).Where("user_id = ?", currentUserID(c)).Order("id desc").Limit(limit)
	if source := c.Query("source"); source != "" {
		q = q.Where("source = ?", source)
	}
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func listSAThreadTriggersAPI(c *gin.Context) {
	threadID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || threadID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid thread id"})
		return
	}
	var thread model.SAThread
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", threadID, currentUserID(c)).First(&thread).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "thread not found"})
		return
	}
	var rows []model.SATrigger
	if err := model.DB.WithContext(c.Request.Context()).Where("thread_id = ?", thread.Id).Order("id asc").Limit(saAPILimit(c)).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func listSAOutboundAPI(c *gin.Context) {
	var rows []model.SAOutbound
	q := model.DB.WithContext(c.Request.Context()).Where("user_id = ?", currentUserID(c)).Order("id desc").Limit(saAPILimit(c))
	if status := c.Query("status"); status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func saAPILimit(c *gin.Context) int {
	n, err := strconv.Atoi(c.Query("limit"))
	if err != nil || n <= 0 {
		return 100
	}
	if n > 200 {
		return 200
	}
	return n
}

func init() {
	saRouteRegistrars = append(saRouteRegistrars, func(api *gin.RouterGroup) {
		api.GET("/sa/threads", listSAThreadsAPI)
		api.GET("/sa/threads/:id/triggers", listSAThreadTriggersAPI)
		api.GET("/sa/outbound", listSAOutboundAPI)
	})
}
