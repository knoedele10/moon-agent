package web

import (
	"context"
	"encoding/json"
	"fmt"
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
	// allowed 只描述 QQ_ALLOW_* 的集合语义。适配器不再用它过滤消息，
	// 见 TestQQAddressedSkipsAllowlistAndDoesNotDispatch。
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
	// inputs 记录前台轮次：适配器自己不跑轮次，普通消息只入库，轮次由判断环节决定。
	saSwap(t, &saRunFrontTurn, func(_ context.Context, _ model.SAThread, content string) (string, error) {
		sent.mu.Lock()
		inputs = append(inputs, content)
		sent.mu.Unlock()
		return "回复:" + content, nil
	})
	saSwap(t, &qqDispatch, func(_ qqEvent, text string) {
		sent.mu.Lock()
		sent.msgs = append(sent.msgs, text)
		sent.mu.Unlock()
	})
	return sent, &inputs
}

func TestQQHandleMessageBindsConversation(t *testing.T) {
	// /new 仍然只换这个聊天的前台会话，但只对过了准入的人生效；
	// 普通消息不再回退到旧白名单和直接跑轮次。
	setupSATestDB(t)
	t.Setenv("QQ_ALLOW_USERS", "200")
	t.Setenv("QQ_ALLOW_GROUPS", "300")
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", Model: "m1", Kind: KindChat, AllowUsers: qqIDSet("200"), AllowGroups: qqIDSet("300"), Timeout: time.Minute}

	renew := qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", Message: json.RawMessage(`"/new"`)}
	qqHandleMessage(cfg, renew)
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

	qqHandleMessage(cfg, renew)
	model.DB.Where("chat_key = ?", "private:200").First(&bindings[0])
	if bindings[0].ConversationID == first {
		t.Fatal("/new 应换到新会话")
	}

	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", Message: json.RawMessage(`"hi"`)})
	group := `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"sender":{"nickname":"小明"},"message":[{"type":"text","data":{"text":"随便聊"}}]}`
	qqHandleMessage(cfg, qqTestEvent(t, group))
	group = `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"sender":{"nickname":"小明"},"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":"在吗"}}]}`
	qqHandleMessage(cfg, qqTestEvent(t, group))
	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "999", Message: json.RawMessage(`"hi"`)})

	if len(*inputs) != 0 {
		t.Fatalf("适配器不应再直接跑轮次: %q", *inputs)
	}
	if got := sent.all(); len(got) != 2 || got[0] != "好的，已开始新的对话。" || got[1] != "好的，已开始新的对话。" {
		t.Fatalf("sent = %q", got)
	}
}

// 本地命令也要过准入：陌生人的 /help、/new 不回话、不重置会话，只按普通消息入库；
// 群里能 @ 的名单外成员可以 /help，但不能 /new 重置大家共用的前台会话。
func TestQQCommandsRequireAdmission(t *testing.T) {
	setupSATestDB(t)
	model.DB.Create(&model.User{Id: "u1", Username: "owner"})
	t.Setenv("QQ_OWNER", "42")
	t.Setenv("QQ_ALLOW_USERS", "200")
	t.Setenv("QQ_ALLOW_GROUPS", "300") // 默认策略：群里名单外 @ 为 explicit
	sent, _ := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", Model: "m1", Kind: KindChat, Timeout: time.Minute}
	priv := func(user, text string, id int) qqEvent {
		return qqTestEvent(t, fmt.Sprintf(`{"post_type":"message","message_type":"private","self_id":100,"user_id":%s,"message_id":%d,"message":[{"type":"text","data":{"text":%q}}]}`, user, id, text))
	}
	group := func(user, text string, id int) qqEvent {
		return qqTestEvent(t, fmt.Sprintf(`{"post_type":"message","message_type":"group","self_id":100,"user_id":%s,"group_id":300,"message_id":%d,"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":%q}}]}`, user, id, text))
	}

	qqHandleMessage(cfg, priv("999", "/help", 1))
	qqHandleMessage(cfg, priv("999", "/new", 2))
	if got := sent.all(); len(got) != 0 {
		t.Fatalf("陌生人的命令不应回话: %q", got)
	}
	var n int64
	model.DB.Model(&model.SATrigger{}).Where("actor_id = ?", "999").Count(&n)
	if n != 2 {
		t.Fatalf("被拒的命令应按普通消息入库交给 gate: %d", n)
	}
	model.DB.Model(&model.QQChatBinding{}).Count(&n)
	if n != 0 {
		t.Fatal("陌生人的 /new 不应建会话")
	}

	qqHandleMessage(cfg, group("777", "/help", 3)) // 名单外，但群里 @ 是 explicit
	qqHandleMessage(cfg, group("777", "/new", 4))  // 不能重置群会话
	qqHandleMessage(cfg, group("200", "/new", 5))  // 名单内可以
	qqHandleMessage(cfg, priv("42", "/new", 6))    // 主人私聊可以（不在名单里也行）
	got := sent.all()
	if len(got) != 3 || got[0] != qqHelpText || got[1] != "好的，已开始新的对话。" || got[2] != "好的，已开始新的对话。" {
		t.Fatalf("sent = %q", got)
	}
	model.DB.Model(&model.SATrigger{}).Where("actor_id = ? AND text = ?", "777", "/new").Count(&n)
	if n != 1 {
		t.Fatal("群里名单外成员的 /new 应按普通消息入库")
	}
}

