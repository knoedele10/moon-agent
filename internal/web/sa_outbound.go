package web

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"blog-my/internal/model"
	"gorm.io/gorm"
)

var saOutboundWake = make(chan struct{}, 1)
var (
	saOutboundMu      sync.Mutex
	saOutboundCancel  context.CancelFunc
	saOutboundRunning bool
)

func init() {
	saEnqueueOutbound = enqueueSAOutbound
	StartSAOutboundSender(context.Background())
}

// StartSAOutboundSender starts the durable outbox worker.  It is safe to call
// more than once; app.Run uses it alongside the other schedulers.
func StartSAOutboundSender(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	saOutboundMu.Lock()
	defer saOutboundMu.Unlock()
	if saOutboundRunning {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	saOutboundCancel = cancel
	saOutboundRunning = true
	go saOutboundLoop(ctx)
}

// StopSAOutboundSender lets the application shut down without leaving a
// goroutine behind. Pending rows stay in the database and resume on restart.
func StopSAOutboundSender() {
	saOutboundMu.Lock()
	cancel := saOutboundCancel
	saOutboundCancel = nil
	saOutboundRunning = false
	saOutboundMu.Unlock()
	if cancel != nil {
		cancel()
	}
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
		if err := tx.Where("dedupe_key = ?", req.DedupeKey).First(&out).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		out = model.SAOutbound{UserID: req.UserID, ThreadID: req.ThreadID, TaskID: req.TaskID, Purpose: req.Purpose, Text: req.Text, ReplyToTriggerID: req.ReplyToTriggerID, DedupeKey: req.DedupeKey, Status: model.SAOutPending, CreatedAt: time.Now()}
		return tx.Create(&out).Error
	})
	if err == nil {
		select {
		case saOutboundWake <- struct{}{}:
		default:
		}
	}
	return out, err
}

func saOutboundLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		saSendPending()
		select {
		case <-ctx.Done():
			return
		case <-saOutboundWake:
		case <-ticker.C:
		}
	}
}

func saSendPending() {
	if model.DB == nil {
		return
	}
	var rows []model.SAOutbound
	now := time.Now()
	model.DB.Where("status IN ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", []string{model.SAOutPending, model.SAOutFailed}, now).Order("id asc").Limit(100).Find(&rows)
	for _, out := range rows {
		var thread model.SAThread
		if model.DB.First(&thread, out.ThreadID).Error != nil {
			continue
		}
		// Preserve FIFO within a Thread while an earlier message is still
		// pending or retryable. Once that message has exhausted its retry
		// budget (status=failed, attempts>=3), allow later messages through.
		var previous []model.SAOutbound
		if err := model.DB.Where("thread_id = ? AND id < ?", out.ThreadID, out.Id).
			Order("id desc").Limit(32).Find(&previous).Error; err != nil {
			continue
		}
		blocked := false
		for _, prev := range previous {
			if prev.Status == model.SAOutPending || (prev.Status == model.SAOutFailed && prev.Attempts < 3) {
				blocked = true
				break
			}
		}
		if blocked {
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
		ref, err := ch.Send(context.Background(), thread, out, trig)
		if err != nil {
			var pe saPermanentError
			if errors.As(err, &pe) {
				model.DB.Model(&out).Updates(map[string]any{"status": model.SAOutDropped, "drop_reason": pe.Error(), "last_error": pe.Error()})
			} else {
				saMarkOutboundFailure(out, err)
			}
			continue
		}
		t := time.Now()
		model.DB.Model(&out).Updates(map[string]any{"status": model.SAOutSent, "sent_ref": ref, "sent_at": t, "last_error": ""})
	}
}

func saMarkOutboundFailure(out model.SAOutbound, err error) {
	attempts := out.Attempts + 1
	status := model.SAOutPending
	if attempts >= 3 {
		status = model.SAOutFailed
	}
	delay := 5 * time.Second * time.Duration(1<<min(attempts-1, 6))
	if delay > 300*time.Second {
		delay = 300 * time.Second
	}
	next := time.Now().Add(delay)
	model.DB.Model(&out).Updates(map[string]any{"status": status, "attempts": attempts, "next_attempt_at": next, "last_error": fmt.Sprint(err)})
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
