package model

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DefaultSQLitePath 是没人指定文件时的数据位置。
// Windows 用 %APPDATA%\blog-my\data.db；其它系统用 XDG 数据目录。
func DefaultSQLitePath() string {
	if p := os.Getenv("SQLITE_PATH"); p != "" {
		return p
	}
	if p := os.Getenv("DB_PATH"); p != "" {
		return p
	}
	return filepath.Join(defaultDataDir(), "blog-my", "data.db")
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if p := os.Getenv("APPDATA"); p != "" {
			return p
		}
		return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, ".local", "share")
}

func initSQLite() error {
	path := DefaultSQLitePath()
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create sqlite directory: %w", err)
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return fmt.Errorf("open sqlite %q: %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("sqlite pool: %w", err)
	}
	// 单文件库上多连接容易踩锁；桌面进程一个连接就够。
	sqlDB.SetMaxOpenConns(1)
	DB = db
	if err := autoMigrate(); err != nil {
		if closeErr := sqlDB.Close(); closeErr == nil {
			DB = nil
		}
		return err
	}
	if path != ":memory:" {
		if err := backupSQLiteDaily(path); err != nil {
			log.Printf("sqlite 每日备份失败（不影响启动）：%v", err)
		}
	}
	log.Printf("database: sqlite %s", path)
	return nil
}

func autoMigrate() error {
	if err := DB.AutoMigrate(&User{}, &UserKey{}, &Conversation{}, &Message{}, &KBDocument{}, &KBChunk{}, &AgentSetting{}, &KBMemoryEvent{}, &AgentSkill{}, &AgentPendingTool{}, &AgentAssistant{}, &AgentPrompt{}, &AgentProactiveState{}, &AgentProactiveMessage{}, &AgentSubagentRun{}, &AgentTeamTask{},
		&ContentAsset{}, &ContentDraft{}, &ContentDraftVersion{}, &ContentDraftAsset{}, &ContentPublication{},
		&AgentWakeup{}, &AgentUsage{}); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	reclaimRunningSubagents()
	return nil
}

// backupSQLiteDaily 把库文件复制成 data.db.bak-YYYYMMDD。当天已有备份则跳过。
func backupSQLiteDaily(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil
	}
	dest := path + ".bak-" + time.Now().Format("20060102")
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
