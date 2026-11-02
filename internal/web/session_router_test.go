package web

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"blog-my/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 旧版基于团队成员的 accept/attach 已被 docs/agent/18 的批量路由取代（见 sa_router_test.go）；
// 这里保留压缩摘要复用的回归。
func TestCompactStoredSummaryReuse(t *testing.T) {
	t.Setenv("AGENT_COMPACT", "1")
	t.Setenv("AGENT_COMPACT_KEEP", "10")
	t.Setenv("AGENT_COMPACT_TRIGGER", "24")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "compact.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Conversation{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; sqlDB, _ := db.DB(); sqlDB.Close() })
	// Exercise stored-summary reuse against real message IDs: five older
	// messages are new but below the six-message summary refresh threshold.
	long := model.Conversation{UserID: "owner", Kind: KindCode}
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
		{KindWork, ModeAuto, 0, true}, {KindCode, ModeConfirm, 0, true}, {KindChat, ModeAuto, 0, true},
		{KindWork, ModePlan, 0, false}, {KindWork, ModeAuto, 1, false},
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
