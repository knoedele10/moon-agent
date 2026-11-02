package web

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

type saRouterFixture struct {
	conv   model.Conversation
	thread model.SAThread
	policy model.SAPolicy

	mu       sync.Mutex
	outbound []saOutboundReq
	accepted []saAcceptReq
	attached []uint
	stopped  []uint
	cards    []saTicketCard
}

// newSARouterFixture 建一个 QQ Thread（私聊或群）和它的前台会话；其它工作线全部换成假实现。
func newSARouterFixture(t *testing.T, threadKey string, mutate func(*saPolicyView)) *saRouterFixture {
	t.Helper()
	setupSATestDB(t)
	t.Setenv("QQ_ALLOW_USERS", "100")
	t.Setenv("QQ_ALLOW_GROUPS", "900")
	f := &saRouterFixture{}
	f.conv = model.Conversation{UserID: "u1", Title: "front", Model: "m1", Kind: KindChat}
	if err := model.DB.Create(&f.conv).Error; err != nil {
		t.Fatal(err)
	}
	f.thread = model.SAThread{UserID: "u1", ThreadKey: threadKey, Source: model.SASourceQQ, ConversationID: f.conv.Id, EvalState: "idle"}
	if err := model.DB.Create(&f.thread).Error; err != nil {
		t.Fatal(err)
	}
	p, err := saPolicyForDB("u1")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		v := saPolicyToView(p)
		mutate(&v)
		if p, err = saSavePolicy("u1", 0, v); err != nil {
			t.Fatal(err)
		}
	}
	f.policy = p
	saSwap(t, &saEvalRetryBase, 10*time.Millisecond)
	saSwap(t, &saLiveTickets, func(uint) ([]saTicketCard, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]saTicketCard(nil), f.cards...), nil
	})
	saSwap(t, &saEnqueueOutbound, func(req saOutboundReq) (model.SAOutbound, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.outbound = append(f.outbound, req)
		return model.SAOutbound{Id: uint(len(f.outbound))}, nil
	})
	saSwap(t, &saAcceptTicket, func(_ context.Context, req saAcceptReq) (model.AgentTask, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.accepted = append(f.accepted, req)
		return model.AgentTask{Id: 70 + uint(len(f.accepted))}, nil
	})
	saSwap(t, &saAttachTicket, func(_ context.Context, id uint, _ model.SATrigger, _ string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.attached = append(f.attached, id)
		return nil
	})
	saSwap(t, &saStopTicket, func(_ context.Context, id uint, _, _ string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = append(f.stopped, id)
		return nil
	})
	// 最后注册、最先执行：先等后台判断跑完，再还原假实现、拆库。
	t.Cleanup(saEvalWG.Wait)
	return f
}

func (f *saRouterFixture) trigger(t *testing.T, actor, addressed, text string) model.SATrigger {
	t.Helper()
	tr := model.SATrigger{
		UserID: "u1", ThreadID: f.thread.Id, Source: model.SASourceQQ, SourceMsgID: strconv.FormatInt(time.Now().UnixNano(), 10),
		ActorID: actor, ActorName: "n" + actor, ActorKind: model.SAActorHuman, Addressed: addressed, Text: text,
		Disposition: model.SADispQueued,
	}
	if err := model.DB.Create(&tr).Error; err != nil {
		t.Fatal(err)
	}
	return tr
}

func (f *saRouterFixture) route(t *testing.T, raw string) []saRouteResult {
	t.Helper()
	tool := sessionRouterTool{policy: agentToolPolicy{UserID: "u1"}}
	res, err := tool.Execute(context.Background(), json.RawMessage(raw), &agent.Exec{SessionID: strconv.FormatUint(uint64(f.conv.Id), 10)})
	if err != nil || res.IsError {
		t.Fatalf("route failed: %v %s", err, res.Content)
	}
	var out struct {
		Results []saRouteResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	return out.Results
}

func saWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待超时：" + what)
}

func saDecisionOf(t *testing.T, id uint) model.SADecision {
	t.Helper()
	var tr model.SATrigger
	model.DB.First(&tr, id)
	var d model.SADecision
	if tr.DecisionID != 0 {
		model.DB.First(&d, tr.DecisionID)
	}
	return d
}

