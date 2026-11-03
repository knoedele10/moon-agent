package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 工单详情接口带上授权（来源、发起人、强度）与订阅；普通长任务没有这两项。
func TestSATicketTaskAPIShowsMandate(t *testing.T) {
	h := newSATicketHarness(t, KindWork)
	h.makeOwner()
	origin := h.newTrigger("整理周报")
	task := h.mustAccept(origin)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxUserIDKey, h.owner); c.Next() })
	r.GET("/api/tasks/:id", getTaskAPI)
	get := func(id uint) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tasks/%d", id), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/tasks/%d = %d %s", id, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if inner, ok := body["task"].(map[string]any); ok {
			return inner
		}
		return body
	}

	body := get(task.Id)
	m, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("工单详情缺少 mandate: %v", body)
	}
	if m["source"] != model.SASourceQQ || m["source_label"] == "" || m["actor_id"] != origin.ActorID ||
		m["strength"] != model.SAStrengthExplicit || m["origin_trigger_id"] != float64(origin.Id) ||
		m["thread_id"] != float64(h.th.Id) || m["withdrawn_at"] != nil {
		t.Fatalf("mandate = %v", m)
	}
	subs, ok := body["subscriptions"].([]any)
	if !ok || len(subs) != 1 {
		t.Fatalf("subscriptions = %v", body["subscriptions"])
	}
	if s := subs[0].(map[string]any); s["thread_id"] != float64(h.th.Id) || s["role"] != model.SASubOrigin {
		t.Fatalf("origin 订阅不对: %v", s)
	}

	// 叫停后接口如实显示 withdrawn_at。
	if err := saStopTicket(t.Context(), task.Id, "router", "不做了"); err != nil {
		t.Fatal(err)
	}
	if m := get(task.Id)["mandate"].(map[string]any); m["withdrawn_at"] == nil {
		t.Fatalf("叫停后应有 withdrawn_at: %v", m)
	}

	// 普通长任务（不经过 superagent 接单）没有 mandate / subscriptions。
	plain, err := createTask(taskSpec{Goal: "普通任务", Criteria: []taskCriterion{{Text: "完成", Method: criterionManual}}},
		taskCreateOptions{UserID: h.owner, MainConversationID: h.conv.Id, Model: "sa-test", Kind: KindWork, Mode: ModeConfirm})
	if err != nil {
		t.Fatal(err)
	}
	pb := get(plain.Id)
	if _, has := pb["mandate"]; has {
		t.Fatalf("普通任务不应有 mandate: %v", pb["mandate"])
	}
	if _, has := pb["subscriptions"]; has {
		t.Fatal("普通任务不应有 subscriptions")
	}
}
