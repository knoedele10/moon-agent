package web

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 关闭时补齐记忆：把「上次补齐之后」产生的消息按会话汇总成一条知识库记录。
//
// 为什么需要它：每轮对话完成时已经会自动记一条，但有两种情况会漏——
//   1. 那一轮停在“等用户确认”，或模型没给出正常回复；
//   2. 用户只是聊天、没让 AI 做事，而那句话又不够长。
// 关闭时统一扫一遍，把这些补进知识库，保证“聊过的都留得下”。
//
// 用时间水位（agent_setting.last_memory_at）而不是消息 id：实现更简单，
// 且天然幂等——补齐后水位前移，再点一次也不会重复写。

const flushMaxMessages = 400

// flushMemoryFor 把某用户还没入库的消息补进知识库，返回处理的会话数与消息数。
func flushMemoryFor(userID string) (convs int, msgs int, err error) {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return 0, 0, nil
	}

	var s model.AgentSetting
	hasSetting := model.DB.Where("user_id = ?", userID).First(&s).Error == nil
	var since time.Time
	if hasSetting && s.LastMemoryAt != nil {
		since = *s.LastMemoryAt
	}

	// 圈出该用户所有会话里、水位之后的消息（工具轨迹不参与，它只给前端回放用）。
	var rows []struct {
		ConversationID uint
		Role           string
		Content        string
	}
	q := model.DB.Table("message").
		Select("message.conversation_id, message.role, message.content").
		Joins("JOIN conversation ON conversation.id = message.conversation_id").
		Where("conversation.user_id = ? AND message.role <> ?", userID, roleToolTrace)
	if !since.IsZero() {
		q = q.Where("message.created_at > ?", since)
	}
	if err := q.Order("message.id asc").Limit(flushMaxMessages).Scan(&rows).Error; err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}

	// 按会话分组，每个会话汇总成一条。
	order := make([]uint, 0, 4)
	grouped := make(map[uint][]string)
	for _, r := range rows {
		if _, ok := grouped[r.ConversationID]; !ok {
			order = append(order, r.ConversationID)
		}
		who := "用户"
		if r.Role == "assistant" {
			who = "AI"
		}
		grouped[r.ConversationID] = append(grouped[r.ConversationID], who+"："+clipRunes(r.Content, 300))
	}

	for _, cid := range order {
		lines := grouped[cid]
		if len(lines) == 0 {
			continue
		}
		title := "会话回顾：" + clipRunes(strings.TrimPrefix(lines[0], "用户："), 30)
		text := "这一段对话的要点：\n" + strings.Join(lines, "\n")
		if _, err := kb().IngestText(context.Background(), userID, title, text); err != nil {
			// 单条失败不影响其他会话，也不影响关闭流程。
			log.Printf("[memory] 关闭补齐失败（会话 %d，不影响退出）：%v", cid, err)
			continue
		}
		convs++
		msgs += len(lines)
	}

	// 水位前移：即使某些会话写失败也不回退，避免每次关闭都重写一遍。
	now := time.Now()
	if hasSetting {
		model.DB.Model(&model.AgentSetting{}).Where("user_id = ?", userID).Update("last_memory_at", now)
	} else {
		model.DB.Create(&model.AgentSetting{UserID: userID, LastMemoryAt: &now})
	}
	return convs, msgs, nil
}

// postFlushMemory 供前端在关闭页面时用 sendBeacon 调用：把本轮会话补进知识库。
// touchMemoryWatermark 把水位推到当前时间。
//
// 自动记录成功后也要调用它：否则关闭时会把已经记过的回合再当“未入库”重写一遍。
func touchMemoryWatermark(userID string) {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return
	}
	now := time.Now()
	var s model.AgentSetting
	if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err != nil {
		model.DB.Create(&model.AgentSetting{UserID: userID, LastMemoryAt: &now})
		return
	}
	model.DB.Model(&model.AgentSetting{}).Where("user_id = ?", userID).Update("last_memory_at", now)
}

func postFlushMemory(c *gin.Context) {
	userID := currentUserID(c)
	convs, msgs, err := flushMemoryFor(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "补齐记忆失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "conversations": convs, "messages": msgs})
}