func saAllDecided(ids ...uint) bool {
	var n int64
	model.DB.Model(&model.SATrigger{}).Where("id IN ? AND decision_id = 0", ids).Count(&n)
	return n == 0
}

// 一批 4 项，其中 1 项被拒，其余照常执行，结果按序。
func TestSARouterRouteBatchIndependentItems(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	f.cards = []saTicketCard{{TaskID: 5, Title: "旧工单", Status: model.TaskRunning, Stage: "working", Role: model.SASubOrigin}}
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "帮我整理周报")
	t2 := f.trigger(t, "100", model.SAAddressedDirect, "补充给 99")
	t3 := f.trigger(t, "100", model.SAAddressedDirect, "旧的那个别做了")
	t4 := f.trigger(t, "100", model.SAAddressedDirect, "谢谢")
	raw := `{"decisions":[
		{"action":"accept","trigger_id":` + strconv.Itoa(int(t1.Id)) + `,"title":"周报","goal":"整理本周周报","criteria":["包含三项进展","[人工] 语气合适"],"ack":"好的，我来整理周报"},
		{"action":"attach","trigger_id":` + strconv.Itoa(int(t2.Id)) + `,"task_id":99},
		{"action":"stop","trigger_id":` + strconv.Itoa(int(t3.Id)) + `,"task_id":5,"reason":"用户叫停"},
		{"action":"ignore","trigger_id":` + strconv.Itoa(int(t4.Id)) + `}]}`
	res := f.route(t, raw)
	if len(res) != 4 || res[0].TriggerID != t1.Id || res[3].TriggerID != t4.Id {
		t.Fatalf("结果顺序不对: %+v", res)
	}
	if res[0].Decision != model.SADecisionAccept || res[0].TaskID != 71 || !res[1].Refused || res[2].Decision != model.SADecisionStop || res[3].Decision != model.SADecisionIgnore {
		t.Fatalf("results = %+v", res)
	}
	req := f.accepted[0]
	if req.Strength != model.SAStrengthExplicit || req.Origin.Id != t1.Id || len(req.Criteria) != 2 || req.Criteria[1].Method != criterionManual || req.DecisionID == 0 || req.Ack == "" {
		t.Fatalf("accept 请求不对: %+v", req)
	}
	if d := saDecisionOf(t, t1.Id); d.TaskID != 71 || d.Kind != model.SADecisionAccept {
		t.Fatalf("accept 决定未回填工单: %+v", d)
	}
	if d := saDecisionOf(t, t2.Id); d.Id != 0 {
		t.Fatal("被拒的那项不应有决定")
	}
	if len(f.stopped) != 1 || f.stopped[0] != 5 || len(f.attached) != 0 {
		t.Fatalf("stop/attach 调用不对: %v %v", f.stopped, f.attached)
	}
	// 已有决定的消息不能再路由。
	if again := f.route(t, `{"decisions":[{"action":"ignore","trigger_id":`+strconv.Itoa(int(t1.Id))+`}]}`); !again[0].Refused {
		t.Fatal("重复路由应被拒")
	}
}

func TestSARouterAcceptFailureRollsBack(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	saSwap(t, &saAcceptTicket, func(context.Context, saAcceptReq) (model.AgentTask, error) {
		return model.AgentTask{}, errors.New("建不了")
	})
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "做个事")
	res := f.route(t, `{"decisions":[{"action":"accept","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"goal":"做个事"}]}`)
	if !res[0].Refused {
		t.Fatalf("应被拒: %+v", res)
	}
	var tr model.SATrigger
	model.DB.First(&tr, t1.Id)
	var n int64
	model.DB.Model(&model.SADecision{}).Count(&n)
	if tr.DecisionID != 0 || tr.Disposition != model.SADispQueued || n != 0 {
		t.Fatalf("接单失败应撤回决定: %+v decisions=%d", tr, n)
	}
}

