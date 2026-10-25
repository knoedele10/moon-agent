package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteInitMigrateAndUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	t.Setenv("DB_DRIVER", "sqlite")
	t.Setenv("SQLITE_PATH", path)
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_NAME", "")
	t.Setenv("DB_PASSWORD", "")

	prev := DB
	t.Cleanup(func() {
		if DB != nil {
			if sqlDB, err := DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		DB = prev
	})
	if err := InitDB(); err != nil {
		t.Fatal(err)
	}
	if Driver() != "sqlite" {
		t.Fatalf("driver = %s", Driver())
	}
	user := User{Username: "sqlite-user", Password: "hash"}
	if err := DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	conv := Conversation{UserID: user.Id, Title: "hi", Model: "deepseek-chat", Kind: "work"}
	if err := DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	if err := RecordUsage(AgentUsage{
		UserID: user.Id, ConversationID: conv.Id, RunID: "run-1", Model: "deepseek-chat",
		InputTokens: 100, OutputTokens: 20, CachedTokens: 40,
	}); err != nil {
		t.Fatal(err)
	}
	totals, err := SumUsage(user.Id, conv.Id)
	if err != nil {
		t.Fatal(err)
	}
	if totals.InputTokens != 100 || totals.OutputTokens != 20 || totals.CachedTokens != 40 || totals.Requests != 1 {
		t.Fatalf("totals = %+v", totals)
	}
	payload, err := ExportUser(user.Id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "hi") {
		t.Fatalf("export missing conversation: %s", payload)
	}
	// 再导一次备份：文件已有内容，当天备份应出现。
	if err := backupSQLiteDaily(path); err != nil {
		t.Fatal(err)
	}
	bak := path + ".bak-" + time.Now().Format("20060102")
	if _, err := os.Stat(bak); err != nil {
		t.Fatal(err)
	}
}
