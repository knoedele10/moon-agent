package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	sessionCookie = "session"
	sessionMaxAge = 7 * 24 * 60 * 60 // 7 天
	ctxUserIDKey  = "web.userID"
)

// sessionMAC 用 APP_SECRET 对 userID 做 HMAC-SHA256。
func sessionMAC(userID string) []byte {
	mac := hmac.New(sha256.New, []byte(os.Getenv("APP_SECRET")))
	mac.Write([]byte(userID))
	return mac.Sum(nil)
}

// makeSession 生成 cookie 值：base64url(userID) + "." + base64url(HMAC(userID))。
func makeSession(userID string) string {
	id := base64.RawURLEncoding.EncodeToString([]byte(userID))
	sig := base64.RawURLEncoding.EncodeToString(sessionMAC(userID))
	return id + "." + sig
}

// parseSession 校验 cookie 值，通过则返回 userID。任何格式/签名问题都视为未登录。
func parseSession(val string) (string, bool) {
	i := strings.LastIndex(val, ".")
	if i <= 0 || i == len(val)-1 {
		return "", false
	}
	rawID, err := base64.RawURLEncoding.DecodeString(val[:i])
	if err != nil {
		return "", false
	}
	userID := string(rawID)
	if userID == "" {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(val[i+1:])
	if err != nil {
		return "", false
	}
	if !hmac.Equal(sig, sessionMAC(userID)) {
		return "", false
	}
	return userID, true
}

func setSession(c *gin.Context, userID string) {
	c.SetCookie(sessionCookie, makeSession(userID), sessionMaxAge, "/", "", false, true)
}

func clearSession(c *gin.Context) {
	c.SetCookie(sessionCookie, "", -1, "/", "", false, true)
}

func isAPIRequest(c *gin.Context) bool {
	return strings.HasPrefix(c.Request.URL.Path, "/api")
}

// authRequired 校验签名会话；页面请求失败跳登录，/api 请求失败返回 401。
func authRequired(c *gin.Context) {
	val, _ := c.Cookie(sessionCookie)
	if userID, ok := parseSession(val); ok {
		c.Set(ctxUserIDKey, userID)
		c.Next()
		return
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
