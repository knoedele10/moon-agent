package web

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"blog-my/internal/model"
	"gorm.io/gorm"
)

const saTestSource = "sa-test"

type saFakeChannel struct {
	sent []model.SAOutbound
	send func(out model.SAOutbound) (string, error)
}

func (f *saFakeChannel) Send(_ context.Context, _ model.SAThread, out model.SAOutbound, _ *model.SATrigger) (string, error) {
	f.sent = append(f.sent, out)
	if f.send != nil {
		return f.send(out)
	}
	return fmt.Sprintf("ref-%d", out.Id), nil
}

func (f *saFakeChannel) ids() []uint {
	ids := make([]uint, len(f.sent))
	for i := range f.sent {
		ids[i] = f.sent[i].Id
	}
	return ids
}

func saUseChannel(t *testing.T, source string, ch saChannel) {
	t.Helper()
	prev, ok := saChannels[source]
	saChannels[source] = ch
	t.Cleanup(func() {
		if ok {
			saChannels[source] = prev
		} else {
			delete(saChannels, source)
		}
	})
}

func saSeedThread(t *testing.T, db *gorm.DB, key string) model.SAThread {
	t.Helper()
	th := model.SAThread{UserID: "u1", ThreadKey: key, Source: saTestSource, EvalState: "idle"}
	if err := db.Create(&th).Error; err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	return th
}

func saEnqueueText(t *testing.T, threadID uint, key, text string) model.SAOutbound {
	t.Helper()
	row, err := saEnqueueOutbound(saOutboundReq{
		UserID: "u1", ThreadID: threadID, Purpose: model.SAPurposeReply, Text: text, DedupeKey: key,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	return row
}

func saReloadOut(t *testing.T, db *gorm.DB, id uint) model.SAOutbound {
	t.Helper()
	var out model.SAOutbound
	if err := db.First(&out, id).Error; err != nil {
		t.Fatalf("reload outbound %d: %v", id, err)
	}
	return out
}

func saForceDue(t *testing.T, db *gorm.DB, id uint) {
	t.Helper()
	past := time.Now().Add(-2 * time.Second)
	if err := db.Model(&model.SAOutbound{}).Where("id = ?", id).Update("next_attempt_at", past).Error; err != nil {
		t.Fatal(err)
	}
}

func drainSAWake(t *testing.T) {
	t.Helper()
	select {
	case <-saOutboundWake:
	default:
	}
}

func assertSAWake(t *testing.T) {
	t.Helper()
	select {
	case <-saOutboundWake:
	default:
		t.Fatal("enqueue did not wake the sender")
	}
}

func assertIDs(t *testing.T, got, want []uint) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("send order = %v, want %v", got, want)
	}
}

