package web

import (
	"context"
	"path/filepath"
	"testing"

	"blog-my/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// setupSATestDB 是三条工作线共用的测试库：会话、消息、任务与 superagent 全部表。
func setupSATestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sa.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tables := []any{&model.User{}, &model.Conversation{}, &model.Message{}, &model.AgentSetting{}, &model.AgentSubagentRun{}, &model.QQChatBinding{}}
	tables = append(tables, model.TaskModels()...)
	tables = append(tables, model.SAModels()...)
	if err := db.AutoMigrate(tables...); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sqlDB, _ := db.DB(); sqlDB.Close() })
	return db
}

// saSwap 在测试期间替换一个契约函数变量，结束时还原。用法：saSwap(t, &saEvaluate, fake)。
func saSwap[T any](t *testing.T, slot *T, fake T) {
	t.Helper()
	old := *slot
	*slot = fake
	t.Cleanup(func() { *slot = old })
}

func TestSAContractTablesMigrate(t *testing.T) {
	db := setupSATestDB(t)
	th := model.SAThread{UserID: "u1", ThreadKey: "qq:private:1", Source: model.SASourceQQ}
	if err := db.Create(&th).Error; err != nil {
		t.Fatal(err)
	}
	tr := model.SATrigger{UserID: "u1", ThreadID: th.Id, Source: model.SASourceQQ, SourceMsgID: "m1"}
	if err := db.Create(&tr).Error; err != nil {
		t.Fatal(err)
	}
	dup := model.SATrigger{UserID: "u1", ThreadID: th.Id, Source: model.SASourceQQ, SourceMsgID: "m1"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("(source, source_msg_id) 应唯一")
	}
	called := false
	saSwap(t, &saEvaluate, func(context.Context, uint) { called = true })
	saEvaluate(context.Background(), th.Id)
	if !called {
		t.Fatal("saSwap 未生效")
	}
}