func TestQQOneBotWSRoundTrip(t *testing.T) {
	setupSATestDB(t)
	_, _ = stubQQ(t)
	stubSAIngressSideEffects(t)
	qqDispatch = qqSend // 走真实发送，验证动作帧
	// 最后注册、最先执行：先等处理中的入站消息结束，再还原假实现、拆库。
	t.Cleanup(qqHandlers.Wait)
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
	// 陌生人（999 不在名单里）的 /help 不回帧；名单内（200）的 /help 走适配器本地回复。
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"message","message_type":"private","self_id":100,"user_id":999,"message_id":6,"message":[{"type":"text","data":{"text":"/help"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"message","message_type":"private","self_id":100,"user_id":200,"message_id":7,"message":[{"type":"text","data":{"text":"/help"}}]}`)); err != nil {
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
	if frame.Action != "send_private_msg" || frame.Params.UserID != 200 || len(frame.Params.Message) != 1 || frame.Params.Message[0].Data["text"] != qqHelpText {
		t.Fatalf("frame = %+v", frame)
	}
	// 普通消息不直接回帧：没有 SA 表时也不再跑一轮。
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"message","message_type":"private","self_id":100,"user_id":999,"message_id":8,"message":[{"type":"text","data":{"text":"你好"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"message_id":9,"message":[{"type":"text","data":{"text":"随便聊"}}]}`)); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var extra map[string]any
	if err := ws.ReadJSON(&extra); err == nil {
		t.Fatalf("普通消息不应直接回帧: %+v", extra)
	}
}

func stubSAIngressSideEffects(t *testing.T) {
	t.Helper()
	saSwap(t, &saFanOut, func(context.Context, model.SATrigger) ([]uint, error) { return nil, nil })
	saSwap(t, &saEvaluate, func(context.Context, uint) {})
}

