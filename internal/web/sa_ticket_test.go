package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"

	"gorm.io/gorm"
)

// 工作线 C 的验收测试（docs/agent/18-superagent-plan.md「工作线 C」）。
// 用临时 SQLite + saSwap 把 A（发件箱）与 B（策略、gate、交回）换成假实现，
// 不访问真实模型，也不启动任务调度器：工单的状态迁移直接由测试驱动。

type saTicketHarness struct {
	t     *testing.T
	db    *gorm.DB
	owner string
	conv  model.Conversation
	th    model.SAThread
	seq   int

	mu   sync.Mutex
	out  []saOutboundReq
	seen map[string]bool

	// A/B 两线的假实现返回这些。
	policy     model.SAPolicy
	strength   string
	gateReason string
	handBacks  []uint
}

func newSATicketHarness(t *testing.T, convKind string) *saTicketHarness {
	t.Helper()
	t.Setenv("AGENT_MODE", "confirm")
	t.Setenv("AGENT_EVOLVE", "0")
	db := setupSATestDB(t)
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	h := &saTicketHarness{t: t, db: db, owner: "owner-1", seen: map[string]bool{},
		strength: model.SAStrengthExplicit, gateReason: "本机主人在网页上的请求"}
	h.conv = model.Conversation{UserID: h.owner, Title: "QQ · 测试", Model: "sa-test", Kind: convKind}
	if err := db.Create(&h.conv).Error; err != nil {
		t.Fatal(err)
	}
	h.th = model.SAThread{UserID: h.owner, ThreadKey: "qq:private:10001", Source: model.SASourceQQ,
		Label: "QQ · 测试", ConversationID: h.conv.Id, EvalState: "idle"}
	if err := db.Create(&h.th).Error; err != nil {
		t.Fatal(err)
	}
	h.policy = model.SAPolicy{UserID: h.owner, Version: 1, AcceptMode: model.SAAcceptObserve,
		GuestKind: KindChat, GuestMode: ModeConfirm, Rules: "[]"}

	saSwap(t, &saEnqueueOutbound, func(req saOutboundReq) (model.SAOutbound, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.seen[req.DedupeKey] {
			return model.SAOutbound{ThreadID: req.ThreadID, TaskID: req.TaskID, Purpose: req.Purpose,
				Text: req.Text, DedupeKey: req.DedupeKey, Status: model.SAOutPending}, nil
		}
		h.seen[req.DedupeKey] = true
		h.out = append(h.out, req)
		return model.SAOutbound{Id: uint(len(h.out)), ThreadID: req.ThreadID, TaskID: req.TaskID,
			Purpose: req.Purpose, Text: req.Text, DedupeKey: req.DedupeKey, Status: model.SAOutPending}, nil
	})
	saSwap(t, &saPolicyFor, func(string) (model.SAPolicy, error) { return h.policy, nil })
	saSwap(t, &saGate, func(model.SAPolicy, model.SATrigger, bool) (string, string) {
		return h.strength, h.gateReason
	})
	saSwap(t, &saHandBack, func(_ context.Context, _ uint, trig model.SATrigger, _ string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.handBacks = append(h.handBacks, trig.Id)
		return nil
	})
	return h
}

// makeOwner 建一行账号，让 isOwnerUser 有据可查；不建时任何 id 都按单机主人处理。
func (h *saTicketHarness) makeOwner() {
	h.t.Helper()
	u := model.User{Id: h.owner, Username: "owner", Password: "x", CreatedAt: time.Now().Add(-time.Hour)}
	if err := h.db.Create(&u).Error; err != nil {
		h.t.Fatal(err)
	}
}

func (h *saTicketHarness) setOwnerMode(mode string) {
	h.t.Helper()
	if err := h.db.Create(&model.AgentSetting{UserID: h.owner, Mode: mode}).Error; err != nil {
		h.t.Fatal(err)
	}
}

type saTriggerOpt func(*model.SATrigger)

func (h *saTicketHarness) newTrigger(text string, opts ...saTriggerOpt) model.SATrigger {
	h.t.Helper()
	h.seq++
	trig := model.SATrigger{
		UserID: h.owner, ThreadID: h.th.Id, Source: model.SASourceQQ,
		SourceMsgID: fmt.Sprintf("m%d", h.seq), ActorID: h.owner, ActorName: "主人",
		ActorKind: model.SAActorHuman, Addressed: model.SAAddressedDirect,
		Text: text, Disposition: model.SADispQueued, CreatedAt: time.Now(),
	}
	for _, o := range opts {
		o(&trig)
	}
	if err := h.db.Create(&trig).Error; err != nil {
		h.t.Fatal(err)
	}
	return trig
}

