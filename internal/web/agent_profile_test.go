package web

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"blog-my/internal/model"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestFormatProfileBlock(t *testing.T) {
	if got := formatProfileBlock(" \n "); got != "" {
		t.Fatalf("空画像应不注入，得到 %q", got)
	}
	got := formatProfileBlock("用户偏好中文回答。")
	if !strings.Contains(got, "【关于这位用户的长期画像】") || !strings.Contains(got, "用户偏好中文回答。") {
		t.Fatalf("画像未正确包装为注入块：%q", got)
	}
}

func TestProfileRefreshThreshold(t *testing.T) {
	if profileRefreshDue(19, 20) {
		t.Fatal("19 轮不应触发画像刷新")
	}
	if !profileRefreshDue(20, 20) {
		t.Fatal("达到 20 轮应触发画像刷新")
	}
	if profileRefreshDue(20, 0) {
		t.Fatal("非法阈值不应触发")
	}
}

func TestRegenerateProfileRejectsInvalidUTF8(t *testing.T) {
	for _, bad := range []string{string([]byte{0xff}), "损坏�画像"} {
		if err := validateProfileText(bad); err == nil {
			t.Fatalf("应拒绝乱码画像 %q", bad)
		}
	}
	if err := validateProfileText("用户偏好中文回复。\n使用 Go 项目。"); err != nil {
		t.Fatalf("正常中文应通过校验：%v", err)
	}
}

// TestProfileMySQLIntegration exercises the real persistence/injection path.
// Run with AGENT_PROFILE_IT=1 and DB_* variables set; it cleans up its UUID-scoped rows.
func TestProfileMySQLIntegration(t *testing.T) {
	if os.Getenv("AGENT_PROFILE_IT") != "1" {
		t.Skip("设置 AGENT_PROFILE_IT=1 才连接 MySQL")
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
		t.Fatalf("连接 MySQL 失败：%v", err)
	}
	if err := db.AutoMigrate(&model.AgentSetting{}, &model.KBDocument{}); err != nil {
		t.Fatalf("同步画像字段失败：%v", err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB })

	userID := uuid.NewString()
	t.Cleanup(func() {
		db.Where("user_id = ?", userID).Delete(&model.KBDocument{})
		db.Where("user_id = ?", userID).Delete(&model.AgentSetting{})
		if sqlDB, e := db.DB(); e == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.Create(&model.KBDocument{UserID: userID, Kind: "text", Title: "测试记忆", Source: "memory", Content: []byte("用户偏好中文回复，项目使用 Go。")}).Error; err != nil {
		t.Fatalf("创建测试记忆失败：%v", err)
	}
	if n, err := incrementProfileTurns(userID); err != nil || n != 1 {
		t.Fatalf("首次累计画像轮数：n=%d err=%v", n, err)
	}
	// 再次读取/递增模拟进程重启后的数据库状态恢复。
	if n, err := incrementProfileTurns(userID); err != nil || n != 2 {
		t.Fatalf("持久化画像轮数：n=%d err=%v", n, err)
	}
	want := "## 稳定偏好\n用户偏好中文回复。"
	if err := regenerateProfileWithCaller(context.Background(), userID, func(context.Context, string, string) (string, error) {
		return want, nil
	}); err != nil {
		t.Fatalf("生成画像失败：%v", err)
	}
	var saved model.KBDocument
	if err := db.Where("user_id = ? AND source = ?", userID, "profile").First(&saved).Error; err != nil {
		t.Fatalf("画像未写入：%v", err)
	}
	if string(saved.Content) != want || !utf8.Valid(saved.Content) || strings.ContainsRune(string(saved.Content), utf8.RuneError) {
		t.Fatalf("画像保存内容异常：%q", string(saved.Content))
	}
	if block := kbProfileBlock(userID); !strings.Contains(block, want) {
		t.Fatalf("画像未进入提示词注入块：%q", block)
	}
	if err := regenerateProfileWithCaller(context.Background(), userID, func(context.Context, string, string) (string, error) {
		return string([]byte{0xff}), nil
	}); err == nil {
		t.Fatal("非法 UTF-8 画像应被拒绝")
	}
	var after model.KBDocument
	if err := db.Where("user_id = ? AND source = ?", userID, "profile").First(&after).Error; err != nil {
		t.Fatalf("拒绝坏输出后旧画像应保留：%v", err)
	}
	if string(after.Content) != want {
		t.Fatalf("拒绝坏输出后画像被覆盖：%q", string(after.Content))
	}
}