func TestSAOutboundEnqueueDedupe(t *testing.T) {
	db := setupSATestDB(t)
	th := saSeedThread(t, db, "dedupe")
	drainSAWake(t)

	req := saOutboundReq{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeResult,
		Text: "hello", DedupeKey: "task:1:result:1",
	}
	first, err := saEnqueueOutbound(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSAWake(t)
	if first.Id == 0 || first.Status != model.SAOutPending {
		t.Fatalf("first row = %+v", first)
	}

	req.Text = "hello again"
	second, err := saEnqueueOutbound(req)
	if err != nil {
		t.Fatal(err)
	}
	assertSAWake(t)
	if second.Id != first.Id || second.Text != "hello" {
		t.Fatalf("dedupe returned %+v, want id=%d text=%q", second, first.Id, "hello")
	}
	var n int64
	if err := db.Model(&model.SAOutbound{}).Where("dedupe_key = ?", req.DedupeKey).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows with dedupe key = %d, want 1", n)
	}

	other := saEnqueueText(t, th.Id, "task:1:result:2", "other")
	if other.Id == first.Id {
		t.Fatal("a different dedupe key must insert a new row")
	}
	if err := db.Model(&model.SAOutbound{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("outbound rows = %d, want 2", n)
	}
}

func TestSAOutboundFIFOBlock(t *testing.T) {
	db := setupSATestDB(t)
	blocked := saSeedThread(t, db, "fifo-blocked")
	other := saSeedThread(t, db, "fifo-other")
	ch := &saFakeChannel{}
	var hold uint
	ch.send = func(out model.SAOutbound) (string, error) {
		if out.Id == hold {
			return "", errors.New("temporary outage")
		}
		return fmt.Sprintf("ref-%d", out.Id), nil
	}
	saUseChannel(t, saTestSource, ch)

	first := saEnqueueText(t, blocked.Id, "fifo-1", "first")
	second := saEnqueueText(t, blocked.Id, "fifo-2", "second")
	side := saEnqueueText(t, other.Id, "fifo-side", "side")
	hold = first.Id

	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{first.Id, side.Id})
	gotSecond := saReloadOut(t, db, second.Id)
	if gotSecond.Status != model.SAOutPending || gotSecond.SentRef != "" || gotSecond.Attempts != 0 {
		t.Fatalf("second sent while first is still retrying: %+v", gotSecond)
	}
	gotSide := saReloadOut(t, db, side.Id)
	if gotSide.Status != model.SAOutSent || gotSide.SentRef == "" {
		t.Fatalf("other thread should send independently: %+v", gotSide)
	}
	gotFirst := saReloadOut(t, db, first.Id)
	if gotFirst.Attempts != 1 || gotFirst.Status != model.SAOutPending {
		t.Fatalf("first failure = %+v", gotFirst)
	}

	saForceDue(t, db, first.Id)
	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{first.Id, side.Id, first.Id})
	gotSecond = saReloadOut(t, db, second.Id)
	if gotSecond.Status != model.SAOutPending || gotSecond.SentRef != "" {
		t.Fatalf("second sent before attempts>=3: %+v", gotSecond)
	}

	saForceDue(t, db, first.Id)
	saSendPending(context.Background())
	gotFirst = saReloadOut(t, db, first.Id)
	if gotFirst.Attempts != 3 || gotFirst.Status != model.SAOutFailed {
		t.Fatalf("third failure = %+v", gotFirst)
	}
	gotSecond = saReloadOut(t, db, second.Id)
	if gotSecond.Status != model.SAOutSent || gotSecond.SentRef == "" {
		t.Fatalf("second should send once the head has attempts>=3: %+v", gotSecond)
	}
	assertIDs(t, ch.ids(), []uint{first.Id, side.Id, first.Id, first.Id, second.Id})

	// 前一条这次就成功时，后一条在同一次扫描里按顺序发出。
	okThread := saSeedThread(t, db, "fifo-ok")
	ok1 := saEnqueueText(t, okThread.Id, "fifo-ok-1", "ok1")
	ok2 := saEnqueueText(t, okThread.Id, "fifo-ok-2", "ok2")
	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{first.Id, side.Id, first.Id, first.Id, second.Id, ok1.Id, ok2.Id})
	if saReloadOut(t, db, ok1.Id).Status != model.SAOutSent || saReloadOut(t, db, ok2.Id).Status != model.SAOutSent {
		t.Fatal("both messages should send when the earlier one succeeds")
	}
}

func TestSAOutboundPermanentDrop(t *testing.T) {
	db := setupSATestDB(t)
	th := saSeedThread(t, db, "drop")
	ch := &saFakeChannel{}
	first := saEnqueueText(t, th.Id, "drop-1", "gone")
	second := saEnqueueText(t, th.Id, "drop-2", "next")
	ch.send = func(out model.SAOutbound) (string, error) {
		if out.Id == first.Id {
			return "", fmt.Errorf("channel: %w", saPermanentError{errors.New("group exited")})
		}
		return "ref-next", nil
	}
	saUseChannel(t, saTestSource, ch)

	saSendPending(context.Background())
	got := saReloadOut(t, db, first.Id)
	if got.Status != model.SAOutDropped {
		t.Fatalf("status = %s, want dropped", got.Status)
	}
	if got.DropReason != "group exited" {
		t.Fatalf("drop_reason = %q", got.DropReason)
	}
	gotNext := saReloadOut(t, db, second.Id)
	if gotNext.Status != model.SAOutSent || gotNext.SentRef != "ref-next" {
		t.Fatalf("later message = %+v", gotNext)
	}

	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{first.Id, second.Id})
	if saReloadOut(t, db, first.Id).Status != model.SAOutDropped {
		t.Fatal("dropped row was retried")
	}
}