// 推断消息（群里没 @）只能在命中规则时接单；observe 只记录「本会接单」。
func TestSARouterInferredNeedsRule(t *testing.T) {
	f := newSARouterFixture(t, "qq:group:900", func(v *saPolicyView) {
		v.Rules = []saRule{{ID: "bug", Name: "报错", Instruction: "群里有人贴报错时排查", Enabled: true}}
	})
	t1 := f.trigger(t, "100", model.SAAddressedNone, "又报错了")
	res := f.route(t, `{"decisions":[{"action":"accept","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"goal":"排查"}]}`)
	if !res[0].Refused {
		t.Fatal("没命中规则的推断接单应被拒")
	}
	res = f.route(t, `{"decisions":[{"action":"accept","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"goal":"排查","rule_ids":["bug"]}]}`)
	if d := saDecisionOf(t, t1.Id); res[0].Refused || d.Kind != model.SADecisionIgnore || !d.WouldAccept || len(f.accepted) != 0 {
		t.Fatalf("observe 应只记录: %+v %+v", res, d)
	}
	// 名单外的人在群里 @ 也能 stop？不能：准入表把名单外 @ 设为 explicit 之外的情况。
	f.cards = []saTicketCard{{TaskID: 5}}
	t2 := f.trigger(t, "300", model.SAAddressedNone, "停")
	res = f.route(t, `{"decisions":[{"action":"stop","trigger_id":`+strconv.Itoa(int(t2.Id))+`,"task_id":5}]}`)
	if !res[0].Refused || len(f.stopped) != 0 {
		t.Fatal("推断强度不能叫停工单")
	}
}

func TestSARouterInferredAutoAccepts(t *testing.T) {
	f := newSARouterFixture(t, "qq:group:900", func(v *saPolicyView) {
		v.AcceptMode = model.SAAcceptAuto
		v.Rules = []saRule{{ID: "bug", Name: "报错", Instruction: "排查报错", Enabled: true}}
	})
	t1 := f.trigger(t, "100", model.SAAddressedNone, "又报错了")
	res := f.route(t, `{"decisions":[{"action":"accept","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"goal":"排查","rule_ids":["bug","nope"]}]}`)
	if res[0].Decision != model.SADecisionAccept || len(f.accepted) != 1 || f.accepted[0].Strength != model.SAStrengthInferred || strings.Join(f.accepted[0].MatchedRule, ",") != "bug" {
		t.Fatalf("auto 模式应接单并只记启用的规则: %+v %+v", res, f.accepted)
	}
}

func TestSARouterRefusesNonFrontSessions(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	exec := model.Conversation{UserID: "u1", Title: "exec"}
	model.DB.Create(&exec)
	model.DB.Create(&model.AgentTask{UserID: "u1", ExecConversationID: exec.Id, Status: model.TaskRunning})
	tool := sessionRouterTool{policy: agentToolPolicy{UserID: "u1"}}
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "x")
	raw := json.RawMessage(`{"decisions":[{"action":"ignore","trigger_id":` + strconv.Itoa(int(t1.Id)) + `}]}`)
	if res, _ := tool.Execute(context.Background(), raw, &agent.Exec{SessionID: strconv.FormatUint(uint64(exec.Id), 10)}); !res.IsError {
		t.Fatal("执行对话不能路由")
	}
	other := sessionRouterTool{policy: agentToolPolicy{UserID: "u2"}}
	if res, _ := other.Execute(context.Background(), raw, &agent.Exec{SessionID: strconv.FormatUint(uint64(f.conv.Id), 10)}); !res.IsError {
		t.Fatal("别的用户不能路由")
	}
}

