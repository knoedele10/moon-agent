package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"blog-my/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupAuthTest(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("APP_SECRET", "auth-test-secret")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.UserSession{}); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sqlDB, _ := db.DB(); sqlDB.Close() })

	r := gin.New()
	api := r.Group("/api", authRequired)
	api.GET("/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"id": currentUserID(c)}) })
	admin := api.Group("/admin", adminRequired)
	admin.POST("/users/:id/disable", adminDisableUser)
	return db, r
}

func createAuthUser(t *testing.T, db *gorm.DB, id, role string) {
	t.Helper()
	if err := db.Create(&model.User{Id: id, Username: id, Password: "x", Role: role, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
}

// loginCookie 走 setSession 拿到真实的会话 cookie。
func loginCookie(t *testing.T, userID string) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/login", nil)
	if err := setSession(c, userID); err != nil {
		t.Fatal(err)
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == sessionCookie {
			return ck
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func doAuth(r *gin.Engine, method, path string, ck *http.Cookie) int {
	req := httptest.NewRequest(method, path, nil)
	if ck != nil {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestAuthRejectsDisabledUser(t *testing.T) {
	db, r := setupAuthTest(t)
	createAuthUser(t, db, "u1", "user")
	ck := loginCookie(t, "u1")
	if code := doAuth(r, http.MethodGet, "/api/me", ck); code != http.StatusOK {
		t.Fatalf("active user: got %d", code)
	}
	// 直接改库，模拟会话未撤销但账号已禁用：中间件本身也必须拦住。
	db.Model(&model.User{}).Where("id = ?", "u1").Update("status", "disabled")
	if code := doAuth(r, http.MethodGet, "/api/me", ck); code != http.StatusUnauthorized {
		t.Fatalf("disabled user: got %d, want 401", code)
	}
}

func TestAuthRejectsLegacyCookie(t *testing.T) {
	db, r := setupAuthTest(t)
	createAuthUser(t, db, "u1", "user")
	// 旧格式：base64url(userID) + "." + base64url(HMAC(userID))，签名合法但没有会话 id。
	legacy := base64.RawURLEncoding.EncodeToString([]byte("u1")) + "." + base64.RawURLEncoding.EncodeToString(sessionMAC("u1"))
	if code := doAuth(r, http.MethodGet, "/api/me", &http.Cookie{Name: sessionCookie, Value: legacy}); code != http.StatusUnauthorized {
		t.Fatalf("legacy cookie: got %d, want 401", code)
	}
}

func TestAuthRejectsRevokedAndExpiredSessions(t *testing.T) {
	db, r := setupAuthTest(t)
	createAuthUser(t, db, "u1", "user")

	revoked := loginCookie(t, "u1")
	db.Model(&model.UserSession{}).Where("revoked_at IS NULL").Update("revoked_at", time.Now())
	if code := doAuth(r, http.MethodGet, "/api/me", revoked); code != http.StatusUnauthorized {
		t.Fatalf("revoked: got %d, want 401", code)
	}

	expired := loginCookie(t, "u1")
	db.Model(&model.UserSession{}).Where("revoked_at IS NULL").Update("created_at", time.Now().Add(-8*24*time.Hour))
	if code := doAuth(r, http.MethodGet, "/api/me", expired); code != http.StatusUnauthorized {
		t.Fatalf("expired: got %d, want 401", code)
	}
}

func TestAdminDisableRevokesSessions(t *testing.T) {
	db, r := setupAuthTest(t)
	createAuthUser(t, db, "admin", "admin")
	createAuthUser(t, db, "u1", "user")
	adminCk := loginCookie(t, "admin")
	userCk := loginCookie(t, "u1")

	if code := doAuth(r, http.MethodPost, "/api/admin/users/admin/disable", adminCk); code != http.StatusBadRequest {
		t.Fatalf("self-disable: got %d, want 400", code)
	}
	if code := doAuth(r, http.MethodPost, "/api/admin/users/u1/disable", adminCk); code != http.StatusOK {
		t.Fatalf("disable: got %d", code)
	}
	var live int64
	db.Model(&model.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", "u1").Count(&live)
	if live != 0 {
		t.Fatalf("u1 still has %d live sessions", live)
	}
	if code := doAuth(r, http.MethodGet, "/api/me", userCk); code != http.StatusUnauthorized {
		t.Fatalf("disabled user's old cookie: got %d, want 401", code)
	}
}

func TestAuthPageRedirectsOnInvalidSession(t *testing.T) {
	_, _ = setupAuthTest(t)
	r := gin.New()
	r.GET("/settings", authRequired, func(c *gin.Context) { c.Status(http.StatusOK) })
	if code := doAuth(r, http.MethodGet, "/settings", &http.Cookie{Name: sessionCookie, Value: "bogus.value"}); code != http.StatusFound {
		t.Fatalf("page with bad cookie: got %d, want 302", code)
	}
}
