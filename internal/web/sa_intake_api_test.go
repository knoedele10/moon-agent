package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

func saAPIEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("APP_SECRET", "sa-api-test-secret")
	if err := model.DB.AutoMigrate(&model.UserSession{}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	api := r.Group("/api", authRequired)
	for _, register := range saRouteRegistrars {
		register(api)
	}
	return r
}

func saAPIUser(t *testing.T, id string) {
	t.Helper()
	if err := model.DB.Create(&model.User{Id: id, Username: id, Password: "x", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
}

func saDo(r *gin.Engine, method, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if ck != nil {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSAThreadsAPIRequiresLogin(t *testing.T) {
	setupSATestDB(t)
	saAPIUser(t, "u1")
	r := saAPIEngine(t)
	if w := saDo(r, http.MethodGet, "/api/sa/threads", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous threads: got %d body=%s", w.Code, w.Body.String())
	}
	if w := saDo(r, http.MethodGet, "/api/sa/outbound?status=failed", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous outbound: got %d", w.Code)
	}
	ck := loginCookie(t, "u1")
	w := saDo(r, http.MethodGet, "/api/sa/threads", ck)
	if w.Code != http.StatusOK {
		t.Fatalf("logged-in threads: got %d body=%s", w.Code, w.Body.String())
	}
	if stringsTrim(w.Body.String()) != "[]" {
		t.Fatalf("empty list = %s", w.Body.String())
	}
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func TestSAThreadsAPIListFilterAndIsolation(t *testing.T) {
	db := setupSATestDB(t)
	saAPIUser(t, "u1")
	saAPIUser(t, "u2")
	r := saAPIEngine(t)
	rows := []model.SAThread{
		{UserID: "u1", ThreadKey: "qq:private:1", Source: model.SASourceQQ, Label: "qq"},
		{UserID: "u1", ThreadKey: "web:conv:1", Source: model.SASourceWeb, Label: "web"},
		{UserID: "u2", ThreadKey: "qq:private:2", Source: model.SASourceQQ, Label: "other"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ck := loginCookie(t, "u1")
	w := saDo(r, http.MethodGet, "/api/sa/threads", ck)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var got []model.SAThread
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ThreadKey != "web:conv:1" || got[1].ThreadKey != "qq:private:1" {
		t.Fatalf("threads = %+v", got)
	}
	w = saDo(r, http.MethodGet, "/api/sa/threads?source=qq", ck)
	got = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ThreadKey != "qq:private:1" || got[0].UserID != "u1" {
		t.Fatalf("filtered = %+v", got)
	}
	w = saDo(r, http.MethodGet, "/api/sa/threads?source=wakeup&limit=10", ck)
	if stringsTrim(w.Body.String()) != "[]" {
		t.Fatalf("empty filter = %s", w.Body.String())
	}
	w = saDo(r, http.MethodGet, "/api/sa/threads?limit=1", ck)
	got = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ThreadKey != "web:conv:1" {
		t.Fatalf("limit = %+v", got)
	}
}

func TestSAThreadsAPITriggers(t *testing.T) {
	db := setupSATestDB(t)
	saAPIUser(t, "u1")
	saAPIUser(t, "u2")
	r := saAPIEngine(t)
	own := model.SAThread{UserID: "u1", ThreadKey: "qq:group:9", Source: model.SASourceQQ}
	other := model.SAThread{UserID: "u2", ThreadKey: "qq:group:8", Source: model.SASourceQQ}
	if err := db.Create(&own).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		tr := model.SATrigger{UserID: "u1", ThreadID: own.Id, Source: model.SASourceQQ, SourceMsgID: id, Text: id, Disposition: model.SADispQueued}
		if err := db.Create(&tr).Error; err != nil {
			t.Fatal(err)
		}
	}
	hidden := model.SATrigger{UserID: "u2", ThreadID: other.Id, Source: model.SASourceQQ, SourceMsgID: "secret", Text: "nope"}
	if err := db.Create(&hidden).Error; err != nil {
		t.Fatal(err)
	}
	ck := loginCookie(t, "u1")
	if w := saDo(r, http.MethodGet, "/api/sa/threads/abc/triggers", ck); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", w.Code)
	}
	if w := saDo(r, http.MethodGet, "/api/sa/threads/0/triggers", ck); w.Code != http.StatusBadRequest {
		t.Fatalf("zero id: %d", w.Code)
	}
	if w := saDo(r, http.MethodGet, "/api/sa/threads/99999/triggers", ck); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", w.Code, w.Body.String())
	}
	if w := saDo(r, http.MethodGet, "/api/sa/threads/"+strconv.FormatUint(uint64(other.Id), 10)+"/triggers", ck); w.Code != http.StatusNotFound {
		t.Fatalf("other user thread: %d %s", w.Code, w.Body.String())
	}
	w := saDo(r, http.MethodGet, "/api/sa/threads/"+strconv.FormatUint(uint64(own.Id), 10)+"/triggers?limit=2", ck)
	if w.Code != http.StatusOK {
		t.Fatalf("triggers: %d %s", w.Code, w.Body.String())
	}
	var got []model.SATrigger
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SourceMsgID != "m1" || got[1].SourceMsgID != "m2" {
		t.Fatalf("triggers = %+v", got)
	}
	w = saDo(r, http.MethodGet, "/api/sa/threads/"+strconv.FormatUint(uint64(own.Id), 10)+"/triggers", ck)
	got = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].SourceMsgID != "m3" {
		t.Fatalf("all triggers = %+v", got)
	}
}

func TestSAThreadsAPILimitClamp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		raw  string
		want int
	}{
		{"", 100},
		{"0", 100},
		{"-3", 100},
		{"abc", 100},
		{"25", 25},
		{"200", 200},
		{"9999", 200},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		target := "/api/sa/threads"
		if tc.raw != "" {
			target += "?limit=" + tc.raw
		}
		c.Request = httptest.NewRequest(http.MethodGet, target, nil)
		if got := saAPILimit(c); got != tc.want {
			t.Fatalf("limit %q = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestSAOutboundAPIFailedStatus(t *testing.T) {
	db := setupSATestDB(t)
	saAPIUser(t, "u1")
	saAPIUser(t, "u2")
	r := saAPIEngine(t)
	th := model.SAThread{UserID: "u1", ThreadKey: "qq:private:5", Source: model.SASourceQQ}
	if err := db.Create(&th).Error; err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	rows := []model.SAOutbound{
		{UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeReply, Text: "pending", DedupeKey: "p", Status: model.SAOutPending, NextAttemptAt: &later},
		{UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeResult, Text: "failed", DedupeKey: "f", Status: model.SAOutFailed, NextAttemptAt: &later},
		{UserID: "u1", ThreadID: th.Id, Purpose: model.SAPurposeAck, Text: "sent", DedupeKey: "s", Status: model.SAOutSent, SentRef: "1"},
		{UserID: "u2", ThreadID: th.Id, Purpose: model.SAPurposeReply, Text: "other", DedupeKey: "o", Status: model.SAOutFailed, NextAttemptAt: &later},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ck := loginCookie(t, "u1")
	w := saDo(r, http.MethodGet, "/api/sa/outbound?status=failed", ck)
	if w.Code != http.StatusOK {
		t.Fatalf("failed: %d %s", w.Code, w.Body.String())
	}
	var got []model.SAOutbound
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DedupeKey != "f" || got[0].Status != model.SAOutFailed || got[0].UserID != "u1" {
		t.Fatalf("failed rows = %+v", got)
	}
	w = saDo(r, http.MethodGet, "/api/sa/outbound", ck)
	got = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].DedupeKey != "s" || got[2].DedupeKey != "p" {
		t.Fatalf("all rows = %+v", got)
	}
}
