package model

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var DB *gorm.DB

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// InitDB initializes persistence; callers decide how to report startup errors.
func InitDB() error {
	host := env("DB_HOST", "127.0.0.1")
	port := env("DB_PORT", "3306")
	user := env("DB_USER", "root")
	password := env("DB_PASSWORD", "")
	name := env("DB_NAME", "blog_my")

	// 库可能还不存在，先连服务器（不带库名）把它建出来。
	serverDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/?charset=utf8mb4&parseTime=True&loc=Local", user, password, host, port)
	server, err := gorm.Open(mysql.Open(serverDSN), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect mysql server: %w", err)
	}
	serverDB, err := server.DB()
	if err != nil {
		return fmt.Errorf("get mysql server connection: %w", err)
	}
	createSQL := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", strings.ReplaceAll(name, "`", "``"))
	createErr := server.Exec(createSQL).Error
	_ = serverDB.Close()
	if createErr != nil {
		return fmt.Errorf("create database %q: %w", name, createErr)
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", user, password, host, port, name)
	DB, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect database %q: %w", name, err)
	}

	if err := DB.AutoMigrate(&User{}, &UserKey{}, &Conversation{}, &Message{}, &KBDocument{}, &KBChunk{}, &AgentSetting{}, &KBMemoryEvent{}, &AgentSkill{}, &AgentPendingTool{}, &AgentAssistant{}, &AgentPrompt{}, &AgentProactiveState{}, &AgentProactiveMessage{}, &AgentSubagentRun{}, &AgentTeamTask{},
		// 自媒体内容库（素材/稿件/版本/关联/发布台账）：只增表，不改既有表语义。
		&ContentAsset{}, &ContentDraft{}, &ContentDraftVersion{}, &ContentDraftAsset{}, &ContentPublication{},
		// 定时唤醒（到点让 agent 在该会话里跑一轮）：同样只增表。
		&AgentWakeup{}); err != nil {
		if sqlDB, closeErr := DB.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
		DB = nil
		return fmt.Errorf("migrate database: %w", err)
	}
	reclaimRunningSubagents()
	dropLegacySHAIndex()
	// v2 的索引对账：把老库上 child_conversation_id 的唯一索引降级（续跑要同一子会话多行 run）。
	migrateTeamSchema()
	return nil
}

// dropLegacySHAIndex 删掉 kb_document.sha256 上那个历史遗留的**全局**唯一索引。
// reclaimRunningSubagents 是启动对账：进程重启后，上次遗留的 running 子任务永远不会再有人收尾
// （跑它的 goroutine 随进程一起没了），不处理就会永久占着并发名额。
// 这里统一标成 failed，让容量立刻可用；子会话本身不删，用户还能点开看它跑到哪一步。
func reclaimRunningSubagents() {
	if DB == nil {
		return
	}
	res := DB.Model(&AgentSubagentRun{}).
		Where("status = ?", SubagentRunning).
		Updates(map[string]any{
			"status":      SubagentFailed,
			"stop_reason": "restart",
			"err_text":    "服务重启，这个子任务没跑完",
			"finished_at": time.Now(),
		})
	if res.Error != nil {
		log.Printf("[subagent] 启动对账失败（残留的 running 子任务未回收）：%v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("[subagent] 启动对账：把 %d 个残留的 running 子任务标为失败", res.RowsAffected)
	}
}

// 它曾经让「两个用户存同一条内容」变成后写的那条静默失败（撞唯一键）。现在唯一性由
// (user_id, sha256) 复合索引承担；AutoMigrate 只建不删，所以老索引必须在这里显式清掉，
// 否则老库上这个 bug 依旧存在。索引本来就不存在时 MySQL 报 1091，属正常情况。
func dropLegacySHAIndex() {
	if DB == nil {
		return
	}
	// 先查存在性再删：直接 DROP 一个不存在的索引会让 MySQL 报 1091，
	// GORM 会把这条失败语句连同错误一起打进日志——每次启动刷一行没用的报错。
	var n int64
	if err := DB.Raw("SELECT COUNT(*) FROM information_schema.statistics " +
		"WHERE table_schema = DATABASE() AND table_name = 'kb_document' " +
		"AND index_name = 'idx_kb_document_sha256'").Scan(&n).Error; err != nil {
		log.Printf("检查历史索引是否存在失败（跳过清理）：%v", err)
		return
	}
	if n == 0 {
		return // 已经清过了
	}
	if err := DB.Exec("DROP INDEX idx_kb_document_sha256 ON kb_document").Error; err != nil {
		log.Printf("删除历史索引失败（不影响启动，但跨用户去重可能仍有问题）：%v", err)
		return
	}
	log.Printf("已删除 kb_document.sha256 的历史全局唯一索引（改用 (user_id, sha256) 复合唯一）")
}