// 同一 Thread 同时只有一次判断；判断期间到达的消息合并进下一批。
func TestSAEvaluateSerializesAndBatches(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	release := make(chan struct{})
	var mu sync.Mutex
	var contents []string
	saSwap(t, &saRunFrontTurn, func(_ context.Context, _ model.SAThread, content string) (string, error) {
		mu.Lock()
		contents = append(contents, content)
		first := len(contents) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return "回答:" + content, nil
	})
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "一")
	saEvaluate(context.Background(), f.thread.Id)
	saEvaluate(context.Background(), f.thread.Id)
	saWait(t, "第一轮开始", func() bool { mu.Lock(); defer mu.Unlock(); return len(contents) == 1 })
	t2 := f.trigger(t, "100", model.SAAddressedDirect, "二")
	t3 := f.trigger(t, "100", model.SAAddressedDirect, "三")
	saEvaluate(context.Background(), f.thread.Id)
	close(release)
	saWait(t, "全部决定", func() bool { return saAllDecided(t1.Id, t2.Id, t3.Id) })
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 2 || contents[0] != "一" || contents[1] != "二\n三" {
		t.Fatalf("批次不对: %q", contents)
	}
	if len(f.outbound) != 2 || f.outbound[1].DedupeKey != "reply:"+strconv.Itoa(int(t3.Id)) || f.outbound[1].ReplyToTriggerID != t3.Id || f.outbound[1].Purpose != model.SAPurposeReply {
		t.Fatalf("回答入队不对: %+v", f.outbound)
	}
	if d := saDecisionOf(t, t2.Id); d.Kind != model.SADecisionReply || d.TriggerIDs != saIDsJSON(t2.Id, t3.Id) {
		t.Fatalf("合并批次应共用一条 reply 决定: %+v", d)
	}
	saEvalWG.Wait()
	var th model.SAThread
	model.DB.First(&th, f.thread.Id)
	if th.EvalState != "idle" {
		t.Fatalf("租约未释放: %s", th.EvalState)
	}
}

func TestSAEvaluateGateFiltersSilently(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:200", nil)
	called := false
	saSwap(t, &saRunFrontTurn, func(context.Context, model.SAThread, string) (string, error) { called = true; return "x", nil })
	t1 := f.trigger(t, "200", model.SAAddressedDirect, "你好")
	saEvaluate(context.Background(), f.thread.Id)
	saWait(t, "被过滤", func() bool { return saAllDecided(t1.Id) })
	var tr model.SATrigger
	model.DB.First(&tr, t1.Id)
	d := saDecisionOf(t, t1.Id)
	if called || tr.Disposition != model.SADispFiltered || d.By != "gate" || d.Strength != model.SAStrengthNone || len(f.outbound) != 0 {
		t.Fatalf("名单外私聊应静默过滤: %+v %+v called=%v", tr, d, called)
	}
}

func TestSAEvaluateUnavailableAfterThreeFailures(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	var mu sync.Mutex
	calls := 0
	saSwap(t, &saRunFrontTurn, func(context.Context, model.SAThread, string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "", errors.New("模型挂了")
	})
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "在吗")
	saEvaluate(context.Background(), f.thread.Id)
	saWait(t, "记 unavailable", func() bool { return saAllDecided(t1.Id) })
	mu.Lock()
	defer mu.Unlock()
	if d := saDecisionOf(t, t1.Id); d.Kind != model.SADecisionUnavailable || calls != saEvalMaxFails || len(f.outbound) != 0 {
		t.Fatalf("decision=%+v calls=%d", d, calls)
	}
}

func TestSAEvaluateInferredRules(t *testing.T) {
	f := newSARouterFixture(t, "qq:group:900", nil)
	saSwap(t, &saRunFrontTurn, func(context.Context, model.SAThread, string) (string, error) {
		t.Error("推断消息不应进入前台轮次")
		return "", nil
	})
	t1 := f.trigger(t, "100", model.SAAddressedNone, "随口一说")
	saEvaluate(context.Background(), f.thread.Id)
	saWait(t, "没有规则时忽略", func() bool { return saAllDecided(t1.Id) })
	if d := saDecisionOf(t, t1.Id); d.Kind != model.SADecisionIgnore || d.WouldAccept || !strings.Contains(d.Reason, "没有启用") {
		t.Fatalf("decision=%+v", d)
	}

	v := saPolicyToView(f.policy)
	v.AcceptMode = model.SAAcceptAuto
	v.Rules = []saRule{{ID: "bug", Name: "报错", Instruction: "排查报错", Enabled: true}}
	if _, err := saSavePolicy("u1", 0, v); err != nil {
		t.Fatal(err)
	}
	saSwap(t, &saAssess, func(_ context.Context, _ model.SAThread, _ model.SAPolicy, _ []saRule, ts []model.SATrigger) ([]saAssessItem, error) {
		out := make([]saAssessItem, 0, len(ts))
		for _, tr := range ts {
			out = append(out, saAssessItem{TriggerID: tr.Id, Accept: strings.Contains(tr.Text, "报错"), RuleIDs: []string{"bug"}, Goal: "排查：" + tr.Text, Ack: "我看看"})
		}
		return out, nil
	})
	t2 := f.trigger(t, "100", model.SAAddressedNone, "构建报错了")
	t3 := f.trigger(t, "100", model.SAAddressedNone, "中午吃啥")
	saEvaluate(context.Background(), f.thread.Id)
	saWait(t, "评估完成", func() bool { return saAllDecided(t2.Id, t3.Id) })
	if d := saDecisionOf(t, t2.Id); d.Kind != model.SADecisionAccept || d.TaskID == 0 {
		t.Fatalf("命中规则应接单: %+v", d)
	}
	if d := saDecisionOf(t, t3.Id); d.Kind != model.SADecisionIgnore {
		t.Fatalf("未命中应忽略: %+v", d)
	}
	if len(f.accepted) != 1 || f.accepted[0].Strength != model.SAStrengthInferred || f.accepted[0].MatchedRule[0] != "bug" {
		t.Fatalf("accepted = %+v", f.accepted)
	}
}