func asGuest(t *model.SATrigger) {
	t.ActorID, t.ActorName, t.Addressed = "10001", "访客", model.SAAddressedDirect
}

func (h *saTicketHarness) accept(trig model.SATrigger, opts ...func(*saAcceptReq)) (model.AgentTask, error) {
	h.t.Helper()
	req := saAcceptReq{
		UserID: h.owner, Thread: h.th, Origin: trig, Strength: model.SAStrengthExplicit,
		Title: "写报告", Goal: "整理数据并写一份报告", Constraints: "只用中文",
		Criteria: []taskCriterion{{Text: "报告包含三个章节"}, {Text: "数据来源已注明"}},
		Ack:      "好的，我来写报告。", Policy: h.policy,
	}
	for _, o := range opts {
		o(&req)
	}
	return saAcceptTicket(context.Background(), req)
}

func (h *saTicketHarness) mustAccept(trig model.SATrigger, opts ...func(*saAcceptReq)) model.AgentTask {
	h.t.Helper()
	task, err := h.accept(trig, opts...)
	if err != nil {
		h.t.Fatalf("接单失败：%v", err)
	}
	return task
}

func (h *saTicketHarness) outbounds() []saOutboundReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]saOutboundReq(nil), h.out...)
}

func (h *saTicketHarness) reload(id uint) model.AgentTask {
	h.t.Helper()
	task, err := loadTask(id)
	if err != nil {
		h.t.Fatalf("读取任务失败：%v", err)
	}
	return task
}

// 1. 同一 Origin 接单两次只有一个任务；受理确认只入队一次。
func TestSATicketAcceptIdempotent(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	trig := h.newTrigger("帮我整理数据并写一份报告")

	first := h.mustAccept(trig)
	second := h.mustAccept(trig)
	if first.Id == 0 || second.Id != first.Id {
		t.Fatalf("同一 Origin 应返回同一个工单：%d vs %d", first.Id, second.Id)
	}
	var tasks, mandates, subs int64
	h.db.Model(&model.AgentTask{}).Count(&tasks)
	h.db.Model(&model.SAMandate{}).Count(&mandates)
	h.db.Model(&model.SASubscription{}).Count(&subs)
	if tasks != 1 || mandates != 1 || subs != 1 {
		t.Fatalf("应只建一份工单：tasks=%d mandates=%d subs=%d", tasks, mandates, subs)
	}
	var originSub model.SASubscription
	h.db.Where("task_id = ?", first.Id).First(&originSub)
	if originSub.ThreadID != h.th.Id || originSub.Role != model.SASubOrigin {
		t.Fatalf("应有 origin 订阅：%+v", originSub)
	}
	acks := 0
	for _, o := range h.outbounds() {
		if o.Purpose == model.SAPurposeAck {
			acks++
			if o.DedupeKey != fmt.Sprintf("task:%d:ack", first.Id) {
				t.Fatalf("受理确认的 dedupe 不对：%s", o.DedupeKey)
			}
			if !strings.Contains(o.Text, fmt.Sprintf("#%d", first.Id)) || o.ReplyToTriggerID != trig.Id {
				t.Fatalf("受理确认内容不对：%+v", o)
			}
		}
	}
	if acks != 1 {
		t.Fatalf("受理确认应只入队一次，实际 %d", acks)
	}
	if first.Kind != KindWork || first.Mode != ModeConfirm || first.MainConversationID != h.conv.Id {
		t.Fatalf("任务配置不对：kind=%s mode=%s main=%d", first.Kind, first.Mode, first.MainConversationID)
	}
	var m model.SAMandate
	h.db.Where("task_id = ?", first.Id).First(&m)
	if m.OriginTriggerID != trig.Id || m.ThreadID != h.th.Id || m.Source != model.SASourceQQ || m.Strength != model.SAStrengthExplicit {
		t.Fatalf("授权记录不对：%+v", m)
	}
	if m.Kind != first.Kind || m.Mode != first.Mode {
		t.Fatalf("Mandate 的 Kind/Mode 应是工单实际配置：%s/%s vs %s/%s", m.Kind, m.Mode, first.Kind, first.Mode)
	}
}

