package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"blog-my/internal/model"
	"gorm.io/gorm"
)

// saOutboundWake 是入队后的立即扫描信号。缓冲 1：发送器忙时合并多次唤醒。
var saOutboundWake = make(chan struct{}, 1)

var (
	saOutboundMu     sync.Mutex
	saOutboundCancel context.CancelFunc
	saOutboundDone   chan struct{}
)

func init() {
	// 只接线，不启动。生命周期由 app.Run 的 Start/Stop 持有应用上下文。
	saEnqueueOutbound = enqueueSAOutbound
}

// StartSAOutboundSender 启动可恢复的发件箱。重复调用无效果。
// 启动后立刻扫一遍，把上次没发出的 pending/failed 继续发出去。
func StartSAOutboundSender(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	saOutboundMu.Lock()
	defer saOutboundMu.Unlock()
	if saOutboundCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	saOutboundCancel = cancel
	saOutboundDone = done
	go func() {
		defer close(done)
		saOutboundLoop(ctx)
	}()
	log.Printf("superagent: 发件箱已启动")
}

// StopSAOutboundSender 停掉发送循环。库里的 pending/failed 留到下次启动再发。
func StopSAOutboundSender() {
	saOutboundMu.Lock()
	cancel, done := saOutboundCancel, saOutboundDone
	saOutboundCancel, saOutboundDone = nil, nil
	saOutboundMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func enqueueSAOutbound(req saOutboundReq) (model.SAOutbound, error) {
	if model.DB == nil {
		return model.SAOutbound{}, errors.New("superagent: database is not initialized")
	}
	if req.ThreadID == 0 || req.DedupeKey == "" || req.Text == "" {
		return model.SAOutbound{}, errors.New("superagent: invalid outbound request")
	}
	var out model.SAOutbound
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("dedupe_key = ?", req.DedupeKey).First(&out).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		out = model.SAOutbound{
			UserID:           req.UserID,
			ThreadID:         req.ThreadID,
			TaskID:           req.TaskID,
			Purpose:          req.Purpose,
			Text:             req.Text,
			ReplyToTriggerID: req.ReplyToTriggerID,
			DedupeKey:        req.DedupeKey,
			Status:           model.SAOutPending,
			CreatedAt:        time.Now(),
		}
		if err := tx.Create(&out).Error; err != nil {
			if !saIsDuplicateKey(err) {
				return err
			}
			// 并发入队撞上唯一键：返回已经落库的那一行。
			return tx.Where("dedupe_key = ?", req.DedupeKey).First(&out).Error
		}
		return nil
	})
	if err != nil {
		return model.SAOutbound{}, err
	}
	saWakeOutbound()
	return out, nil
}

func saWakeOutbound() {
	select {
	case saOutboundWake <- struct{}{}:
	default:
	}
}

func saIsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "duplicate entry")
}

func saOutboundLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		saSendPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-saOutboundWake:
		case <-ticker.C:
		}
	}
}

// saSendPending 扫一遍到期的 pending/failed。
// 同一 Thread 内，更早的行还是 pending、或 failed 且 attempts<3 时，后面的行等待。
// 已经有 sent_ref（或 status=sent）的不重发。
func saSendPending(ctx context.Context) {
	if model.DB == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []model.SAOutbound
	now := time.Now()
	err := model.DB.Where(
		"status IN ? AND (sent_ref = '' OR sent_ref IS NULL) AND (next_attempt_at IS NULL OR next_attempt_at <= ?)",
		[]string{model.SAOutPending, model.SAOutFailed}, now,
	).Order("id asc").Find(&rows).Error
	if err != nil {
		log.Printf("superagent: 扫描发件箱失败: %v", err)
		return
	}
	for _, out := range rows {
		if ctx.Err() != nil {
			return
		}
		if out.SentRef != "" || out.Status == model.SAOutSent {
			continue
		}
		if saOutboundBlocked(out) {
			continue
		}
		var thread model.SAThread
		if err := model.DB.First(&thread, out.ThreadID).Error; err != nil {
			saMarkOutboundFailure(out, fmt.Errorf("thread %d: %w", out.ThreadID, err))
			continue
		}
		ch := saChannels[thread.Source]
		if ch == nil {
			saMarkOutboundFailure(out, errors.New("no channel for source "+thread.Source))
			continue
		}
		var trig *model.SATrigger
		if out.ReplyToTriggerID != 0 {
			var t model.SATrigger
			if model.DB.First(&t, out.ReplyToTriggerID).Error == nil {
				trig = &t
			}
		}
		ref, err := ch.Send(ctx, thread, out, trig)
		if err != nil {
			var pe saPermanentError
			if errors.As(err, &pe) {
				reason := clipRunes(pe.Error(), 240)
				model.DB.Model(&out).Updates(map[string]any{
					"status":      model.SAOutDropped,
					"drop_reason": reason,
					"last_error":  clipRunes(err.Error(), 480),
				})
			} else {
				saMarkOutboundFailure(out, err)
			}
			continue
		}
		sentAt := time.Now()
		model.DB.Model(&out).Updates(map[string]any{
			"status":     model.SAOutSent,
			"sent_ref":   ref,
			"sent_at":    sentAt,
			"last_error": "",
		})
	}
}

// saOutboundBlocked 报告同一 Thread 里还有更早、尚未发出且失败未满 3 次的行。
func saOutboundBlocked(out model.SAOutbound) bool {
	var blocker model.SAOutbound
	err := model.DB.Where(
		"thread_id = ? AND id < ? AND (sent_ref = '' OR sent_ref IS NULL) AND (status = ? OR (status = ? AND attempts < ?))",
		out.ThreadID, out.Id, model.SAOutPending, model.SAOutFailed, 3,
	).Order("id asc").Limit(1).Take(&blocker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	// 查询失败时先不发，避免把顺序打乱。
	return true
}

// saMarkOutboundFailure 记一次临时失败。5s 起翻倍，封顶 300s。
// 连续 3 次改为 failed，但仍然按退避继续重试（系统问题）。
func saMarkOutboundFailure(out model.SAOutbound, err error) {
	attempts := out.Attempts + 1
	status := model.SAOutPending
	if attempts >= 3 {
		status = model.SAOutFailed
	}
	shift := min(attempts-1, 6)
	delay := 5 * time.Second * time.Duration(uint(1)<<shift)
	if delay > 300*time.Second {
		delay = 300 * time.Second
	}
	next := time.Now().Add(delay)
	model.DB.Model(&out).Updates(map[string]any{
		"status":          status,
		"attempts":        attempts,
		"next_attempt_at": next,
		"last_error":      clipRunes(err.Error(), 480),
	})
}