func TestQQAddressedGroupUnmentionedIngestsQueued(t *testing.T) {
	setupSATestDB(t)
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	// 群不在白名单、人也没 @。旧适配器会直接丢掉；现在必须入库且不回话。
	cfg := qqConfig{UserID: "u1", AllowUsers: qqIDSet("1"), AllowGroups: qqIDSet("1")}
	ev := qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"message_id":42,"sender":{"nickname":"小明"},"message":[{"type":"text","data":{"text":"随便聊"}}]}`)
	qqHandleMessage(cfg, ev)
	qqHandleMessage(cfg, ev)

	if len(*inputs) != 0 || len(sent.all()) != 0 {
		t.Fatalf("未 @ 的群消息不应跑轮次或回话 inputs=%q sent=%q", *inputs, sent.all())
	}
	var trigs []model.SATrigger
	if err := model.DB.Order("id asc").Find(&trigs).Error; err != nil {
		t.Fatal(err)
	}
	if len(trigs) != 1 {
		t.Fatalf("同一消息应只入库一次: %+v", trigs)
	}
	tr := trigs[0]
	if tr.Addressed != model.SAAddressedNone || tr.Disposition != model.SADispQueued {
		t.Fatalf("addressed=%s disposition=%s", tr.Addressed, tr.Disposition)
	}
	if tr.Source != model.SASourceQQ || tr.SourceMsgID != "42" || tr.Text != "随便聊" || tr.ActorID != "201" || tr.ActorName != "小明" || tr.ActorKind != model.SAActorHuman {
		t.Fatalf("trigger = %+v", tr)
	}
	var th model.SAThread
	if err := model.DB.First(&th, tr.ThreadID).Error; err != nil {
		t.Fatal(err)
	}
	if th.ThreadKey != "qq:group:300" || th.Source != model.SASourceQQ || th.Label != "QQ 群 300" || th.UserID != "u1" {
		t.Fatalf("thread = %+v", th)
	}
}

func TestQQAddressedPrivateDirectAndMention(t *testing.T) {
	setupSATestDB(t)
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", AllowUsers: map[string]bool{}, AllowGroups: map[string]bool{}}

	qqHandleMessage(cfg, qqTestEvent(t, `{"post_type":"message","message_type":"private","self_id":100,"user_id":200,"message_id":7,"sender":{"nickname":"阿花"},"message":[{"type":"text","data":{"text":"在吗"}}]}`))
	qqHandleMessage(cfg, qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"message_id":8,"sender":{"nickname":"小明"},"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":" 看一下 "}}]}`))

	if len(*inputs) != 0 || len(sent.all()) != 0 {
		t.Fatalf("普通消息不应直接回话 inputs=%q sent=%q", *inputs, sent.all())
	}
	var priv, mentioned model.SATrigger
	if err := model.DB.Where("source_msg_id = ?", "7").First(&priv).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Where("source_msg_id = ?", "8").First(&mentioned).Error; err != nil {
		t.Fatal(err)
	}
	if priv.Addressed != model.SAAddressedDirect || priv.Disposition != model.SADispQueued || priv.Text != "在吗" {
		t.Fatalf("private = %+v", priv)
	}
	if mentioned.Addressed != model.SAAddressedMention || mentioned.Disposition != model.SADispQueued || mentioned.Text != "看一下" {
		t.Fatalf("mention = %+v", mentioned)
	}
	var group model.SAThread
	if err := model.DB.First(&group, mentioned.ThreadID).Error; err != nil {
		t.Fatal(err)
	}
	if group.ThreadKey != "qq:group:300" {
		t.Fatalf("group thread = %+v", group)
	}
	var direct model.SAThread
	if err := model.DB.First(&direct, priv.ThreadID).Error; err != nil {
		t.Fatal(err)
	}
	if direct.ThreadKey != "qq:private:200" || direct.Label != "QQ · 阿花" {
		t.Fatalf("private thread = %+v", direct)
	}
}