// 2. 访客的工单不得超过策略上限（guest_kind/guest_mode），也不能设置命令验收。
func TestSATicketGuestAuthorityLimit(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	h.policy = model.SAPolicy{UserID: h.owner, Version: 7, AcceptMode: model.SAAcceptAuto,
		GuestKind: KindLearn, GuestMode: ModeConfirm, Rules: "[]"}
	trig := h.newTrigger("帮我整理数据并写一份报告", asGuest)

	task := h.mustAccept(trig)
	if task.Kind != KindLearn || task.Mode != ModeConfirm {
		t.Fatalf("访客工单应被压到策略上限 learn/confirm：%s/%s", task.Kind, task.Mode)
	}
	var m model.SAMandate
	h.db.Where("task_id = ?", task.Id).First(&m)
	if m.PolicyVersion != 7 || m.ActorID != "10001" {
		t.Fatalf("授权应记录发起人与策略版本：%+v", m)
	}

	// 命令验收等于让宿主机跑命令：guest 的低档位不允许模型代填。
	h2 := newSATicketHarness(t, KindWork)
	h2.makeOwner()
	h2.policy = model.SAPolicy{UserID: h2.owner, Version: 1, AcceptMode: model.SAAcceptObserve,
		GuestKind: KindWork, GuestMode: ModeConfirm, Rules: "[]"}
	cmdTrig := h2.newTrigger("跑测试并写报告", asGuest)
	_, err := h2.accept(cmdTrig, func(r *saAcceptReq) {
		r.Criteria = []taskCriterion{{Text: "测试通过", Method: criterionCommand,
			Check: &taskCheck{Program: "go", Args: []string{"test", "./..."}}}}
	})
	if err == nil {
		t.Fatal("访客档位不应允许命令验收")
	}

	// 默认聊天档没有执行工具：任何任务都会超过这份授权，因此直接拒单。
	h3 := newSATicketHarness(t, KindWork)
	h3.makeOwner()
	h3.policy = model.SAPolicy{UserID: h3.owner, Version: 1, AcceptMode: model.SAAcceptAuto,
		GuestKind: KindChat, GuestMode: ModeConfirm, Rules: "[]"}
	if _, err := h3.accept(h3.newTrigger("帮我整理数据并写报告", asGuest)); err == nil {
		t.Fatal("guest_kind=chat 时不应建出工单（任务至少是 work 档）")
	}
	var n int64
	h3.db.Model(&model.AgentTask{}).Count(&n)
	if n != 0 {
		t.Fatalf("拒单不应留下任务：%d", n)
	}
}

// 2b. 本机主人用自己的设置（会话用途 + 自主档位），命令验收在自主档允许。
func TestSATicketOwnerAuthority(t *testing.T) {
	h := newSATicketHarness(t, KindCode)
	h.makeOwner()
	h.setOwnerMode(ModeAuto)
	trig := h.newTrigger("修一下这个 bug")
	task := h.mustAccept(trig, func(r *saAcceptReq) {
		r.Criteria = []taskCriterion{{Text: "go test 通过", Method: criterionCommand,
			Check: &taskCheck{Program: "go", Args: []string{"test", "./..."}}}}
	})
	if task.Kind != KindCode || task.Mode != ModeAuto {
		t.Fatalf("主人应使用自己的设置 code/auto：%s/%s", task.Kind, task.Mode)
	}
}

// 3a. 回复工单发出的消息 → 认领为补充输入。
func TestSATicketFanOutReplyToAck(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	// 模拟 A 线已经把受理确认发出去，并记下了来源里的消息 id。
	out := model.SAOutbound{UserID: h.owner, ThreadID: h.th.Id, TaskID: task.Id,
		Purpose: model.SAPurposeAck, Text: "收到", DedupeKey: "seed:ack", Status: model.SAOutSent, SentRef: "qq-msg-100"}
	if err := h.db.Create(&out).Error; err != nil {
		t.Fatal(err)
	}

	follow := h.newTrigger("在报告里加一节图表", func(tr *model.SATrigger) { tr.ReplyToMsgID = "qq-msg-100" })
	claimed, err := saFanOut(context.Background(), follow)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != task.Id {
		t.Fatalf("回复受理确认应认领给工单 %d：%v", task.Id, claimed)
	}
	var reload model.SATrigger
	h.db.First(&reload, follow.Id)
	if reload.Disposition != model.SADispFannedOut {
		t.Fatalf("Trigger 应记为 fanned_out：%s", reload.Disposition)
	}
	var rows []model.AgentTaskOutbox
	h.db.Where("task_id = ?", task.Id).Find(&rows)
	found := false
	for _, r := range rows {
		if strings.Contains(r.Content, "在报告里加一节图表") {
			found = true
			if r.DedupeKey != fmt.Sprintf("t%d:follow:t%d", task.Id, follow.Id) {
				t.Fatalf("补充输入的 dedupe 不对：%s", r.DedupeKey)
			}
		}
	}
	if !found {
		t.Fatal("后续消息没有投给执行对话")
	}
}