func TestSAHandBackRequeuesOnce(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:100", nil)
	saSwap(t, &saEvaluate, func(context.Context, uint) {})
	f.cards = []saTicketCard{{TaskID: 5, Title: "旧工单", Status: model.TaskRunning, Stage: "working"}}
	t1 := f.trigger(t, "100", model.SAAddressedDirect, "顺便把博客也改了")
	f.route(t, `{"decisions":[{"action":"attach","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"task_id":5}]}`)
	if err := saHandBack(context.Background(), 5, t1, "超出本工单范围"); err != nil {
		t.Fatal(err)
	}
	var tr model.SATrigger
	model.DB.First(&tr, t1.Id)
	if tr.DecisionID != 0 || tr.Disposition != model.SADispQueued {
		t.Fatalf("交回后应重新排队: %+v", tr)
	}
	if err := saHandBack(context.Background(), 5, t1, "再交一次"); err == nil {
		t.Fatal("同一工单只能交回一次")
	}
	prompt := saFrontPrompt(&f.conv)
	if !strings.Contains(prompt, "trigger_id="+strconv.Itoa(int(t1.Id))) || !strings.Contains(prompt, "超出本工单范围") || !strings.Contains(prompt, "task_id=5") {
		t.Fatalf("前台提示缺少交回原因或工单卡:\n%s", prompt)
	}
	if res := f.route(t, `{"decisions":[{"action":"attach","trigger_id":`+strconv.Itoa(int(t1.Id))+`,"task_id":5}]}`); !res[0].Refused {
		t.Fatal("交回过的消息不能再交给同一工单")
	}
}

func TestSAFrontPromptEmptyWithoutThread(t *testing.T) {
	setupSATestDB(t)
	conv := model.Conversation{UserID: "u1"}
	model.DB.Create(&conv)
	if p := saFrontPrompt(&conv); p != "" {
		t.Fatalf("没有接入的会话不应有路由段: %q", p)
	}
}

func TestSAFinishInlineTurnRecordsReply(t *testing.T) {
	setupSATestDB(t)
	conv := model.Conversation{UserID: "u1"}
	model.DB.Create(&conv)
	th := model.SAThread{UserID: "u1", ThreadKey: "web:conv:" + strconv.Itoa(int(conv.Id)), Source: model.SASourceWeb, ConversationID: conv.Id}
	model.DB.Create(&th)
	tr := model.SATrigger{UserID: "u1", ThreadID: th.Id, Source: model.SASourceWeb, SourceMsgID: "1", ActorID: "u1", ActorKind: model.SAActorHuman, Addressed: model.SAAddressedDirect, Disposition: model.SADispInline}
	model.DB.Create(&tr)
	saFinishInlineTurn(conv.Id, "好的")
	model.DB.First(&tr, tr.Id)
	d := saDecisionOf(t, tr.Id)
	if d.Kind != model.SADecisionReply || tr.Disposition != model.SADispInline || d.Strength != model.SAStrengthExplicit {
		t.Fatalf("inline 轮次结束应记 reply: %+v %+v", tr, d)
	}
}
