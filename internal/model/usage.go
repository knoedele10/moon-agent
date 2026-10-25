package model

import "time"

// AgentUsage 是一轮模型调用汇总后的 token 账。
// 数字来自供应商响应；没有用量的轮次不写行。
type AgentUsage struct {
	Id             uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID         string    `gorm:"column:user_id;index;size:36;not null"`
	ConversationID uint      `gorm:"column:conversation_id;index"`
	RunID          string    `gorm:"column:run_id;size:128;index"`
	Model          string    `gorm:"column:model;size:128"`
	InputTokens    int       `gorm:"column:input_tokens"`
	OutputTokens   int       `gorm:"column:output_tokens"`
	CachedTokens   int       `gorm:"column:cached_tokens"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (AgentUsage) TableName() string { return "agent_usage" }

// UsageTotals 是一个会话（或用户）的累加。
type UsageTotals struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
	Requests     int `json:"requests"`
}

func RecordUsage(row AgentUsage) error {
	if DB == nil {
		return nil
	}
	if row.InputTokens == 0 && row.OutputTokens == 0 && row.CachedTokens == 0 {
		return nil
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	return DB.Create(&row).Error
}

func SumUsage(userID string, conversationID uint) (UsageTotals, error) {
	var out UsageTotals
	if DB == nil || userID == "" {
		return out, nil
	}
	q := DB.Model(&AgentUsage{}).Where("user_id = ?", userID)
	if conversationID != 0 {
		q = q.Where("conversation_id = ?", conversationID)
	}
	err := q.Select("COALESCE(SUM(input_tokens),0) AS input_tokens, COALESCE(SUM(output_tokens),0) AS output_tokens, COALESCE(SUM(cached_tokens),0) AS cached_tokens, COUNT(*) AS requests").
		Scan(&out).Error
	return out, err
}
