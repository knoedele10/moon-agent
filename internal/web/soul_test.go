package web

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"blog-my/internal/model"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestSoulThreeParts(t *testing.T) {
	if soulBlockFrom("", "", "") != "" {
		t.Fatal("empty soul should be omitted")
	}
	s := soulBlockFrom("温和有主见", "简洁中文", "完成后验证")
	for _, want := range []string{"## 人格\n温和有主见", "## 说话方式\n简洁中文", "## 做事方式\n完成后验证"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.Contains(soulBlockFrom("诚实", "", ""), "## 人格\n诚实") {
		t.Fatal("personality-only soul omitted")
	}
}

func TestSoulMySQLRoundTrip(t *testing.T) {
	if os.Getenv("AGENT_SOUL_IT") != "1" {
		t.Skip("requires MySQL")
	}
	get := func(k, fallback string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return fallback
	}
	dsn := get("DB_USER", "root") + ":" + os.Getenv("DB_PASSWORD") + "@tcp(" + get("DB_HOST", "127.0.0.1") + ":" + get("DB_PORT", "3306") + ")/" + get("DB_NAME", "blog_my") + "?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.AgentSetting{}, &model.AgentAssistant{}); err != nil {
		t.Fatal(err)
	}
	old := model.DB
	model.DB = db
	uid := uuid.NewString()
	t.Cleanup(func() {
		db.Where("user_id = ?", uid).Delete(&model.AgentAssistant{})
		db.Where("user_id = ?", uid).Delete(&model.AgentSetting{})
		model.DB = old
		if conn, e := db.DB(); e == nil {
			_ = conn.Close()
		}
	})
	for _, body := range []string{`{"soul_personality":"温和、有主见"}`, `{"soul_voice":"简洁中文","soul_work":"执行后验证"}`} {
		c, w := newTestContext(http.MethodPut, "/api/agent/settings", body)
		c.Set(ctxUserIDKey, uid)
		putAgentSettings(c)
		if w.Code != 200 {
			t.Fatalf("save: %s", w.Body.String())
		}
	}
	c, w := newTestContext(http.MethodGet, "/api/agent/settings", "")
	c.Set(ctxUserIDKey, uid)
	getAgentSettings(c)
	if !strings.Contains(w.Body.String(), `"soul_personality":"温和、有主见"`) {
		t.Fatalf("read: %s", w.Body.String())
	}
	a := model.AgentAssistant{UserID: uid, Name: "人格测试", Enabled: true}
	if err = db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	if got := resolveAssistantContext(uid, a.Id); got.SoulPersonality != "温和、有主见" {
		t.Fatalf("inherit: %+v", got)
	}
	p := "冷静理性"
	if err = applyAssistantPatch(&a, assistantPatch{SoulPersonality: &p}); err != nil {
		t.Fatal(err)
	}
	if err = db.Save(&a).Error; err != nil {
		t.Fatal(err)
	}
	got := resolveAssistantContext(uid, a.Id)
	block := soulBlockFrom(got.SoulPersonality, got.SoulVoice, got.SoulWork)
	if !strings.Contains(block, "冷静理性") || strings.Contains(block, "温和、有主见") || !strings.Contains(block, "执行后验证") {
		t.Fatalf("override: %s", block)
	}
	empty := ""
	if err = applyAssistantPatch(&a, assistantPatch{SoulPersonality: &empty}); err != nil {
		t.Fatal(err)
	}
	if err = db.Save(&a).Error; err != nil {
		t.Fatal(err)
	}
	if got := resolveAssistantContext(uid, a.Id); got.SoulPersonality != "温和、有主见" {
		t.Fatal("clearing override must restore inheritance")
	}
}
