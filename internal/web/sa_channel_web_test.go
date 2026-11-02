package web

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"blog-my/internal/model"
)

func TestSAChannelWebSendReturnsMessageID(t *testing.T) {
	db := setupSATestDB(t)
	conv := model.Conversation{UserID: "u1", Title: "网页", Model: "m"}
	if err := db.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	thread := model.SAThread{UserID: "u1", ThreadKey: "web:conv:1", Source: model.SASourceWeb, ConversationID: conv.Id, Label: "网页"}
	ref, err := (saWebChannel{}).Send(context.Background(), thread, model.SAOutbound{Text: "已转交", Purpose: model.SAPurposeReply}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := strconv.ParseUint(ref, 10, 64)
	if err != nil || id == 0 {
		t.Fatalf("sent_ref = %q", ref)
	}
	var m model.Message
	if err := db.First(&m, id).Error; err != nil {
		t.Fatal(err)
	}
	if m.Role != "assistant" || m.Content != "已转交" || m.ConversationID != conv.Id {
		t.Fatalf("message = %+v", m)
	}
	ref2, err := (saWebChannel{}).Send(context.Background(), thread, model.SAOutbound{Text: "第二条"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref2 == ref {
		t.Fatalf("两条回复应有不同的 message id: %s", ref)
	}
}

func TestSAChannelWebSendMissingConversationIsPermanent(t *testing.T) {
	setupSATestDB(t)
	_, err := (saWebChannel{}).Send(context.Background(), model.SAThread{ThreadKey: "web:conv:0", Source: model.SASourceWeb}, model.SAOutbound{Text: "hi"}, nil)
	var pe saPermanentError
	if !errors.As(err, &pe) {
		t.Fatalf("missing conversation should be permanent, got %v", err)
	}
}
