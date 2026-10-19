package web

import (
	"log"
	"net/http"
	"os"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// resolveKey 取用某个端点的 API Key：优先用户自填（解密后），否则回落服务端环境变量。
// 解密失败仅记日志并继续回落，不打断请求。
func resolveKey(userID string, ep llm.Endpoint) string {
	var row model.UserKey
	if err := model.DB.Where("user_id = ? AND endpoint = ?", userID, ep.ID).First(&row).Error; err == nil {
		plain, derr := model.DecryptKey(row.APIKey)
		if derr != nil {
			log.Printf("解密用户 key 失败 (user=%s endpoint=%s): %v", userID, ep.ID, derr)
		} else if plain != "" {
			return plain
		}
	}
	return os.Getenv(ep.EnvKey)
}

// maskKey 长度 > 7 时保留前 3 后 4，否则整体打码。
func maskKey(k string) string {
	if len(k) > 7 {
		return k[:3] + "****" + k[len(k)-4:]
	}
	return "****"
}

type keyItem struct {
	ID       uint   `json:"id"`
	Endpoint string `json:"endpoint"`
	Masked   string `json:"masked"`
}

func listKeys(c *gin.Context) {
	var rows []model.UserKey
	model.DB.Where("user_id = ?", currentUserID(c)).Order("id asc").Find(&rows)

	items := make([]keyItem, 0, len(rows))
	for _, r := range rows {
		masked := ""
		if plain, err := model.DecryptKey(r.APIKey); err == nil {
			masked = maskKey(plain)
		}
		items = append(items, keyItem{ID: r.Id, Endpoint: r.Endpoint, Masked: masked})
	}
	c.JSON(http.StatusOK, items)
}

func upsertKey(c *gin.Context) {
	userID := currentUserID(c)

	var body struct {
		Endpoint string `json:"endpoint"`
		APIKey   string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	if body.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key 不能为空"})
		return
	}
	ep, ok := registry.Endpoint(body.Endpoint)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知端点"})
		return
	}

	enc, err := model.EncryptKey(body.APIKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "加密失败"})
		return
	}

	var row model.UserKey
	if err := model.DB.Where("user_id = ? AND endpoint = ?", userID, ep.ID).First(&row).Error; err == nil {
		row.APIKey = enc
		model.DB.Save(&row)
	} else {
		model.DB.Create(&model.UserKey{UserID: userID, Endpoint: ep.ID, APIKey: enc})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func deleteKey(c *gin.Context) {
	var row model.UserKey
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), currentUserID(c)).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "不存在"})
		return
	}
	model.DB.Delete(&row)
	c.Status(http.StatusNoContent)
}
