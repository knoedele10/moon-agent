package model

import "time"

// KBMemoryEvent 是记忆的变更流水（ADD / DELETE 等）。
//
// 为什么单独记一张表：kb_document 只保留最终状态，出问题时答不出
// 「这条记忆是什么时候、因为哪轮对话进来的」。流水表让记忆可审计，
// 也方便以后做「这条记错了，撤掉」这类操作。
type KBMemoryEvent struct {
	Id         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	DocumentID uint      `gorm:"column:document_id;index;not null"`
	UserID     string    `gorm:"column:user_id;index;size:36"`
	Event      string    `gorm:"column:event;size:16;not null"`
	Detail     string    `gorm:"column:detail;type:text"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (KBMemoryEvent) TableName() string { return "kb_memory_event" }
