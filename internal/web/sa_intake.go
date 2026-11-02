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

// saIngest is the single write boundary for inbound events.  The event is
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
	var result saIngestResult
	err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var thread model.SAThread
		if err := tx.Where("thread_key = ?", in.ThreadKey).First(&thread).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			thread = model.SAThread{UserID: in.UserID, ThreadKey: in.ThreadKey, Source: in.Source, Label: in.ThreadLabel, ConversationID: in.ConversationID, EvalState: "idle"}
			if thread.ConversationID == 0 && in.Source == model.SASourceQQ {
				// Migrate the legacy QQ binding when one exists.  The binding is
				// deliberately read once, at Thread creation; subsequent /new
				// commands only change the old binding and do not fork Threads.
				chatKey := strings.TrimPrefix(in.ThreadKey, "qq:")
				var binding model.QQChatBinding
				if tx.Migrator().HasTable(&model.QQChatBinding{}) &&
					tx.Where("chat_key = ? AND user_id = ?", chatKey, in.UserID).First(&binding).Error == nil {
					thread.ConversationID = binding.ConversationID
				}
				if thread.ConversationID == 0 {
					kind := normalizeKind(os.Getenv("QQ_BOT_KIND"))
					conv := model.Conversation{UserID: in.UserID, Title: in.ThreadLabel, Kind: kind}
					if err := tx.Create(&conv).Error; err != nil {
						return err
					}
					thread.ConversationID = conv.Id
				}
			}
			if err := tx.Create(&thread).Error; err != nil {
				return err
			}
		} else if in.Source == model.SASourceQQ && tx.Migrator().HasTable(&model.QQChatBinding{}) {
			// /new changes the foreground conversation while preserving the
			// Thread identity. Follow that binding on the next inbound event.
			var binding model.QQChatBinding
			chatKey := strings.TrimPrefix(in.ThreadKey, "qq:")
			if tx.Where("chat_key = ? AND user_id = ?", chatKey, in.UserID).First(&binding).Error == nil && binding.ConversationID != 0 && binding.ConversationID != thread.ConversationID {
				if err := tx.Model(&thread).Update("conversation_id", binding.ConversationID).Error; err != nil {
					return err
				}
				thread.ConversationID = binding.ConversationID
			}
		}
		var trig model.SATrigger
		if err := tx.Where("source = ? AND source_msg_id = ?", in.Source, in.SourceMsgID).First(&trig).Error; err == nil {
			result = saIngestResult{Trigger: trig, Thread: thread, Duplicate: true}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		trig = model.SATrigger{UserID: in.UserID, ThreadID: thread.Id, Source: in.Source, SourceMsgID: in.SourceMsgID, ReplyToMsgID: in.ReplyToMsgID, ActorID: in.ActorID, ActorName: in.ActorName, ActorKind: in.ActorKind, Addressed: in.Addressed, Text: in.Text, Raw: in.Raw, Disposition: model.SADispQueued, CreatedAt: time.Now()}
		if in.Inline {
			trig.Disposition = model.SADispInline
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
		model.DB.Model(&model.SATrigger{}).Where("id = ?", result.Trigger.Id).Update("disposition", model.SADispFannedOut)
		result.Trigger.Disposition = model.SADispFannedOut
		return result, nil
	}
	if !in.Inline {
		go saEvaluate(context.Background(), result.Thread.Id)
	}
	return result, nil
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