func TestQQAddressedSkipsAllowlistAndDoesNotDispatch(t *testing.T) {
	setupSATestDB(t)
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", AllowUsers: qqIDSet("1"), AllowGroups: qqIDSet("1")}

	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "999", MessageID: "3", Message: json.RawMessage(`"不在名单"`)})
	qqHandleMessage(cfg, qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"user_id":999,"group_id":888,"message_id":4,"message":[{"type":"at","data":{"qq":"100"}},{"type":"text","data":{"text":"群也不在名单"}}]}`))
	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "999", MessageID: "5", Message: json.RawMessage(`"/help"`)})

	if len(*inputs) != 0 {
		t.Fatalf("不应直接跑轮次: %q", *inputs)
	}
	// 名单外的人连 /help 也不回话：本地命令同样要过准入，被拒就按普通消息入库。
	if got := sent.all(); len(got) != 0 {
		t.Fatalf("名单外的人不应收到任何本地回复: %q", got)
	}
	var n int64
	model.DB.Model(&model.SATrigger{}).Where("source_msg_id IN ?", []string{"3", "4", "5"}).Count(&n)
	if n != 3 {
		t.Fatalf("名单外的消息（含被拒的 /help）也应入库, count=%d", n)
	}
	var priv model.SATrigger
	if err := model.DB.Where("source_msg_id = ?", "3").First(&priv).Error; err != nil {
		t.Fatal(err)
	}
	if priv.Addressed != model.SAAddressedDirect || priv.Disposition != model.SADispQueued {
		t.Fatalf("allowlist private = %+v", priv)
	}
}

func TestQQAddressedDropsOwnMessage(t *testing.T) {
	setupSATestDB(t)
	sent, _ := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", AllowUsers: qqIDSet("*"), AllowGroups: qqIDSet("*")}
	qqHandleMessage(cfg, qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "100", MessageID: "1", Message: json.RawMessage(`"我自己说的"`)})
	if len(sent.all()) != 0 {
		t.Fatalf("自己的消息不应回复: %q", sent.all())
	}
	var n int64
	model.DB.Model(&model.SATrigger{}).Count(&n)
	if n != 0 {
		t.Fatalf("自己的消息不应入库, count=%d", n)
	}
}

func TestQQReplyToMsgIDArrayAndCQ(t *testing.T) {
	ev := qqTestEvent(t, `{"self_id":100,"message":[{"type":"reply","data":{"id":88}},{"type":"text","data":{"text":"ok"}}]}`)
	if got := qqReplyToID(ev); got != "88" {
		t.Fatalf("array reply id = %q", got)
	}
	ev = qqTestEvent(t, `{"self_id":100,"message":"[CQ:reply,id=77][CQ:at,qq=100] 收到"}`)
	if got := qqReplyToID(ev); got != "77" {
		t.Fatalf("cq reply id = %q", got)
	}
	ev = qqTestEvent(t, `{"self_id":100,"raw_message":"[CQ:reply,id=66] hi","message":""}`)
	if got := qqReplyToID(ev); got != "66" {
		t.Fatalf("raw reply id = %q", got)
	}
	ev = qqTestEvent(t, `{"self_id":100,"message":[{"type":"text","data":{"text":"nope"}}]}`)
	if got := qqReplyToID(ev); got != "" {
		t.Fatalf("plain message reply id = %q", got)
	}
}

func TestQQNewSwitchesForegroundSession(t *testing.T) {
	setupSATestDB(t)
	t.Setenv("QQ_ALLOW_USERS", "200") // /new 需要过准入
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1", Model: "m1", Kind: KindChat}

	hi := qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", MessageID: "1", Message: json.RawMessage(`"hi"`)}
	hi.Sender.Nickname = "阿花"
	qqHandleMessage(cfg, hi)

	var thread model.SAThread
	if err := model.DB.Where("thread_key = ?", "qq:private:200").First(&thread).Error; err != nil {
		t.Fatal(err)
	}
	firstConv := thread.ConversationID
	if firstConv == 0 {
		t.Fatal("ingest should open a foreground conversation")
	}

	renew := qqEvent{PostType: "message", MessageType: "private", SelfID: "100", UserID: "200", Message: json.RawMessage(`"/new"`)}
	renew.Sender.Nickname = "阿花"
	qqHandleMessage(cfg, renew)
	var again model.SAThread
	if err := model.DB.Where("thread_key = ?", "qq:private:200").First(&again).Error; err != nil {
		t.Fatal(err)
	}
	if again.Id != thread.Id {
		t.Fatalf("/new changed the thread: %d -> %d", thread.Id, again.Id)
	}
	if again.ConversationID == 0 || again.ConversationID == firstConv {
		t.Fatalf("foreground conversation = %d, was %d", again.ConversationID, firstConv)
	}
	var conv model.Conversation
	if err := model.DB.First(&conv, again.ConversationID).Error; err != nil {
		t.Fatal(err)
	}
	if conv.UserID != "u1" || conv.Kind != KindChat || conv.Model != "m1" || conv.Title != "QQ · 阿花" {
		t.Fatalf("new foreground = %+v", conv)
	}
	var binding model.QQChatBinding
	if err := model.DB.Where("chat_key = ?", "private:200").First(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.ConversationID != again.ConversationID {
		t.Fatalf("binding conversation = %d, thread = %d", binding.ConversationID, again.ConversationID)
	}
	var n int64
	model.DB.Model(&model.SAThread{}).Count(&n)
	if n != 1 {
		t.Fatalf("threads = %d", n)
	}
	if len(*inputs) != 0 {
		t.Fatalf("/new 不应跑轮次: %q", *inputs)
	}
	if got := sent.all(); len(got) != 1 || got[0] != "好的，已开始新的对话。" {
		t.Fatalf("sent = %q", got)
	}
}

func TestQQReplyToMsgIDStoredOnIngest(t *testing.T) {
	setupSATestDB(t)
	sent, inputs := stubQQ(t)
	stubSAIngressSideEffects(t)
	cfg := qqConfig{UserID: "u1"}
	// 群里引用回复但没 @：Addressed 仍是 none，ReplyToMsgID 要带出来，且不回话。
	qqHandleMessage(cfg, qqTestEvent(t, `{"post_type":"message","message_type":"group","self_id":100,"user_id":201,"group_id":300,"message_id":9,"message":[{"type":"reply","data":{"id":"55"}},{"type":"text","data":{"text":"收到"}}]}`))
	if len(*inputs) != 0 || len(sent.all()) != 0 {
		t.Fatalf("引用回复不应直接回话 inputs=%q sent=%q", *inputs, sent.all())
	}
	var tr model.SATrigger
	if err := model.DB.Where("source_msg_id = ?", "9").First(&tr).Error; err != nil {
		t.Fatal(err)
	}
	if tr.ReplyToMsgID != "55" || tr.Addressed != model.SAAddressedNone || tr.Disposition != model.SADispQueued || tr.Text != "收到" {
		t.Fatalf("trigger = %+v", tr)
	}
}
