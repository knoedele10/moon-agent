package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

func qqTestEvent(t *testing.T, raw string) qqEvent {
	t.Helper()
	var ev qqEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestQQExtractTextArrayAndCQ(t *testing.T) {
	ev := qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"user_id":"200","group_id":300,
		"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":" 你好 "}},{"type":"image","data":{"file":"x"}}]}`)
	text, at := qqExtractText(ev)
	if text != "你好" || !at {
		t.Fatalf("array: got %q at=%v", text, at)
	}
	if ev.UserID != "200" || ev.GroupID != "300" || ev.chatKey() != "group:300" {
		t.Fatalf("ids: %+v", ev)
	}

	ev = qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"message":"[CQ:at,qq=999] hi &#91;x&#93;"}`)
	text, at = qqExtractText(ev)
	if text != "hi [x]" || at {
		t.Fatalf("cq other at: got %q at=%v", text, at)
	}
	ev = qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"message":"[CQ:at,qq=100] 在吗"}`)
	if text, at = qqExtractText(ev); text != "在吗" || !at {
		t.Fatalf("cq self at: got %q at=%v", text, at)
	}
}

func TestQQAllowlist(t *testing.T) {
	cfg := qqConfig{AllowUsers: qqIDSet("1, 2"), AllowGroups: qqIDSet("")}
	if !cfg.allowed(qqEvent{MessageType: "private", UserID: "2"}) {
		t.Fatal("白名单用户应放行")
	}
	if cfg.allowed(qqEvent{MessageType: "private", UserID: "3"}) {
		t.Fatal("非白名单用户应拒绝")
	}
	if cfg.allowed(qqEvent{MessageType: "group", GroupID: "9", UserID: "1"}) {
		t.Fatal("群白名单为空应拒绝")
	}
	cfg.AllowGroups = qqIDSet("*")
	if !cfg.allowed(qqEvent{MessageType: "group", GroupID: "9"}) {
		t.Fatal("* 应放行所有群")
	}
}

func TestQQSplit(t *testing.T) {
	long := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 30)
	parts := qqSplit(long, 40)
	if len(parts) != 2 || parts[0] != strings.Repeat("a", 30) || parts[1] != strings.Repeat("b", 30) {
		t.Fatalf("split: %q", parts)
	}
	if got := qqSplit("短", 40); len(got) != 1 {
		t.Fatalf("short: %q", got)
	}
}

func setupQQTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "qq.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Conversation{}, &model.Message{}, &model.QQChatBinding{}); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sqlDB, _ := db.DB(); sqlDB.Close() })
}

type qqSent struct {
	mu   sync.Mutex
	msgs []string
}

func (s *qqSent) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func stubQQ(t *testing.T) (*qqSent, *[]string) {
	t.Helper()
	sent := &qqSent{}
	var inputs []string
	oldRun, oldDispatch := qqRunTurn, qqDispatch
	qqRunTurn = func(_ context.Context, conv *model.Conversation, _ string, content string) (string, error) {
		inputs = append(inputs, content)
		saveChatMessage(conv.Id, "user", content)
		return "回复:" + content, nil
	}
	qqDispatch = func(_ qqEvent, text string) {
		sent.mu.Lock()
		sent.msgs = append(sent.msgs, text)
		sent.mu.Unlock()
	}
	t.Cleanup(func() { qqRunTurn, qqDispatch = oldRun, oldDispatch })
	return sent, &inputs
}

func TestQQHandleMessageBindsConversation(t *testing.T) {
	setupQQTestDB(t)
	sent, inputs := stubQQ(t)
	cfg := qqConfig{UserID: "u1", Model: "m1", Kind: KindChat, AllowUsers: qqIDSet("200"), AllowGroups: qqIDSet("300"), Timeout: time.Minute}

	priv := qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", Message: json.RawMessage(`"hi"`)}
	qqHandleMessage(cfg, priv)
	qqHandleMessage(cfg, priv)
	var bindings []model.QQChatBinding
	model.DB.Find(&bindings)
	if len(bindings) != 1 || bindings[0].ChatKey != "private:200" {
		t.Fatalf("同一私聊应只绑一个会话: %+v", bindings)
	}
	first := bindings[0].ConversationID
	var conv model.Conversation
	model.DB.First(&conv, first)
	if conv.Kind != KindChat || conv.Model != "m1" || conv.UserID != "u1" {
		t.Fatalf("会话配置不对: %+v", conv)
	}

	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", Message: json.RawMessage(`"/new"`)})
	model.DB.Where("chat_key = ?", "private:200").First(&bindings[0])
	if bindings[0].ConversationID == first {
		t.Fatal("/new 应换到新会话")
	}

	// 群里不 @ 不回；@ 了带上说话人。
	group := `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"sender":{"nickname":"小明"},"message":[{"type":"text","data":{"text":"随便聊"}}]}`
	qqHandleMessage(cfg, qqTestEvent(t, group))
	group = `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"sender":{"nickname":"小明"},"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":"在吗"}}]}`
	qqHandleMessage(cfg, qqTestEvent(t, group))
	// 不在白名单的私聊不回。
	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "999", Message: json.RawMessage(`"hi"`)})

	want := []string{"hi", "hi", "[小明(201)]: 在吗"}
	if strings.Join(*inputs, "|") != strings.Join(want, "|") {
		t.Fatalf("inputs = %q", *inputs)
	}
	if got := sent.all(); len(got) != 4 || got[2] != "好的，已开始新的对话。" {
		t.Fatalf("sent = %q", got)
	}
}

func TestQQOneBotWSRoundTrip(t *testing.T) {
	setupQQTestDB(t)
	_, _ = stubQQ(t)
	qqDispatch = qqSend // 走真实发送，验证动作帧
	gin.SetMode(gin.TestMode)
	t.Setenv("QQ_ONEBOT_ENABLED", "1")
	t.Setenv("QQ_ONEBOT_TOKEN", "secret")
	t.Setenv("QQ_BOT_USER_ID", "u1")
	t.Setenv("QQ_BOT_MODEL", "m1")
	t.Setenv("QQ_ALLOW_USERS", "200")

	r := gin.New()
	r.GET("/onebot/v11/ws", qqOneBotWS)
	srv := httptest.NewServer(r)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/onebot/v11/ws"

	if _, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer wrong"}}); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 token 应被拒: %v", err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer secret"}, "X-Self-ID": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"meta_event","meta_event_type":"heartbeat","self_id":100}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"message","message_type":"private","self_id":100,"user_id":200,"message_id":7,"message":[{"type":"text","data":{"text":"你好"}}]}`)); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame struct {
		Action string `json:"action"`
		Params struct {
			UserID  int64 `json:"user_id"`
			Message []struct {
				Type string            `json:"type"`
				Data map[string]string `json:"data"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := ws.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.Action != "send_private_msg" || frame.Params.UserID != 200 || len(frame.Params.Message) != 1 || frame.Params.Message[0].Data["text"] != "回复:你好" {
		t.Fatalf("frame = %+v", frame)
	}
}
