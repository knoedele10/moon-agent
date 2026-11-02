package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// saIntakeHooks replaces the other workstreams. Tests never call a model or QQ.
type saIntakeHooks struct {
	mu      sync.Mutex
	fanouts int
	evals   int
	threads []uint
	claimed []uint
	fanErr  error
	last    model.SATrigger
}

func (h *saIntakeHooks) install(t *testing.T) {
	t.Helper()
	saSwap(t, &saFanOut, func(_ context.Context, trig model.SATrigger) ([]uint, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.fanouts++
		h.last = trig
		if h.fanErr != nil {
			return nil, h.fanErr
		}
		return append([]uint(nil), h.claimed...), nil
	})
	saSwap(t, &saEvaluate, func(_ context.Context, threadID uint) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.evals++
		h.threads = append(h.threads, threadID)
	})
}

func (h *saIntakeHooks) counts() (fanouts, evals int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fanouts, h.evals
}

func qqTrigger(msgID, key, label, addressed string) saTriggerIn {
	return saTriggerIn{
		UserID:       "u1",
		Source:       model.SASourceQQ,
		SourceMsgID:  msgID,
		ReplyToMsgID: "quoted-" + msgID,
		ThreadKey:    key,
		ThreadLabel:  label,
		ActorID:      "201",
		ActorName:    "小明",
		ActorKind:    model.SAActorHuman,
		Addressed:    addressed,
		Text:         "你好",
		Raw:          `{"message_id":"` + msgID + `"}`,
	}
}

func TestSAIntakeDuplicateQQMessage(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{}
	hooks.install(t)

	in := qqTrigger("m1", "qq:private:201", "QQ · 小明", model.SAAddressedDirect)
	first, err := saIngest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || first.Trigger.Id == 0 || first.Trigger.Disposition != model.SADispQueued {
		t.Fatalf("first = %+v", first)
	}
	if first.Trigger.ReplyToMsgID != "quoted-m1" || first.Trigger.Text != "你好" {
		t.Fatalf("trigger fields = %+v", first.Trigger)
	}

	in.Text = "第二次不该覆盖"
	second, err := saIngest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.Trigger.Id != first.Trigger.Id || second.Trigger.Text != "你好" {
		t.Fatalf("duplicate = %+v", second)
	}
	if second.Thread.Id != first.Thread.Id {
		t.Fatalf("duplicate forked the thread: %d vs %d", second.Thread.Id, first.Thread.Id)
	}
	var n int64
	if err := db.Model(&model.SATrigger{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("triggers = %d, want 1", n)
	}
	fanouts, evals := hooks.counts()
	if fanouts != 1 || evals != 1 {
		t.Fatalf("fanout=%d evaluate=%d, want 1 and 1", fanouts, evals)
	}
}

func TestSAIntakeDuplicateConcurrent(t *testing.T) {
	db := setupSATestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	db.Exec("PRAGMA busy_timeout = 5000")

	var hooks saIntakeHooks
	hooks.install(t)

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	dups := make([]bool, n)
	in := qqTrigger("same", "qq:private:7", "QQ · 并发", model.SAAddressedDirect)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := saIngest(context.Background(), in)
			errs[i] = err
			dups[i] = res.Duplicate
		}(i)
	}
	wg.Wait()

	fresh := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !dups[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("non-duplicate results = %d, want 1", fresh)
	}
	var rows int64
	if err := db.Model(&model.SATrigger{}).Where("source_msg_id = ?", "same").Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	fanouts, evals := hooks.counts()
	if fanouts != 1 || evals != 1 {
		t.Fatalf("fanout=%d evaluate=%d, want 1 and 1", fanouts, evals)
	}
}

