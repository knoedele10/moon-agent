package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blog-my/internal/content"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 这组用例只测接线：环境变量解析、调度开关、两个 handler 的返回形状与用户隔离。
// 执行器本身的分支（退避、次数用尽、租约）在 internal/content/job_test.go 里测。

type stubJobStore struct {
	pubs   []model.ContentPublication
	marked map[uint]string
}

func newStubJobStore(pubs ...model.ContentPublication) *stubJobStore {
	return &stubJobStore{pubs: pubs, marked: map[uint]string{}}
}

func (s *stubJobStore) DuePublications(userID string, now time.Time, limit int) ([]model.ContentPublication, error) {
	out := make([]model.ContentPublication, 0, len(s.pubs))
	for _, p := range s.pubs {
		if userID != "" && p.UserID != userID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *stubJobStore) CountDuePublications(userID string, now time.Time) (int64, error) {
	var n int64
	for _, p := range s.pubs {
		if p.UserID == userID {
			n++
		}
	}
	return n, nil
}

func (s *stubJobStore) ClaimPublication(id uint, now, leaseUntil time.Time) (model.ContentPublication, bool, error) {
	for _, p := range s.pubs {
		if p.Id == id {
			return p, true, nil
		}
	}
	return model.ContentPublication{}, false, nil
}

func (s *stubJobStore) ExportDraft(userID string, draftID uint, channel string) (content.Export, error) {
	return content.Export{
		Draft:  model.ContentDraft{Id: draftID, Title: "标题"},
		Title:  "标题",
		BodyMd: "正文",
	}, nil
}

func (s *stubJobStore) MarkExported(p model.ContentPublication, path string) (model.ContentPublication, error) {
	s.marked[p.Id] = path
	p.Status = content.PubExported
	return p, nil
}

func (s *stubJobStore) RecordExportFailure(p model.ContentPublication, cause string, retryAt *time.Time, maxAttempts int) (model.ContentPublication, error) {
	return p, nil
}

// installRunner 手动装一个执行器，并确保 Once 不会再用 model.DB 重建它。
func installRunner(t *testing.T, r *content.Runner) {
	t.Helper()
	contentRunnerOnce.Do(func() {})
	contentRunnerInst = r
	t.Cleanup(func() { contentRunnerInst = nil })
}

func callAPI(t *testing.T, handler gin.HandlerFunc, method, path, userID string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(ctxUserIDKey, userID)
		c.Next()
	})
	r.Handle(method, path, handler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("返回不是 JSON：%v\n%s", err, w.Body.String())
	}
	return w, payload
}

func TestContentJobEnv(t *testing.T) {
	t.Setenv("CONTENT_PUBLISH_INTERVAL_MINUTES", "")
	if got := contentJobIntervalMinutes(); got != 5 {
		t.Fatalf("默认间隔应为 5，实际 %d", got)
	}
	t.Setenv("CONTENT_PUBLISH_INTERVAL_MINUTES", "0")
	if got := contentJobIntervalMinutes(); got != 0 {
		t.Fatalf("显式 0 应关闭，实际 %d", got)
	}
	t.Setenv("CONTENT_PUBLISH_INTERVAL_MINUTES", "不是数字")
	if got := contentJobIntervalMinutes(); got != 5 {
		t.Fatalf("非法值应回落默认，实际 %d", got)
	}
	t.Setenv("CONTENT_EXPORT_DIR", ` D:\exports `)
	if got := contentExportDir(); got != `D:\exports` {
		t.Fatalf("导出目录应去空白，实际 %q", got)
	}
	t.Setenv("CONTENT_EXPORT_DIR", "")
	if got := contentExportDir(); !strings.Contains(got, "blog-my") || !strings.Contains(got, "content-publish") {
		t.Fatalf("默认导出目录应落在用户配置目录下，实际 %q", got)
	}
}