func TestSAOutboundBackoff(t *testing.T) {
	db := setupSATestDB(t)
	th := saSeedThread(t, db, "backoff")
	ch := &saFakeChannel{send: func(model.SAOutbound) (string, error) {
		return "", errors.New("down")
	}}
	saUseChannel(t, saTestSource, ch)
	row := saEnqueueText(t, th.Id, "backoff-1", "retry me")

	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		300 * time.Second,
		300 * time.Second,
	}
	for i, delay := range want {
		if i > 0 {
			saForceDue(t, db, row.Id)
		}
		started := time.Now()
		saSendPending(context.Background())
		got := saReloadOut(t, db, row.Id)
		if got.Attempts != i+1 {
			t.Fatalf("attempt %d: attempts=%d", i+1, got.Attempts)
		}
		if got.NextAttemptAt == nil {
			t.Fatalf("attempt %d: next_attempt_at is nil", i+1)
		}
		gotDelay := got.NextAttemptAt.Sub(started)
		if gotDelay < delay-time.Second || gotDelay > delay+2*time.Second {
			t.Fatalf("attempt %d: delay=%s, want %s", i+1, gotDelay, delay)
		}
		wantStatus := model.SAOutPending
		if i+1 >= 3 {
			wantStatus = model.SAOutFailed
		}
		if got.Status != wantStatus || got.SentRef != "" {
			t.Fatalf("attempt %d: status=%s sent_ref=%q", i+1, got.Status, got.SentRef)
		}
		if i == 0 {
			saSendPending(context.Background())
			held := saReloadOut(t, db, row.Id)
			if held.Attempts != 1 {
				t.Fatalf("row retried before next_attempt_at: attempts=%d", held.Attempts)
			}
		}
	}
	if len(ch.ids()) != len(want) {
		t.Fatalf("channel calls = %d, want %d", len(ch.ids()), len(want))
	}
}

func TestSAOutboundNoResendSent(t *testing.T) {
	db := setupSATestDB(t)
	th := saSeedThread(t, db, "no-resend")
	sentAt := time.Now().Add(-time.Minute)
	sent := model.SAOutbound{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeReply, Text: "old",
		DedupeKey: "sent-1", Status: model.SAOutSent, SentRef: "msg-9", SentAt: &sentAt,
	}
	half := model.SAOutbound{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeReply, Text: "half",
		DedupeKey: "sent-half", Status: model.SAOutPending, SentRef: "msg-8", Attempts: 1,
	}
	if err := db.Create(&sent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&half).Error; err != nil {
		t.Fatal(err)
	}
	fresh := saEnqueueText(t, th.Id, "sent-fresh", "fresh")
	ch := &saFakeChannel{}
	saUseChannel(t, saTestSource, ch)

	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{fresh.Id})
	gotSent := saReloadOut(t, db, sent.Id)
	if gotSent.Status != model.SAOutSent || gotSent.SentRef != "msg-9" {
		t.Fatalf("sent row changed: %+v", gotSent)
	}
	gotHalf := saReloadOut(t, db, half.Id)
	if gotHalf.Status != model.SAOutPending || gotHalf.SentRef != "msg-8" || gotHalf.Attempts != 1 {
		t.Fatalf("row with sent_ref was sent again: %+v", gotHalf)
	}
	if saReloadOut(t, db, fresh.Id).Status != model.SAOutSent {
		t.Fatal("fresh pending row was not sent")
	}

	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{fresh.Id})
}

func TestSAOutboundRestartResume(t *testing.T) {
	db := setupSATestDB(t)
	th := saSeedThread(t, db, "resume")
	past := time.Now().Add(-time.Minute)
	sentAt := time.Now().Add(-time.Hour)
	failed := model.SAOutbound{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeNotice, Text: "failed",
		DedupeKey: "resume-failed", Status: model.SAOutFailed, Attempts: 4, NextAttemptAt: &past,
	}
	pending := model.SAOutbound{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeAck, Text: "pending",
		DedupeKey: "resume-pending", Status: model.SAOutPending,
	}
	sent := model.SAOutbound{
		UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeReply, Text: "sent",
		DedupeKey: "resume-sent", Status: model.SAOutSent, SentRef: "already", SentAt: &sentAt,
	}
	for _, row := range []*model.SAOutbound{&failed, &pending, &sent} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	ch := &saFakeChannel{}
	saUseChannel(t, saTestSource, ch)

	// 停掉后再启动，等价于启动扫描再跑一遍 saSendPending。
	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{failed.Id, pending.Id})
	if got := saReloadOut(t, db, failed.Id); got.Status != model.SAOutSent || got.SentRef == "" {
		t.Fatalf("failed row not resumed: %+v", got)
	}
	if got := saReloadOut(t, db, pending.Id); got.Status != model.SAOutSent || got.SentRef == "" {
		t.Fatalf("pending row not resumed: %+v", got)
	}
	if got := saReloadOut(t, db, sent.Id); got.Status != model.SAOutSent || got.SentRef != "already" {
		t.Fatalf("already sent row changed: %+v", got)
	}

	saSendPending(context.Background())
	assertIDs(t, ch.ids(), []uint{failed.Id, pending.Id})
}
