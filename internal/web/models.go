package web

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// health 是探活接口。刻意不碰数据库与本地模型：
// 它要回答的是“进程还在不在”，而不是“下游都健康吗”——后者会把探活变成慢接口。
func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "blog-my",
		"version": "go-1",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

type modelItem struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Endpoint string `json:"endpoint"`
	HasKey   bool   `json:"has_key"`
}

func listModels(c *gin.Context) {
	userID := currentUserID(c)

	items := make([]modelItem, 0)
	for _, ep := range registry.Endpoints() {
		hasKey := resolveKey(userID, ep) != ""
		for _, m := range ep.Models {
			items = append(items, modelItem{
				ID:       m,
				Label:    ep.Label + " · " + m,
				Endpoint: ep.ID,
				HasKey:   hasKey,
			})
		}
	}

	c.JSON(http.StatusOK, items)
}