// 3b. 文本里写 #id → 认领；无关消息 → 不认领、不改 disposition。
func TestSATicketFanOutHashRefAndIgnore(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))

	mention := h.newTrigger(fmt.Sprintf("顺便 #%d 也同步给运营", task.Id))
	claimed, err := saFanOut(context.Background(), mention)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != task.Id {
		t.Fatalf("#id 应认领给工单 %d：%v", task.Id, claimed)
	}

	chatter := h.newTrigger("今天天气不错")
	claimed, err = saFanOut(context.Background(), chatter)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("无关消息不应被认领：%v", claimed)
	}
	var reload model.SATrigger
	h.db.First(&reload, chatter.Id)
	if reload.Disposition != model.SADispQueued {
		t.Fatalf("无关消息应留给前台判断：%s", reload.Disposition)
	}
}

// 4. waiting_human 时提问发到 origin Thread；发起人回答后任务恢复 running；
// 同一状态重复通知不重复发消息。
func TestSATicketQuestionAndAnswer(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	if ok, err := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskWaitingHuman, "stop_reason": "need_human",
		"blocker": "需要数据接口的 key", "next_run_at": nil,
	}); err != nil || !ok {
		t.Fatalf("进入 waiting_human 失败：ok=%v err=%v", ok, err)
	}
	waiting := h.reload(task.Id)
	notifyTask(waiting, model.SAPurposeQuestion, "需要数据接口的 key 才能继续。")
	var qs []saOutboundReq
	for _, o := range h.outbounds() {
		if o.Purpose == model.SAPurposeQuestion {
			qs = append(qs, o)
		}
	}
	if len(qs) != 1 || qs[0].ThreadID != h.th.Id || qs[0].TaskID != task.Id {
		t.Fatalf("提问应发到 origin Thread：%+v", qs)
	}
	wantKey := fmt.Sprintf("task:%d:%s:%d", task.Id, model.TaskWaitingHuman, waiting.Revision)
	if qs[0].DedupeKey != wantKey {
		t.Fatalf("提问的 dedupe 应为 %s：%s", wantKey, qs[0].DedupeKey)
	}
	// 同一状态重复通知：发件箱按 dedupe 只留一条（此时共 1 条受理确认 + 1 条提问）。
	notifyTask(waiting, model.SAPurposeQuestion, "需要数据接口的 key 才能继续。")
	if n := len(h.outbounds()); n != len(qs)+1 {
		t.Fatalf("重复通知不应再入队：%d", n)
	}

	answer := h.newTrigger("key 是 abc-123")
	claimed, err := saFanOut(context.Background(), answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != task.Id {
		t.Fatalf("发起人的回答应认领给唯一在等人的工单：%v", claimed)
	}
	after := h.reload(task.Id)
	if after.Status != model.TaskRunning || after.StopReason != "human_input" {
		t.Fatalf("回答后应恢复 running：%s/%s", after.Status, after.StopReason)
	}
	var rows []model.AgentTaskOutbox
	h.db.Where("task_id = ?", task.Id).Find(&rows)
	if len(rows) != 2 { // 任务说明 + 后续消息
		t.Fatalf("回答应投递为补充输入：%d", len(rows))
	}
}

// 5a. 被人叫停的工单不再认领任何消息（即使消息里写了 #id）。
func TestSATicketStoppedNoClaim(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	if err := saStopTicket(context.Background(), task.Id, "router", "先不做了"); err != nil {
		t.Fatal(err)
	}
	stopped := h.reload(task.Id)
	if stopped.Status != model.TaskCanceled || stopped.StopReason != "stopped_by_router" {
		t.Fatalf("叫停后应为 canceled：%s/%s", stopped.Status, stopped.StopReason)
	}
	m, ok := saTaskMandate(task.Id)
	if !ok || m.WithdrawnAt == nil || m.EndReason == "" {
		t.Fatalf("叫停应收回授权：%+v", m)
	}
	if status, _, blocked := taskMandateStopped(task.Id); !blocked || status != model.TaskCanceled {
		t.Fatalf("每轮前的授权检查应拦住：%s %v", status, blocked)
	}

	follow := h.newTrigger(fmt.Sprintf("继续做 #%d", task.Id))
	claimed, err := saFanOut(context.Background(), follow)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("被叫停的工单不应再认领：%v", claimed)
	}
	if cards, _ := saLiveTickets(h.th.Id); len(cards) != 0 {
		t.Fatalf("被叫停的工单不应出现在前台卡片里：%+v", cards)
	}
}

