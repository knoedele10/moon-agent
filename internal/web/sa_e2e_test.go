package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

type saE2EChannel struct {
	mu   sync.Mutex
	sent []model.SAOutbound
}

func (c *saE2EChannel) Send(_ context.Context, _ model.SAThread, out model.SAOutbound, _ *model.SATrigger) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, out)
	return fmt.Sprintf("qq-out-%d", len(c.sent)), nil
}

func (c *saE2EChannel) all() []model.SAOutbound {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.SAOutbound(nil), c.sent...)
}

func saE2EEvent(t *testing.T, actor string, msgID int, message string) qqEvent {
	t.Helper()
	var ev qqEvent
	raw := fmt.Sprintf(`{"post_type":"message","message_type":"private","self_id":1,"user_id":%s,"message_id":%d,"sender":{"nickname":"n%s"},"message":%s}`, actor, msgID, actor, message)
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// 验收：QQ 主人私聊 → A 入口 → B 判断与路由 → C 建工单 → A 发件箱 → QQ 渠道；
// 工单提问 → 引用回复被工单认领（不再进判断）→ 结局发回；陌生人被静默过滤。
func TestSAEndToEndQQOwnerTicket(t *testing.T) {
	setupSATestDB(t)
	model.DB.Create(&model.User{Id: "u1", Username: "owner"})
	model.DB.Create(&model.Conversation{UserID: "u1", Title: "web", Model: "m1", Kind: KindWork})
	t.Setenv("QQ_OWNER", "42")
	t.Setenv("QQ_ALLOW_USERS", "")
	t.Setenv("QQ_ALLOW_GROUPS", "")

	ch := &saE2EChannel{}
	channels := map[string]saChannel{}
	for k, v := range saChannels {
		channels[k] = v
	}
	channels[model.SASourceQQ] = ch
	saSwap(t, &saChannels, channels)
	saSwap(t, &saEvalRetryBase, 10*time.Millisecond)

	var mu sync.Mutex
	frontTurns := 0
	// 模拟前台模型：看到待处理消息就用路由工具接单，然后回一句话。
	saSwap(t, &saRunFrontTurn, func(ctx context.Context, th model.SAThread, content string) (string, error) {
		mu.Lock()
		frontTurns++
		mu.Unlock()
		var pending []model.SATrigger
		model.DB.Where("thread_id = ? AND decision_id = 0", th.Id).Find(&pending)
		if len(pending) == 0 {
			return "", nil
		}
		raw := fmt.Sprintf(`{"decisions":[{"action":"accept","trigger_id":%d,"title":"周报","goal":%q,"criteria":["包含三项进展"],"ack":"好的，我来整理周报"}]}`, pending[0].Id, content)
		res, err := sessionRouterTool{policy: agentToolPolicy{UserID: th.UserID}}.Execute(ctx, json.RawMessage(raw), &agent.Exec{SessionID: strconv.FormatUint(uint64(th.ConversationID), 10)})
		if err != nil || res.IsError {
			return "", fmt.Errorf("route: %v %s", err, res.Content)
		}
		return "已交给后台处理，完成后告诉你。", nil
	})
	t.Cleanup(saEvalWG.Wait)
	cfg := qqConfig{UserID: "u1", Kind: KindChat, Timeout: time.Minute}
	send := func() { saSendPending(context.Background()) }

	// 1. 主人私聊请求干活。
	qqHandleMessage(cfg, saE2EEvent(t, "42", 1001, `[{"type":"text","data":{"text":"帮我把周报整理好"}}]`))
	var origin model.SATrigger
	saWait(t, "第一条消息有决定", func() bool {
		return model.DB.Where("source_msg_id = ?", "1001").First(&origin).Error == nil && origin.DecisionID != 0
	})
	saEvalWG.Wait()
	model.DB.First(&origin, origin.Id)
	d := saDecisionOf(t, origin.Id)
	if d.Kind != model.SADecisionAccept || d.TaskID == 0 || d.Strength != model.SAStrengthExplicit {
		t.Fatalf("应接单：%+v", d)
	}
	var task model.AgentTask
	if err := model.DB.First(&task, d.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	var m model.SAMandate
	if err := model.DB.Where("task_id = ?", task.Id).First(&m).Error; err != nil || m.ActorID != "42" || m.OriginTriggerID != origin.Id {
		t.Fatalf("授权记录不对：%+v %v", m, err)
	}
	if task.Kind == KindChat {
		t.Fatalf("主人的工单不应被降到聊天档：%s", task.Kind)
	}
	send()
	sent := ch.all()
	if len(sent) != 2 || sent[0].Purpose != model.SAPurposeAck || sent[1].Purpose != model.SAPurposeReply {
		t.Fatalf("应先发受理确认、再发前台回答：%+v", sent)
	}

	// 2. 工单要问人：问题发回原对话；主人引用这条问题回答，直接被工单认领。
	if ok, err := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{"status": model.TaskWaitingHuman, "stop_reason": "need_human", "blocker": "缺数据"}); err != nil || !ok {
		t.Fatalf("进入 waiting_human 失败：%v %v", ok, err)
	}
	model.DB.First(&task, task.Id)
	notifyTask(task, model.SAPurposeQuestion, "本周的数据在哪？")
	send()
	sent = ch.all()
	if len(sent) != 3 || sent[2].Purpose != model.SAPurposeQuestion || sent[2].TaskID != task.Id {
		t.Fatalf("提问没发回 QQ：%+v", sent)
	}
	var q model.SAOutbound
	model.DB.First(&q, sent[2].Id)
	if q.SentRef == "" {
		t.Fatal("提问发出后应记下 sent_ref")
	}
	mu.Lock()
	turnsBefore := frontTurns
	mu.Unlock()
	qqHandleMessage(cfg, saE2EEvent(t, "42", 1002, fmt.Sprintf(`[{"type":"reply","data":{"id":%q}},{"type":"text","data":{"text":"在共享盘 /weekly"}}]`, q.SentRef)))
	var answer model.SATrigger
	saWait(t, "回答入库", func() bool { return model.DB.Where("source_msg_id = ?", "1002").First(&answer).Error == nil })
	saEvalWG.Wait()
	model.DB.First(&answer, answer.Id)
	model.DB.First(&task, task.Id)
	mu.Lock()
	turnsAfter := frontTurns
	mu.Unlock()
	if answer.Disposition != model.SADispFannedOut || answer.ReplyToMsgID != q.SentRef || task.Status != model.TaskRunning || turnsAfter != turnsBefore {
		t.Fatalf("引用回答应直接交给工单：disp=%s reply_to=%q status=%s turns %d→%d", answer.Disposition, answer.ReplyToMsgID, task.Status, turnsBefore, turnsAfter)
	}

	// 3. 完成：结局发回原对话。
	if ok, err := transitionTask(task.Id, []string{model.TaskRunning}, map[string]any{"status": model.TaskDone, "stop_reason": "verified"}); err != nil || !ok {
		t.Fatalf("完成失败：%v %v", ok, err)
	}
	model.DB.First(&task, task.Id)
	notifyTask(task, model.SAPurposeResult, "周报已整理好。")
	send()
	sent = ch.all()
	if len(sent) != 4 || sent[3].Purpose != model.SAPurposeResult {
		t.Fatalf("结局没发回 QQ：%+v", sent)
	}

	// 4. 陌生人私聊：静默过滤，不回话、不进前台。
	qqHandleMessage(cfg, saE2EEvent(t, "77", 2001, `[{"type":"text","data":{"text":"你好"}}]`))
	var stranger model.SATrigger
	saWait(t, "陌生人消息有决定", func() bool {
		return model.DB.Where("source_msg_id = ?", "2001").First(&stranger).Error == nil && stranger.DecisionID != 0
	})
	saEvalWG.Wait()
	send()
	model.DB.First(&stranger, stranger.Id)
	if stranger.Disposition != model.SADispFiltered || len(ch.all()) != 4 {
		t.Fatalf("陌生人应被过滤且不回话：%s，发出 %d 条", stranger.Disposition, len(ch.all()))
	}
}