func TestSAIntakeGroupUnaddressedQueued(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{}
	hooks.install(t)

	res, err := saIngest(context.Background(), qqTrigger("g1", "qq:group:300", "QQ 群 300", model.SAAddressedNone))
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || len(res.FannedOut) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.Trigger.Disposition != model.SADispQueued || res.Trigger.Addressed != model.SAAddressedNone {
		t.Fatalf("trigger = %+v", res.Trigger)
	}
	if res.Thread.ThreadKey != "qq:group:300" || res.Thread.Label != "QQ 群 300" {
		t.Fatalf("thread = %+v", res.Thread)
	}
	var stored model.SATrigger
	if err := db.First(&stored, res.Trigger.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Disposition != model.SADispQueued {
		t.Fatalf("stored disposition = %s", stored.Disposition)
	}
	var outbound int64
	if err := db.Model(&model.SAOutbound{}).Count(&outbound).Error; err != nil {
		t.Fatal(err)
	}
	if outbound != 0 {
		t.Fatalf("unaddressed group message enqueued %d replies", outbound)
	}
	fanouts, evals := hooks.counts()
	if fanouts != 1 || evals != 1 || len(hooks.threads) != 1 || hooks.threads[0] != res.Thread.Id {
		t.Fatalf("fanout=%d evals=%d threads=%v", fanouts, evals, hooks.threads)
	}
}

func TestSAIntakeFanOutClaimsAndSkipsEvaluate(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{claimed: []uint{4, 9}}
	hooks.install(t)

	res, err := saIngest(context.Background(), qqTrigger("m2", "qq:private:201", "QQ · 小明", model.SAAddressedDirect))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FannedOut) != 2 || res.FannedOut[0] != 4 || res.Trigger.Disposition != model.SADispFannedOut {
		t.Fatalf("result = %+v", res)
	}
	var stored model.SATrigger
	if err := db.First(&stored, res.Trigger.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Disposition != model.SADispFannedOut {
		t.Fatalf("stored disposition = %s", stored.Disposition)
	}
	_, evals := hooks.counts()
	if evals != 0 {
		t.Fatalf("evaluate called %d times", evals)
	}
}

func TestSAIntakeFanOutErrorStaysQueued(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{fanErr: errors.New("fanout down")}
	hooks.install(t)

	res, err := saIngest(context.Background(), qqTrigger("m3", "qq:private:201", "QQ · 小明", model.SAAddressedDirect))
	if err == nil || !strings.Contains(err.Error(), "fanout down") {
		t.Fatalf("err = %v", err)
	}
	if res.Trigger.Id == 0 {
		t.Fatal("trigger should be stored before fanout")
	}
	var stored model.SATrigger
	if err := db.First(&stored, res.Trigger.Id).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Disposition != model.SADispQueued {
		t.Fatalf("disposition = %s", stored.Disposition)
	}
	_, evals := hooks.counts()
	if evals != 0 {
		t.Fatalf("evaluate called %d times", evals)
	}
}

func TestSAIntakeInlineSkipsEvaluate(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{}
	hooks.install(t)

	conv := model.Conversation{UserID: "u1", Title: "网页", Kind: KindWork}
	if err := db.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	var before int64
	db.Model(&model.Conversation{}).Count(&before)

	res, err := saIngest(context.Background(), saTriggerIn{
		UserID:         "u1",
		Source:         model.SASourceWeb,
		SourceMsgID:    "55",
		ThreadKey:      "web:conv:55",
		ThreadLabel:    conv.Title,
		ConversationID: conv.Id,
		ActorID:        "u1",
		ActorKind:      model.SAActorHuman,
		Addressed:      model.SAAddressedDirect,
		Text:           "hi",
		Inline:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Trigger.Disposition != model.SADispInline || res.Thread.ConversationID != conv.Id {
		t.Fatalf("result = %+v thread=%+v", res.Trigger, res.Thread)
	}
	var after int64
	db.Model(&model.Conversation{}).Count(&after)
	if after != before {
		t.Fatalf("inline web ingest created a conversation (%d -> %d)", before, after)
	}
	_, evals := hooks.counts()
	if evals != 0 {
		t.Fatalf("inline path called evaluate %d times", evals)
	}

	hooks.claimed = []uint{3}
	claimed, err := saIngest(context.Background(), saTriggerIn{
		UserID:         "u1",
		Source:         model.SASourceWeb,
		SourceMsgID:    "56",
		ThreadKey:      "web:conv:55",
		ConversationID: conv.Id,
		Text:           "继续",
		Inline:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.FannedOut) != 1 || claimed.FannedOut[0] != 3 || claimed.Trigger.Disposition != model.SADispFannedOut {
		t.Fatalf("claimed inline = %+v", claimed)
	}
	if claimed.Thread.Id != res.Thread.Id {
		t.Fatal("inline messages split the thread")
	}
	_, evals = hooks.counts()
	if evals != 0 {
		t.Fatalf("claimed inline called evaluate %d times", evals)
	}
}

func TestSAIntakeMigratesQQBindingOnce(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{}
	hooks.install(t)
	t.Setenv("QQ_BOT_KIND", "code")

	old := model.Conversation{UserID: "u1", Title: "QQ · 旧昵称", Kind: KindChat}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	binding := model.QQChatBinding{ChatKey: "private:42", UserID: "u1", ConversationID: old.Id}
	if err := db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}

	res, err := saIngest(context.Background(), qqTrigger("b1", "qq:private:42", "QQ · 小明", model.SAAddressedDirect))
	if err != nil {
		t.Fatal(err)
	}
	if res.Thread.ConversationID != old.Id {
		t.Fatalf("migrated conversation = %d, want %d", res.Thread.ConversationID, old.Id)
	}
	var convs int64
	db.Model(&model.Conversation{}).Count(&convs)
	if convs != 1 {
		t.Fatalf("conversations = %d, want the migrated one only", convs)
	}

	replacement := model.Conversation{UserID: "u1", Title: "QQ · 新", Kind: KindChat}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&binding).Update("conversation_id", replacement.Id).Error; err != nil {
		t.Fatal(err)
	}
	again, err := saIngest(context.Background(), qqTrigger("b2", "qq:private:42", "QQ · 小明", model.SAAddressedDirect))
	if err != nil {
		t.Fatal(err)
	}
	if again.Thread.Id != res.Thread.Id || again.Thread.ConversationID != old.Id {
		t.Fatalf("binding change rewrote the thread: %+v", again.Thread)
	}

	// A binding that points at a deleted conversation cannot be migrated.
	if err := db.Create(&model.QQChatBinding{ChatKey: "group:9", UserID: "u1", ConversationID: 999}).Error; err != nil {
		t.Fatal(err)
	}
	fresh, err := saIngest(context.Background(), qqTrigger("b3", "qq:group:9", "QQ 群 9", model.SAAddressedMention))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Thread.ConversationID == 0 || fresh.Thread.ConversationID == old.Id {
		t.Fatalf("thread = %+v", fresh.Thread)
	}
	var created model.Conversation
	if err := db.First(&created, fresh.Thread.ConversationID).Error; err != nil {
		t.Fatal(err)
	}
	if created.Title != "QQ 群 9" || created.Kind != KindCode || created.UserID != "u1" {
		t.Fatalf("foreground = %+v", created)
	}

	// Someone else's binding is not this user's foreground session.
	other := model.Conversation{UserID: "u2", Title: "别人的", Kind: KindWork}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.QQChatBinding{ChatKey: "private:77", UserID: "u2", ConversationID: other.Id}).Error; err != nil {
		t.Fatal(err)
	}
	mine := qqTrigger("b4", "qq:private:77", "QQ · 小明", model.SAAddressedDirect)
	got, err := saIngest(context.Background(), mine)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thread.ConversationID == other.Id {
		t.Fatal("adopted another user's binding")
	}
}

func TestSAIntakeQQForegroundKind(t *testing.T) {
	db := setupSATestDB(t)
	(&saIntakeHooks{}).install(t)
	t.Setenv("QQ_BOT_KIND", "")

	res, err := saIngest(context.Background(), qqTrigger("k1", "qq:private:5", "QQ · 小明", model.SAAddressedDirect))
	if err != nil {
		t.Fatal(err)
	}
	var conv model.Conversation
	if err := db.First(&conv, res.Thread.ConversationID).Error; err != nil {
		t.Fatal(err)
	}
	if conv.Kind != KindChat || conv.Title != "QQ · 小明" {
		t.Fatalf("default foreground = %+v", conv)
	}

	t.Setenv("QQ_BOT_KIND", "learn")
	group, err := saIngest(context.Background(), qqTrigger("k2", "qq:group:8", "QQ 群 8", model.SAAddressedMention))
	if err != nil {
		t.Fatal(err)
	}
	var groupConv model.Conversation
	if err := db.First(&groupConv, group.Thread.ConversationID).Error; err != nil {
		t.Fatal(err)
	}
	if groupConv.Kind != KindLearn || groupConv.Title != "QQ 群 8" {
		t.Fatalf("group foreground = %+v", groupConv)
	}
}

func TestSAIntakeRejectsIncomplete(t *testing.T) {
	setupSATestDB(t)
	_, err := saIngest(context.Background(), saTriggerIn{Source: model.SASourceQQ, ThreadKey: "qq:private:1"})
	if err == nil {
		t.Fatal("missing source message id should fail")
	}
}

func saIntakePostChat(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxUserIDKey, "u1")
	postChat(c)
	return w
}

