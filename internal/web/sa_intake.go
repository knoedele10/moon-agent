package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"blog-my/internal/model"
	"gorm.io/gorm"
)

// saIngest is the single write boundary for inbound events. The event is
// persisted before any routing work is started, so a restart can safely pick
// up queued triggers.
func init() { saIngest = ingestSATrigger }

func ingestSATrigger(ctx context.Context, in saTriggerIn) (saIngestResult, error) {
	if model.DB == nil {
		return saIngestResult{}, errors.New("superagent: database is not initialized")
	}
	if strings.TrimSpace(in.Source) == "" || strings.TrimSpace(in.SourceMsgID) == "" || strings.TrimSpace(in.ThreadKey) == "" {
		return saIngestResult{}, errors.New("superagent: source, source message id and thread key are required")
	}
	// Two deliveries can pass the lookup together. The loser hits the unique
	// key; retry once the winner has committed and treat it as a duplicate.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := ingestSATriggerOnce(ctx, in)
		if err == nil || !saIsDuplicateKey(err) {
			return result, err
		}
		lastErr = err
	}
	return saIngestResult{}, lastErr
}

func ingestSATriggerOnce(ctx context.Context, in saTriggerIn) (saIngestResult, error) {
	var result saIngestResult
	err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing model.SATrigger
		err := tx.Where("source = ? AND source_msg_id = ?", in.Source, in.SourceMsgID).First(&existing).Error
		if err == nil {
			thread, err := saThreadByID(tx, existing.ThreadID)
			if err != nil {
				return err
			}
			result = saIngestResult{Trigger: existing, Thread: thread, Duplicate: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		thread, err := saEnsureThread(tx, in)
		if err != nil {
			return err
		}
		disp := model.SADispQueued
		if in.Inline {
			disp = model.SADispInline
		}
		trig := model.SATrigger{
			UserID:       in.UserID,
			ThreadID:     thread.Id,
			Source:       in.Source,
			SourceMsgID:  in.SourceMsgID,
			ReplyToMsgID: in.ReplyToMsgID,
			ActorID:      in.ActorID,
			ActorName:    in.ActorName,
			ActorKind:    in.ActorKind,
			Addressed:    in.Addressed,
			Text:         in.Text,
			Raw:          in.Raw,
			Disposition:  disp,
			CreatedAt:    time.Now(),
		}
		if err := tx.Create(&trig).Error; err != nil {
			return err
		}
		result = saIngestResult{Trigger: trig, Thread: thread}
		return nil
	})
	if err != nil {
		return saIngestResult{}, err
	}
	if result.Duplicate || result.Trigger.Id == 0 {
		return result, nil
	}
	claimed, err := saFanOut(ctx, result.Trigger)
	if err != nil {
		return result, err
	}
	result.FannedOut = claimed
	if len(claimed) > 0 {
		if err := model.DB.WithContext(ctx).Model(&model.SATrigger{}).Where("id = ?", result.Trigger.Id).Update("disposition", model.SADispFannedOut).Error; err != nil {
			return result, err
		}
		result.Trigger.Disposition = model.SADispFannedOut
		return result, nil
	}
	// Inline callers run the foreground turn themselves. Everyone else is
	// queued for saEvaluate; the gate, not the intake, decides who is answered.
	if !in.Inline {
		if result.Trigger.Disposition != model.SADispQueued {
			if err := model.DB.WithContext(ctx).Model(&model.SATrigger{}).Where("id = ?", result.Trigger.Id).Update("disposition", model.SADispQueued).Error; err != nil {
				return result, err
			}
			result.Trigger.Disposition = model.SADispQueued
		}
		saEvaluate(ctx, result.Thread.Id)
	}
	return result, nil
}

// saEnsureThread finds the Thread for this key or creates it. QQ bindings are
// copied onto the new row once; afterwards sa_thread.conversation_id is the
// foreground session (/new changes that column, and this function does not
// follow qq_chat_binding anymore).
func saEnsureThread(tx *gorm.DB, in saTriggerIn) (model.SAThread, error) {
	var thread model.SAThread
	err := tx.Where("thread_key = ?", in.ThreadKey).First(&thread).Error
	if err == nil {
		if thread.ConversationID == 0 && in.Source == model.SASourceQQ {
			id, err := saAdoptQQForeground(tx, in)
			if err != nil {
				return model.SAThread{}, err
			}
			if err := tx.Model(&thread).Update("conversation_id", id).Error; err != nil {
				return model.SAThread{}, err
			}
			thread.ConversationID = id
		}
		return thread, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.SAThread{}, err
	}
	thread = model.SAThread{
		UserID:         in.UserID,
		ThreadKey:      in.ThreadKey,
		Source:         in.Source,
		Label:          in.ThreadLabel,
		ConversationID: in.ConversationID,
		EvalState:      "idle",
	}
	if thread.ConversationID == 0 && in.Source == model.SASourceQQ {
		id, err := saAdoptQQForeground(tx, in)
		if err != nil {
			return model.SAThread{}, err
		}
		thread.ConversationID = id
	}
	if err := tx.Create(&thread).Error; err != nil {
		return model.SAThread{}, err
	}
	return thread, nil
}

// saAdoptQQForeground reuses the legacy qq_chat_binding conversation when the
// row still points at a live session for this user. Otherwise it opens the
// foreground session with the adapter's title and QQ_BOT_KIND (default chat).
func saAdoptQQForeground(tx *gorm.DB, in saTriggerIn) (uint, error) {
	chatKey := strings.TrimPrefix(in.ThreadKey, "qq:")
	if tx.Migrator().HasTable(&model.QQChatBinding{}) {
		var binding model.QQChatBinding
		err := tx.Where("chat_key = ? AND user_id = ?", chatKey, in.UserID).First(&binding).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
		if err == nil && binding.ConversationID != 0 {
			var conv model.Conversation
			err = tx.Where("id = ? AND user_id = ?", binding.ConversationID, in.UserID).First(&conv).Error
			if err == nil {
				return conv.Id, nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return 0, err
			}
		}
	}
	conv := model.Conversation{UserID: in.UserID, Title: in.ThreadLabel, Kind: qqForegroundKind()}
	if err := tx.Create(&conv).Error; err != nil {
		return 0, err
	}
	return conv.Id, nil
}

// qqForegroundKind matches the OneBot adapter. An empty QQ_BOT_KIND is chat;
// normalizeKind would otherwise turn that empty string into work.
func qqForegroundKind() string {
	if k := strings.TrimSpace(os.Getenv("QQ_BOT_KIND")); k != "" {
		return normalizeKind(k)
	}
	return KindChat
}

func saThreadByID(tx *gorm.DB, id uint) (model.SAThread, error) {
	var thread model.SAThread
	err := tx.First(&thread, id).Error
	return thread, err
}

func saIsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "duplicate entry") || strings.Contains(msg, "duplicated key")
}

func saThreadForID(id uint) (model.SAThread, error) {
	var t model.SAThread
	if model.DB == nil {
		return t, errors.New("database is not initialized")
	}
	err := model.DB.First(&t, id).Error
	return t, err
}

func saThreadLabel(source, key string) string {
	if key != "" {
		return fmt.Sprintf("%s · %s", source, key)
	}
	return source
}