// 5b. 结束的工单被认领前先复核授权；策略移除发起人后无法续上。
func TestSATicketFanOutRevokedAfterPolicyChange(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	h.strength = model.SAStrengthInferred
	h.policy = model.SAPolicy{UserID: h.owner, Version: 3, AcceptMode: model.SAAcceptAuto,
		GuestKind: KindWork, GuestMode: ModeConfirm,
		Rules: `[{"id":"r1","name":"接报告","enabled":true}]`}
	trig := h.newTrigger("帮我做周报", asGuest)
	task := h.mustAccept(trig, func(r *saAcceptReq) {
		r.Strength = model.SAStrengthInferred
		r.MatchedRule = []string{"r1"}
	})
	if ok, err := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskDone, "stop_reason": "verified",
	}); err != nil || !ok {
		t.Fatalf("结束任务失败：%v", err)
	}

	// 策略变更：发起人不再被受理、规则停用、模式回到 observe。
	h.strength = model.SAStrengthNone
	h.policy.AcceptMode = model.SAAcceptObserve
	h.policy.Rules = `[{"id":"r1","name":"接报告","enabled":false}]`

	follow := h.newTrigger(fmt.Sprintf("再补一段 #%d", task.Id), asGuest)
	claimed, err := saFanOut(context.Background(), follow)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("复核失败的工单不应续上：%v", claimed)
	}
	m, _ := saTaskMandate(task.Id)
	if m.RevokedAt == nil || m.EndReason == "" {
		t.Fatalf("复核失败应写 revoked_at 与原因：%+v", m)
	}
	if got := h.reload(task.Id); got.Status != model.TaskDone {
		t.Fatalf("吊销授权不应改动已结束的任务：%s", got.Status)
	}
}

// 5c. 复核通过时，结束的工单被认领后继续跑。
func TestSATicketFanOutRevivesEndedTicket(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	if ok, _ := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskDone, "stop_reason": "verified",
	}); !ok {
		t.Fatal("结束任务失败")
	}
	follow := h.newTrigger(fmt.Sprintf("报告加一页摘要 #%d", task.Id))
	claimed, err := saFanOut(context.Background(), follow)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != task.Id {
		t.Fatalf("复核通过的结束工单应续上：%v", claimed)
	}
	revived := h.reload(task.Id)
	if revived.Status != model.TaskRunning || revived.StopReason != "resumed_by_message" || revived.FinishedAt != nil {
		t.Fatalf("续跑状态不对：%s/%s finished=%v", revived.Status, revived.StopReason, revived.FinishedAt)
	}
	var events int64
	h.db.Model(&model.AgentTaskEvent{}).Where("task_id = ? AND kind = ?", task.Id, "resumed").Count(&events)
	if events == 0 {
		t.Fatal("续跑应有审计事件")
	}
}

// 6. 结局（done）走发件箱、带 dedupe；重复结局只发一次。
func TestSATicketResultNotifyDedupe(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	if ok, _ := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskDone, "stop_reason": "verified",
	}); !ok {
		t.Fatal("结束任务失败")
	}
	done := h.reload(task.Id)
	notifyTaskMain(done, "已通过全部验收条件，任务完成。")
	notifyTaskMain(done, "已通过全部验收条件，任务完成。")
	var results []saOutboundReq
	for _, o := range h.outbounds() {
		if o.Purpose == model.SAPurposeResult {
			results = append(results, o)
		}
	}
	if len(results) != 1 {
		t.Fatalf("结局应只发一次：%d", len(results))
	}
	want := fmt.Sprintf("task:%d:%s:%d", task.Id, model.TaskDone, done.Revision)
	if results[0].DedupeKey != want {
		t.Fatalf("结局 dedupe 应为 %s：%s", want, results[0].DedupeKey)
	}
	// 工单通知不再直接写主对话，避免网页端重复。
	var msgs int64
	h.db.Model(&model.Message{}).Where("conversation_id = ?", h.conv.Id).Count(&msgs)
	if msgs != 0 {
		t.Fatalf("工单通知不应再直接写会话：%d", msgs)
	}
}

