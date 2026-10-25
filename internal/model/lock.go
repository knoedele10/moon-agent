package model

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockUpdate 在 MySQL 上加 FOR UPDATE，把同一行的并发写串起来。
// SQLite 不接受这条语法；单机桌面场景里写事务本身已经串行，省略锁子句即可。
func LockUpdate(tx *gorm.DB) *gorm.DB {
	if tx == nil || tx.Dialector == nil {
		return tx
	}
	if tx.Dialector.Name() == "sqlite" {
		return tx
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"})
}
