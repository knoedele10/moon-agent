package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// Cursor pages must be chronological, disjoint and owned by the authenticated user.
func TestMessageCursorHTTP(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("requires the local MySQL test configuration: AGENT_IT=1")
	}
	r, userID := agentHTTPTestSetup(t)
	r.GET("/api/conversations/:id/messages", listMessages)
	conv := model.Conversation{UserID: userID, Title: "cursor test"}
	if err := model.DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]model.Message, 125)
	for i := range rows {
		rows[i] = model.Message{ConversationID: conv.Id, Role: "user", Content: fmt.Sprintf("row %d", i)}
	}
	if err := model.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	request := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/conversations/%d/messages%s", conv.Id, query), nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		return w
	}
	var page struct {
		Messages []messageItem `json:"messages"`
		HasMore  bool          `json:"has_more"`
		Before   uint          `json:"before"`
	}
	seen := map[uint]bool{}
	before := uint(0)
	for _, count := range []int{60, 60, 5} {
		query := "?limit=60"
		if before > 0 {
			query += fmt.Sprintf("&before=%d", before)
		}
		if err := json.Unmarshal(request(query).Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != count || page.HasMore != (count == 60) {
			t.Fatalf("invalid page: size=%d more=%v", len(page.Messages), page.HasMore)
		}
		for i, row := range page.Messages {
			if seen[row.ID] || (i > 0 && row.ID <= page.Messages[i-1].ID) {
				t.Fatal("duplicate or unordered message")
			}
			seen[row.ID] = true
		}
		before = page.Before
	}
	var legacy []messageItem
	if err := json.Unmarshal(request("").Body.Bytes(), &legacy); err != nil || len(legacy) != 125 {
		t.Fatal("legacy array response changed")
	}
	// The same ID must not be visible from another authenticated user.
	other := gin.New()
	other.Use(func(c *gin.Context) { c.Set(ctxUserIDKey, "other-cursor-user"); c.Next() })
	other.GET("/api/conversations/:id/messages", listMessages)
	w := httptest.NewRecorder()
	other.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/conversations/%d/messages?limit=60", conv.Id), nil))
	if w.Code != 404 {
		t.Fatalf("foreign conversation exposed: %d", w.Code)
	}
}
