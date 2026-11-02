package web

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestSessionRouterPersistentConversation(t *testing.T) {
	t.Setenv("AGENT_EVOLVE", "0")
	t.Setenv("AGENT_ENGINE", "legacy")
	t.Setenv("AGENT_MODE", "confirm")
	t.Setenv("AGENT_COMPACT", "0")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "router.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Conversation{}, &model.Message{}, &model.AgentSubagentRun{}, &model.AgentSetting{}, &model.User{}); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sqlDB, _ := db.DB(); sqlDB.Close() })
	parent := model.Conversation{UserID: "router-owner", Model: "router-test", Kind: KindCode}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatal(err)
	}
	// Suppress the separate parent model run; this test verifies worker delivery
	// and completion notices and records both worker requests deterministically.
	wakeMu.Lock()
	wakeWindow[parent.Id] = time.Now()
	wakeCount[parent.Id] = wakeMaxPerHour
	wakeMu.Unlock()
	t.Cleanup(func() {
		wakeMu.Lock()
		delete(wakeWindow, parent.Id)
		delete(wakeCount, parent.Id)
		delete(wakeLast, parent.Id)
		wakeMu.Unlock()
	})
	requests := make(chan llm.ChatRequest, 4)
	fullReply := strings.Repeat("验证结果。", 600)
	oldStreamer := agent.Streamer()
	agent.SetStreamer(func(_ context.Context, _, _ string, req llm.ChatRequest) (<-chan llm.Event, error) {
		requests <- req
		ch := make(chan llm.Event, 2)
		ch <- llm.Event{Type: llm.EventDelta, Content: fullReply}
		ch <- llm.Event{Type: llm.EventDone}
		close(ch)
		return ch, nil
	})
	t.Cleanup(func() { agent.SetStreamer(oldStreamer) })
	tool := sessionRouterTool{policy: agentToolPolicy{UserID: parent.UserID, Kind: KindCode, Mode: ModeConfirm}}
	ex := &agent.Exec{SessionID: strconv.FormatUint(uint64(parent.Id), 10)}
	call := func(raw string) agent.Result {
		t.Helper()
		result, err := tool.Execute(context.Background(), json.RawMessage(raw), ex)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if res := call(`{"action":"accept","target":"worker-one","message":"保留约束甲，完成检查并验证","title":"检查"}`); res.IsError {
		t.Fatal(res.Content)
	}
	wait := func(n int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var notices int64
			db.Model(&model.Message{}).Where("conversation_id = ? AND role = ?", parent.Id, subagentMessageRole).Count(&notices)
			if notices >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("completion notice was not persisted")
	}
	wait(1)
	if res := call(`{"action":"attach","target":"worker-one","message":"在原结果上继续检查乙"}`); res.IsError {
		t.Fatal(res.Content)
	}
	wait(2)
	first, second := <-requests, <-requests
	if len(second.Messages) <= len(first.Messages) {
		t.Fatal("continuation lost conversation history")
	}
	var contents strings.Builder
	for _, m := range second.Messages {
		contents.WriteString(m.Content)
	}
	if !strings.Contains(contents.String(), "约束甲") || !strings.Contains(contents.String(), fullReply) || !strings.Contains(contents.String(), "检查乙") {
		t.Fatal("original goal, full result or follow-up missing")
	}
	var runs []model.AgentSubagentRun
	db.Order("id asc").Find(&runs)
	if len(runs) != 2 || runs[0].ChildConversationID != runs[1].ChildConversationID || runs[0].Depth != runs[1].Depth {
		t.Fatalf("not the same persistent session: %+v", runs)
	}
	foreign := sessionRouterTool{policy: agentToolPolicy{UserID: "other-owner"}}
	res, err := foreign.Execute(context.Background(), json.RawMessage(`{"action":"list"}`), ex)
	if err != nil || !res.IsError {
		t.Fatal("cross-user access accepted")
	}
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`), &agent.Exec{SessionID: strconv.FormatUint(uint64(runs[0].ChildConversationID), 10)})
	if err != nil || !res.IsError {
		t.Fatal("worker gained router authority")
	}
	if res := call(`{"action":"attach","target":"missing-member","message":"继续"}`); !res.IsError {
		t.Fatal("unknown target accepted")
	}
	// Exercise stored-summary reuse against real message IDs: five older
	// messages are new but below the six-message summary refresh threshold.
	t.Setenv("AGENT_COMPACT", "1")
	t.Setenv("AGENT_COMPACT_KEEP", "10")
	t.Setenv("AGENT_COMPACT_TRIGGER", "24")
	long := model.Conversation{UserID: parent.UserID, Kind: KindCode}
	if err := db.Create(&long).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]model.Message, 30)
	for i := range rows {
		rows[i] = model.Message{ConversationID: long.Id, Role: "user", Content: strconv.Itoa(i)}
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	long.Summary, long.SummaryUpto = "此前约束", rows[14].Id
	cr := maybeCompactHistory(context.Background(), &long, toLLMMessages(rows), "unused", "")
	if !cr.Compacted || len(cr.History) != 15 || cr.History[0].Content != "15" || cr.History[14].Content != "29" {
		t.Fatal("stored-summary reuse lost the unsummarized gap or latest input")
	}
}

func TestCompactKeepsUnsummarizedGap(t *testing.T) {
	rows := []model.Message{{Id: 10}, {Id: 20}, {Id: 30}, {Id: 40}}
	got := uncoveredCompactRows(rows, 20)
	if len(got) != 2 || got[0].Id != 30 {
		t.Fatalf("unsummarized messages discarded: %+v", got)
	}
}

func TestSessionRouterPolicy(t *testing.T) {
	for _, tc := range []struct {
		kind, mode string
		depth      int
		want       bool
	}{
		{KindWork, ModeAuto, 0, true}, {KindCode, ModeConfirm, 0, true},
		{KindWork, ModePlan, 0, false}, {KindChat, ModeAuto, 0, false}, {KindWork, ModeAuto, 1, false},
	} {
		found := false
		for _, tool := range buildAgentTools(agentToolPolicy{Kind: tc.kind, Mode: tc.mode, Depth: tc.depth}) {
			if tool.Name() == "orchestrator_route" {
				found = true
			}
		}
		if found != tc.want {
			t.Fatalf("router availability %+v: %v", tc, found)
		}
	}
}