func TestSAIntakePostChatHandsOff(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{claimed: []uint{7, 8}}
	hooks.install(t)

	w := saIntakePostChat(t, `{"content":"帮我做完","model":"missing-model"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	want := "event: delta\ndata: {\"content\":\"已转交任务 #7\"}\n\nevent: done\ndata: {}\n\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("sse = %q", got)
	}
	_, evals := hooks.counts()
	if evals != 0 {
		t.Fatalf("inline handoff called evaluate %d times", evals)
	}
	var msg model.Message
	if err := db.Where("role = ?", "user").First(&msg).Error; err != nil {
		t.Fatal(err)
	}
	var trig model.SATrigger
	if err := db.First(&trig).Error; err != nil {
		t.Fatal(err)
	}
	if trig.Source != model.SASourceWeb || trig.SourceMsgID != strconv.FormatUint(uint64(msg.Id), 10) {
		t.Fatalf("source_msg_id = %s, message id = %d", trig.SourceMsgID, msg.Id)
	}
	if trig.Disposition != model.SADispFannedOut || trig.ThreadID == 0 {
		t.Fatalf("trigger = %+v", trig)
	}
	var thread model.SAThread
	if err := db.First(&thread, trig.ThreadID).Error; err != nil {
		t.Fatal(err)
	}
	wantKey := "web:conv:" + strconv.FormatUint(uint64(msg.ConversationID), 10)
	if thread.ThreadKey != wantKey || thread.ConversationID != msg.ConversationID {
		t.Fatalf("thread = %+v", thread)
	}
	var assistant int64
	db.Model(&model.Message{}).Where("role = ?", "assistant").Count(&assistant)
	if assistant != 0 {
		t.Fatal("handoff should not start a foreground reply")
	}
}

func TestSAIntakePostChatContinuesWhenUnclaimed(t *testing.T) {
	db := setupSATestDB(t)
	hooks := &saIntakeHooks{}
	hooks.install(t)
	oldReg := registry
	registry = llm.NewRegistry()
	t.Cleanup(func() { registry = oldReg })

	w := saIntakePostChat(t, `{"content":"普通聊天","model":"missing-model"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "已转交任务") {
		t.Fatalf("unclaimed chat was handed off: %q", body)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, llm.ErrNoEndpoint.Error()) {
		t.Fatalf("expected the normal stream error, got %q", body)
	}
	_, evals := hooks.counts()
	if evals != 0 {
		t.Fatalf("inline chat called evaluate %d times", evals)
	}
	var trig model.SATrigger
	if err := db.First(&trig).Error; err != nil {
		t.Fatal(err)
	}
	if trig.Disposition != model.SADispInline || trig.Source != model.SASourceWeb {
		t.Fatalf("trigger = %+v", trig)
	}
	var n int64
	db.Model(&model.SATrigger{}).Count(&n)
	if n != 1 {
		t.Fatalf("triggers = %d", n)
	}
}
