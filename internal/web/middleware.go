package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"time"

	"blog-my/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	sessionCookie   = "session"
	sessionMaxAge   = 7 * 24 * 60 * 60 // 7 天
	ctxUserIDKey    = "web.userID"
	ctxSessionIDKey = "web.sessionID"

	// sessionTouchInterval 限制 last_seen_at 的写频率：每个请求都写一次库，SQLite 扛不住。
	sessionTouchInterval = time.Minute
)

// sessionMAC 用 APP_SECRET 对会话载荷做 HMAC-SHA256。
func sessionMAC(payload string) []byte {
	mac := hmac.New(sha256.New, []byte(os.Getenv("APP_SECRET")))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// makeSessionWithID 生成 cookie 值：base64url(sessionID:userID) + "." + base64url(HMAC(载荷))。
func makeSessionWithID(sessionID, userID string) string {
	payload := sessionID + ":" + userID
	sig := base64.RawURLEncoding.EncodeToString(sessionMAC(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// parseSessionWithID 校验 cookie 值，通过则返回 userID 和 sessionID。
// 旧版只签 userID 的 cookie 没有会话 id，无法撤销，这里一律视为未登录，让用户重新登录一次。
func parseSessionWithID(val string) (string, string, bool) {
	i := strings.LastIndex(val, ".")
	if i <= 0 || i == len(val)-1 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(val[:i])
	if err != nil {
		return "", "", false
	}
	payload := string(raw)
	sep := strings.IndexByte(payload, ':')
	if sep <= 0 || sep == len(payload)-1 {
		return "", "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(val[i+1:])
	if err != nil {
		return "", "", false
	}
	if !hmac.Equal(sig, sessionMAC(payload)) {
		return "", "", false
	}
	return payload[sep+1:], payload[:sep], true
}

// setSession 先落会话记录再发 cookie；记录写不进去就不发，否则用户会拿着查无此会话的 cookie 反复被踢。
func setSession(c *gin.Context, userID string) error {
	sessionID := uuid.NewString()
	if model.DB != nil {
		now := time.Now()
		if err := model.DB.Create(&model.UserSession{Id: sessionID, UserID: userID, IP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"), CreatedAt: now, LastSeenAt: now}).Error; err != nil {
			return err
		}
	}
	c.SetCookie(sessionCookie, makeSessionWithID(sessionID, userID), sessionMaxAge, "/", "", false, true)
	return nil
}

func clearSession(c *gin.Context) {
	c.SetCookie(sessionCookie, "", -1, "/", "", false, true)
}

func isAPIRequest(c *gin.Context) bool {
	return strings.HasPrefix(c.Request.URL.Path, "/api")
}

// sessionActive 确认会话仍然有效：未撤销、未过期、账号未禁用。
// 签名只证明 cookie 是我们发的；能不能继续用，以库里的状态为准。
func sessionActive(c *gin.Context, sessionID, userID string) bool {
	var session model.UserSession
	if err := model.DB.Where("id = ? AND user_id = ? AND revoked_at IS NULL", sessionID, userID).First(&session).Error; err != nil {
		return false
	}
	now := time.Now()
	if now.Sub(session.CreatedAt) > sessionMaxAge*time.Second {
		return false
	}
	var u model.User
	if err := model.DB.Select("id", "status").Where("id = ?", userID).First(&u).Error; err != nil || u.Status == "disabled" {
		return false
	}
	if now.Sub(session.LastSeenAt) >= sessionTouchInterval {
		_ = model.DB.Model(&session).Updates(map[string]any{"last_seen_at": now, "ip": c.ClientIP(), "user_agent": c.GetHeader("User-Agent")}).Error
	}
	return true
}

// authRequired 校验签名会话；页面请求失败跳登录，/api 请求失败返回 401。
func authRequired(c *gin.Context) {
	val, _ := c.Cookie(sessionCookie)
	userID, sessionID, ok := parseSessionWithID(val)
	if ok && model.DB != nil {
		ok = sessionActive(c, sessionID, userID)
	}
	if ok {
		c.Set(ctxUserIDKey, userID)
		c.Set(ctxSessionIDKey, sessionID)
		c.Next()
		return
	}

	if val != "" {
		clearSession(c)
	}
	if isAPIRequest(c) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	c.Redirect(http.StatusFound, "/login")
	c.Abort()
}

func currentUserID(c *gin.Context) string {
	return c.GetString(ctxUserIDKey)
}

func adminRequired(c *gin.Context) {
	userID := currentUserID(c)
	var u model.User
	if model.DB == nil || model.DB.Where("id = ?", userID).First(&u).Error != nil || (u.Role != "admin" && os.Getenv("ADMIN_USER_ID") != userID) {
		if isAPIRequest(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		} else {
			c.AbortWithStatus(http.StatusForbidden)
		}
		return
	}
	c.Next()
}
