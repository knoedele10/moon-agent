package web

import (
	"net/http"
	"time"

	"blog-my/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func pageAdmin(c *gin.Context) {
	c.HTML(http.StatusOK, "admin/index.html", gin.H{"Title": "公网用户控制台"})
}

type adminSessionRow struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Username   string     `json:"username"`
	Role       string     `json:"role"`
	IP         string     `json:"ip"`
	UserAgent  string     `json:"user_agent"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func adminListSessions(c *gin.Context) {
	var rows []adminSessionRow
	err := model.DB.Table("user_session").Select("user_session.id, user_session.user_id, user.username, user.role, user_session.ip, user_session.user_agent, user_session.created_at, user_session.last_seen_at, user_session.revoked_at").Joins("JOIN user ON user.id = user_session.user_id").Order("user_session.last_seen_at DESC").Limit(200).Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取会话失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": rows, "now": time.Now()})
}

func adminRevokeSession(c *gin.Context) {
	result := model.DB.Model(&model.UserSession{}).Where("id = ? AND revoked_at IS NULL", c.Param("id")).Updates(map[string]any{"revoked_at": time.Now()})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "撤销会话失败"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在或已撤销"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func adminDisableUser(c *gin.Context) { adminSetUserStatus(c, false) }
func adminEnableUser(c *gin.Context)  { adminSetUserStatus(c, true) }
func adminSetUserStatus(c *gin.Context, enabled bool) {
	userID := c.Param("id")
	status := "disabled"
	if enabled {
		status = "active"
	}
	// 禁用自己会把唯一的管理员锁在门外。
	if !enabled && userID == currentUserID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能禁用当前登录的账号"})
		return
	}
	var found bool
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]any{"status": status})
		if result.Error != nil {
			return result.Error
		}
		found = result.RowsAffected > 0
		if !found || enabled {
			return nil
		}
		// 禁用要立刻生效：把这个账号所有在线会话一起撤销。
		return tx.Model(&model.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", userID).Updates(map[string]any{"revoked_at": time.Now()}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新账号失败"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": status})
}
