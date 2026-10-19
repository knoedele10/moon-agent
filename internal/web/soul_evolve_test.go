package web

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"blog-my/internal/model"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestSoulGrowthBlockAndToolSchema(t *testing.T) {
	got := soulGrowthBlock(`[{"name":"initiative","level":5,"evidence":""},{"name":"unknown","level":5}]`)
	if !containsAll(got, "主动性：5/5", "可变特质") {
		t.Fatalf("unexpected growth block: %s", got)
	}
	if soulGrowthBlock(`[{"name":"unknown","level":5}]`) != "" {
		t.Fatal("unknown trait must be ignored")
	}
	if len(soulEvolveTool{}.Parameters()) == 0 {
		t.Fatal("missing tool schema")
	}
}

func TestSoulEvolveMySQLPersistence(t *testing.T) {
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
	args, _ := json.Marshal(map[string]any{"traits": []map[string]any{{"name": "initiative", "level": 5, "evidence": "多次主动推进任务"}}})
	res, err := (soulEvolveTool{userID: uid}).Execute(context.Background(), args, nil)
	if err != nil || res.IsError {
		t.Fatalf("evolve failed: %+v %v", res, err)
	}
	var saved model.AgentSetting
	if err = db.Where("user_id = ?", uid).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if !containsAll(soulGrowthBlock(saved.SoulGrowth), "主动性：5/5") {
		t.Fatalf("adjustment missing: %s", saved.SoulGrowth)
	}
	personality, voice := "温和但有主见", "先给结论，再解释依据"
	change, _ := json.Marshal(map[string]string{"personality": personality, "voice": voice})
	res, err = (soulEvolveTool{userID: uid}).Execute(context.Background(), change, nil)
	if err != nil || res.IsError {
		t.Fatalf("soul content update failed: %+v %v", res, err)
	}
	if err = db.Where("user_id = ?", uid).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.SoulPersonality != personality || saved.SoulVoice != voice {
		t.Fatalf("soul text not persisted: %+v", saved)
	}
	assistant := model.AgentAssistant{UserID: uid, Name: "evolve-test", Enabled: true}
	if err = db.Create(&assistant).Error; err != nil {
		t.Fatal(err)
	}
	assistantPersonality := "冷静、耐心、擅长分析"
	change, _ = json.Marshal(map[string]string{"personality": assistantPersonality})
	res, err = (soulEvolveTool{userID: uid, assistantID: assistant.Id}).Execute(context.Background(), change, nil)
	if err != nil || res.IsError {
		t.Fatalf("assistant soul update failed: %+v %v", res, err)
	}
	if err = db.First(&assistant, assistant.Id).Error; err != nil {
		t.Fatal(err)
	}
	if assistant.SoulPersonality != assistantPersonality {
		t.Fatalf("assistant soul not persisted: %+v", assistant)
	}
	bad, _ := json.Marshal(map[string]any{"traits": []map[string]any{{"name": "core_rules", "level": 5, "evidence": "try to override core"}}})
	res, err = (soulEvolveTool{userID: uid}).Execute(context.Background(), bad, nil)
	if err != nil || !res.IsError {
		t.Fatal("unknown/core trait should be rejected")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