func TestStartContentSchedulerDisabled(t *testing.T) {
	t.Setenv("CONTENT_PUBLISH_INTERVAL_MINUTES", "0")
	StartContentPublishScheduler(t.Context())
	if contentSchedulerRunning() {
		t.Fatal("间隔为 0 时不该启动调度")
	}
	StopContentPublishScheduler()
}

func TestContentJobsStatusUnavailable(t *testing.T) {
	contentRunnerOnce.Do(func() {})
	contentRunnerInst = nil
	t.Cleanup(func() { contentRunnerInst = nil })

	w, payload := callAPI(t, contentJobsStatus, http.MethodGet, "/api/content/jobs", "u1")
	if w.Code != http.StatusOK {
		t.Fatalf("状态接口应永远 200，实际 %d", w.Code)
	}
	if payload["configured"] != false || payload["mode"] != "export" {
		t.Fatalf("没数据库时应如实报不可用：%v", payload)
	}
	if payload["reason"] == "" {
		t.Fatal("应说明不可用原因")
	}
}

func TestContentJobsStatusAndRun(t *testing.T) {
	planned := time.Now().Add(-time.Minute)
	store := newStubJobStore(model.ContentPublication{
		Id: 1, UserID: "u1", DraftID: 11, Channel: "wechat",
		Status: content.PubPlanned, PlannedAt: &planned,
	})
	now := time.Now()
	runner := content.NewRunner(store, content.PackagePublisher{Dir: t.TempDir()})
	runner.Now = func() time.Time { return now }
	installRunner(t, runner)

	_, payload := callAPI(t, contentJobsStatus, http.MethodGet, "/api/content/jobs", "u1")
	if payload["configured"] != true {
		t.Fatalf("装好执行器后应 configured=true：%v", payload)
	}
	if due, _ := payload["due_count"].(float64); due != 1 {
		t.Fatalf("当前用户应有 1 条待导出，实际 %v", payload["due_count"])
	}

	w, run := callAPI(t, contentJobsRun, http.MethodPost, "/api/content/jobs/run", "u1")
	if w.Code != http.StatusOK {
		t.Fatalf("手动触发应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if exported, _ := run["exported"].(float64); exported != 1 {
		t.Fatalf("应导出 1 条：%v", run)
	}
	if store.marked[1] == "" {
		t.Fatal("台账应记下成品包路径")
	}

	// 别人的视角：待导出 0，最近记录里看不到 u1 的东西。
	_, other := callAPI(t, contentJobsStatus, http.MethodGet, "/api/content/jobs", "u2")
	if due, _ := other["due_count"].(float64); due != 0 {
		t.Fatalf("u2 不该看到 u1 的排期：%v", other["due_count"])
	}
	if recent, _ := other["recent"].([]any); len(recent) != 0 {
		t.Fatalf("u2 不该看到 u1 的运行记录：%v", recent)
	}
	if recent, _ := payload["recent"].([]any); len(recent) != 0 {
		t.Fatalf("跑之前不该有记录：%v", recent)
	}

	// 跑完之后再看 u1：最近记录里有这一条。
	_, after := callAPI(t, contentJobsStatus, http.MethodGet, "/api/content/jobs", "u1")
	recent, _ := after["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("u1 应有 1 条运行记录，实际 %v", after["recent"])
	}
	item, _ := recent[0].(map[string]any)
	if item["publication_id"].(float64) != 1 || item["status"] != content.JobExported {
		t.Fatalf("运行记录内容不对：%v", item)
	}
}

func TestContentJobsRunUnavailable(t *testing.T) {
	contentRunnerOnce.Do(func() {})
	contentRunnerInst = nil
	t.Cleanup(func() { contentRunnerInst = nil })

	w, payload := callAPI(t, contentJobsRun, http.MethodPost, "/api/content/jobs/run", "u1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("不可用时应 503，实际 %d", w.Code)
	}
	if payload["error"] == nil {
		t.Fatal("应返回错误说明")
	}
}