// 7. 执行端的 orchestrator_handback：只接受已经扇出给本任务的消息。
func TestSATicketHandbackTool(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	follow := h.newTrigger(fmt.Sprintf("顺手把服务器重启一下 #%d", task.Id))
	if claimed, _ := saFanOut(context.Background(), follow); len(claimed) != 1 {
		t.Fatalf("前置认领失败：%v", claimed)
	}

	tool := taskHandBackTool{st: &taskRoundState{task: task}}
	raw, _ := json.Marshal(map[string]any{"trigger_id": follow.Id, "reason": "换成了另一件事"})
	res, err := tool.Execute(context.Background(), raw, &agent.Exec{})
	if err != nil || res.IsError {
		t.Fatalf("交回失败：%v %s", err, res.Content)
	}
	h.mu.Lock()
	got := append([]uint(nil), h.handBacks...)
	h.mu.Unlock()
	if len(got) != 1 || got[0] != follow.Id {
		t.Fatalf("应调用 B 线的 saHandBack：%v", got)
	}

	// 没有扇出过的消息不属于任何工单。
	other := h.newTrigger("随便聊聊")
	raw, _ = json.Marshal(map[string]any{"trigger_id": other.Id, "reason": "越界"})
	if res, _ := tool.Execute(context.Background(), raw, &agent.Exec{}); !res.IsError {
		t.Fatal("未扇出的消息不应能交回")
	}
}

// 8. 前台工单卡：Stage 映射、角色、叫停后消失。
func TestSATicketLiveCards(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	cards, err := saLiveTickets(h.th.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("应有 1 张工单卡：%+v", cards)
	}
	if cards[0].TaskID != task.Id || cards[0].Stage != "working" || cards[0].Role != model.SASubOrigin {
		t.Fatalf("工单卡不对：%+v", cards[0])
	}
	if ok, _ := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskWaitingHuman, "stop_reason": "need_human", "blocker": "等 key",
	}); !ok {
		t.Fatal("进入 waiting_human 失败")
	}
	cards, _ = saLiveTickets(h.th.Id)
	if len(cards) != 1 || cards[0].Stage != "person/answer" {
		t.Fatalf("等待人工的阶段应为 person/answer：%+v", cards)
	}
}

// 9. 验收期间认领到的后续消息不能被「验收通过」吞掉：先回到执行轮处理，再重新验收。
func TestSATicketVerificationKeepsClaimedMessage(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	task := h.mustAccept(h.newTrigger("做一份报告"))
	if _, err := deliverTaskOutbox(task); err != nil {
		t.Fatalf("投递任务说明失败：%v", err)
	}
	sub := taskSubmission{Summary: "报告已完成", Items: []taskEvidenceItem{
		{Criterion: "c1", Evidence: "三个章节都在 report.md"},
		{Criterion: "c2", Evidence: "文末列出数据来源"},
	}}
	raw, _ := json.Marshal(sub)
	if ok, err := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{
		"status": model.TaskVerifying, "stop_reason": "submitted", "evidence": string(raw), "next_run_at": time.Now(),
	}); err != nil || !ok {
		t.Fatalf("进入验收失败：%v", err)
	}
	follow := h.newTrigger(fmt.Sprintf("再加一页摘要 #%d", task.Id))
	if claimed, _ := saFanOut(context.Background(), follow); len(claimed) != 1 {
		t.Fatalf("验收期间应能认领后续消息：%v", claimed)
	}

	oldJudge := taskLLMCall
	taskLLMCall = func(context.Context, string, string, string, string) (string, error) {
		return `{"verdict":"pass","reason":"证据与记录一致"}`, nil
	}
	t.Cleanup(func() { taskLLMCall = oldJudge })

	runTaskVerification(context.Background(), h.reload(task.Id))
	after := h.reload(task.Id)
	if after.Status != model.TaskRunning || after.StopReason != "new_input" {
		t.Fatalf("验收期间有新消息时应回到执行轮：%s/%s", after.Status, after.StopReason)
	}
	var events int64
	h.db.Model(&model.AgentTaskEvent{}).Where("task_id = ? AND kind = ?", task.Id, "new_input").Count(&events)
	if events == 0 {
		t.Fatal("应记录「先处理后续消息」的事件")
	}
}
