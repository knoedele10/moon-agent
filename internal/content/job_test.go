package content

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blog-my/internal/model"
)

// 这组用例只测自动任务里「不需要 MySQL」的部分：退避、文件名、成品包渲染，
// 以及用内存假存储驱动的整轮分支（成功 / 失败重试 / 次数用尽 / 被抢 / 并发）。
// 真库上的租约与状态推进见 store_db_test.go（CONTENT_IT=1）。

// ---------- 纯函数 ----------

func TestBackoffFor(t *testing.T) {
	const base, max = time.Minute, 30 * time.Minute
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{5, 16 * time.Minute},
		{6, 30 * time.Minute}, // 32 分钟被上限压住
		{50, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := BackoffFor(c.attempts, base, max); got != c.want {
			t.Fatalf("BackoffFor(%d) = %v，期望 %v", c.attempts, got, c.want)
		}
	}
	// 配置写坏也不能退化成「立刻重试」。
	if got := BackoffFor(1, 0, 0); got != time.Minute {
		t.Fatalf("非法参数应回落 1 分钟，实际 %v", got)
	}
	if got := BackoffFor(3, 10*time.Minute, time.Minute); got != 10*time.Minute {
		t.Fatalf("max < base 时应至少等于 base，实际 %v", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"本周 AI 周报：RAG 到底怎么落地？": "本周-AI-周报-RAG-到底怎么落地",
		"  ///  ":      "",
		"a/b\\c:d*e?f": "a-b-c-d-e-f",
		"标题":           "标题",
	}
	for in, want := range cases {
		if got := Slugify(in, 40); got != want {
			t.Fatalf("Slugify(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := Slugify("一二三四五六七八九十", 4); got != "一二三四" {
		t.Fatalf("应按 rune 截断，实际 %q", got)
	}
}

func TestExportFileName(t *testing.T) {
	pub := model.ContentPublication{Id: 12, DraftID: 7, Channel: "wechat"}
	if got := ExportFileName(pub, "本周 AI 周报"); got != "pub-12-wechat-本周-AI-周报.md" {
		t.Fatalf("文件名 = %q", got)
	}
	if got := ExportFileName(model.ContentPublication{Id: 3, DraftID: 9, Channel: "x"}, ""); got != "pub-3-x-draft-9.md" {
		t.Fatalf("空标题应回落到 draft id，实际 %q", got)
	}
}

func TestSafeSegment(t *testing.T) {
	for _, in := range []string{"../../etc", `C:\Windows`, "..", "  "} {
		got := SafeSegment(in, 64)
		if strings.ContainsAny(got, `/\:`) || strings.HasPrefix(got, ".") {
			t.Fatalf("SafeSegment(%q) = %q，仍可能越出导出目录", in, got)
		}
	}
	if got := SafeSegment("content-it-123", 64); got != "content-it-123" {
		t.Fatalf("正常 id 不该被改写，实际 %q", got)
	}
}

func TestRenderPackage(t *testing.T) {
	planned := time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local)
	e := Export{
		Draft:  model.ContentDraft{Id: 7, Title: "原标题", Tags: `["AI","周报"]`},
		Title:  "公众号版标题",
		BodyMd: "第一段\n\n第二段",
		Assets: []model.ContentAsset{
			{Kind: AssetImage, Title: "封面", FileKey: "uploads/a.png", LicenseNote: "可商用"},
			{Kind: AssetLink, Title: "引用", Url: "https://example.com/x"},
		},
	}
	pub := model.ContentPublication{Id: 12, DraftID: 7, Channel: "wechat", PlannedAt: &planned}
	out := RenderPackage(e, pub, time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local))

	for _, want := range []string{
		"# 公众号版标题",
		"渠道：`wechat`",
		"计划时间：2026-10-25 09:00",
		"稿件：#7",
		"标签：AI / 周报",
		"## 正文",
		"第二段",
		"## 素材清单",
		"封面",
		"uploads/a.png",
		"可商用",
		"https://example.com/x",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("成品包缺少 %q：\n%s", want, out)
		}
	}
	// 没排期时间也要能渲染（人工补一发）。
	if out := RenderPackage(Export{Draft: model.ContentDraft{Id: 1}, BodyMd: "x"}, model.ContentPublication{Channel: "zhihu"}, time.Now()); !strings.Contains(out, "未指定") {
		t.Fatalf("缺计划时间应写「未指定」：\n%s", out)
	}
}

func TestPackagePublisherWritesFile(t *testing.T) {
	dir := t.TempDir()
	pub := model.ContentPublication{Id: 5, DraftID: 2, UserID: "u/../1", Channel: "wechat"}
	e := Export{Draft: model.ContentDraft{Id: 2, Title: "标题"}, Title: "标题", BodyMd: "正文"}
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	path, err := PackagePublisher{Dir: dir, Now: func() time.Time { return now }}.Publish(context.Background(), e, pub)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("应返回绝对路径，实际 %q", path)
	}
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("文件应落在导出目录内，实际 %q（目录 %q）", path, dir)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "正文") || !strings.Contains(string(body), "2026-10-24 22:00") {
		t.Fatalf("文件内容不对：\n%s", body)
	}
}

// ---------- 假存储 ----------

type fakeFailCall struct {
	pub     model.ContentPublication
	cause   string
	retryAt *time.Time
}

type fakeJobStore struct {
	pubs       []model.ContentPublication
	exports    map[uint]Export
	exportErr  map[uint]error
	claimDeny  map[uint]bool
	claimErr   error
	dueErr     error
	marked     map[uint]string
	failCalls  []fakeFailCall
	lastLimit  int
	lastUserID string
}

func newFakeStore(pubs ...model.ContentPublication) *fakeJobStore {
	return &fakeJobStore{
		pubs:      pubs,
		exports:   map[uint]Export{},
		exportErr: map[uint]error{},
		claimDeny: map[uint]bool{},
		marked:    map[uint]string{},
	}
}

func (f *fakeJobStore) DuePublications(userID string, now time.Time, limit int) ([]model.ContentPublication, error) {
	f.lastUserID, f.lastLimit = userID, limit
	if f.dueErr != nil {
		return nil, f.dueErr
	}
	out := make([]model.ContentPublication, 0, len(f.pubs))
	for _, p := range f.pubs {
		if userID != "" && p.UserID != userID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeJobStore) CountDuePublications(userID string, now time.Time) (int64, error) {
	return int64(len(f.pubs)), nil
}

func (f *fakeJobStore) ClaimPublication(id uint, now, leaseUntil time.Time) (model.ContentPublication, bool, error) {
	if f.claimErr != nil {
		return model.ContentPublication{}, false, f.claimErr
	}
	if f.claimDeny[id] {
		return model.ContentPublication{}, false, nil
	}
	for _, p := range f.pubs {
		if p.Id == id {
			p.NextAttemptAt = &leaseUntil
			return p, true, nil
		}
	}
	return model.ContentPublication{}, false, nil
}

func (f *fakeJobStore) ExportDraft(userID string, draftID uint, channel string) (Export, error) {
	if err := f.exportErr[draftID]; err != nil {
		return Export{}, err
	}
	if e, ok := f.exports[draftID]; ok {
		return e, nil
	}
	return Export{Draft: model.ContentDraft{Id: draftID, Title: "标题"}, Title: "标题", BodyMd: "正文"}, nil
}

func (f *fakeJobStore) MarkExported(p model.ContentPublication, path string) (model.ContentPublication, error) {
	f.marked[p.Id] = path
	p.Status = PubExported
	return p, nil
}

func (f *fakeJobStore) RecordExportFailure(p model.ContentPublication, cause string, retryAt *time.Time, maxAttempts int) (model.ContentPublication, error) {
	p.Retries++
	if retryAt == nil {
		p.Status = PubFailed
	}
	f.failCalls = append(f.failCalls, fakeFailCall{pub: p, cause: cause, retryAt: retryAt})
	for i := range f.pubs {
		if f.pubs[i].Id == p.Id {
			f.pubs[i] = p
		}
	}
	return p, nil
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func duePub(id uint, userID string) model.ContentPublication {
	planned := time.Date(2026, 10, 24, 20, 0, 0, 0, time.Local)
	return model.ContentPublication{
		Id: id, UserID: userID, DraftID: id * 10, Channel: "wechat",
		Status: PubPlanned, PlannedAt: &planned,
	}
}

// ---------- 整轮行为 ----------

func TestRunOnceExportsAndAdvances(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	store := newFakeStore(duePub(1, "u1"))
	dir := t.TempDir()
	r := NewRunner(store, PackagePublisher{Dir: dir, Now: fixedClock(now)})
	r.Now = fixedClock(now)

	rep := r.RunOnce(context.Background(), "u1")
	if rep.Busy || rep.Due != 1 || rep.Exported != 1 || rep.Failed != 0 {
		t.Fatalf("报告不对：%+v", rep)
	}
	item := rep.Items[0]
	if item.Status != JobExported || item.Path == "" || !strings.HasPrefix(item.Path, dir) {
		t.Fatalf("单条结果不对：%+v", item)
	}
	if got := store.marked[1]; got != item.Path {
		t.Fatalf("台账应记下成品包路径，实际 %q", got)
	}
	if store.lastUserID != "u1" {
		t.Fatalf("HTTP 触发应只扫当前用户，实际扫了 %q", store.lastUserID)
	}
	if items := r.Recent("u1", 10); len(items) != 1 || items[0].PublicationID != 1 {
		t.Fatalf("历史应能按用户查到：%+v", items)
	}
	if items := r.Recent("u2", 10); len(items) != 0 {
		t.Fatalf("别的用户不该看到这条历史：%+v", items)
	}
}

func TestRunOnceRetrySucceeds(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	p := duePub(2, "u1")
	p.Retries = 2 // 之前失败过两次，这次成功
	store := newFakeStore(p)
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir(), Now: fixedClock(now)})
	r.Now = fixedClock(now)

	rep := r.RunOnce(context.Background(), "u1")
	if rep.Exported != 1 {
		t.Fatalf("重试成功应算导出：%+v", rep)
	}
	if len(store.failCalls) != 0 {
		t.Fatalf("成功不该再记失败：%+v", store.failCalls)
	}
}

func TestRunOnceFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	store := newFakeStore(duePub(3, "u1"))
	store.exportErr[30] = errors.New("稿件不见了")
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir(), Now: fixedClock(now)})
	r.Now = fixedClock(now)
	r.BackoffBase = 2 * time.Minute
	r.MaxAttempts = 3

	rep := r.RunOnce(context.Background(), "u1")
	if rep.Failed != 1 || rep.Exported != 0 {
		t.Fatalf("失败计数不对：%+v", rep)
	}
	if len(store.failCalls) != 1 {
		t.Fatalf("应记一次失败，实际 %+v", store.failCalls)
	}
	call := store.failCalls[0]
	if call.retryAt == nil {
		t.Fatal("第 1 次失败应安排重试")
	}
	if want := now.Add(2 * time.Minute); !call.retryAt.Equal(want) {
		t.Fatalf("退避时间 = %v，期望 %v", call.retryAt, want)
	}
	if !strings.Contains(call.cause, "稿件不见了") {
		t.Fatalf("失败原因应写进台账：%q", call.cause)
	}
}

func TestRunOnceFailureExhausted(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	p := duePub(4, "u1")
	p.Retries = 2 // 这是第 3 次，达到上限
	store := newFakeStore(p)
	store.exportErr[40] = errors.New("磁盘满了")
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir(), Now: fixedClock(now)})
	r.Now = fixedClock(now)
	r.MaxAttempts = 3

	rep := r.RunOnce(context.Background(), "u1")
	if rep.Failed != 1 {
		t.Fatalf("应记一次失败：%+v", rep)
	}
	if got := store.failCalls[0].retryAt; got != nil {
		t.Fatalf("次数用尽不该再排重试，实际 %v", got)
	}
	if store.failCalls[0].pub.Retries != 3 {
		t.Fatalf("失败次数应累计到 3，实际 %d", store.failCalls[0].pub.Retries)
	}
}

func TestRunOnceSkipsClaimedByOther(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	store := newFakeStore(duePub(5, "u1"), duePub(6, "u1"))
	store.claimDeny[5] = true
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir(), Now: fixedClock(now)})
	r.Now = fixedClock(now)

	rep := r.RunOnce(context.Background(), "")
	if rep.Skipped != 1 || rep.Exported != 1 {
		t.Fatalf("被抢的单应跳过：%+v", rep)
	}
	if store.marked[5] != "" {
		t.Fatal("没领到的排期不该被导出")
	}
}

func TestRunOnceBusy(t *testing.T) {
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.Local)
	store := newFakeStore(duePub(7, "u1"))
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir(), Now: fixedClock(now)})
	r.Now = fixedClock(now)
	r.runMu.Lock() // 模拟上一轮还在跑
	rep := r.RunOnce(context.Background(), "u1")
	r.runMu.Unlock()
	if !rep.Busy {
		t.Fatalf("并发触发应返回 Busy：%+v", rep)
	}
	if len(store.marked) != 0 {
		t.Fatal("Busy 时不该处理任何排期")
	}
}

func TestRunOnceScanError(t *testing.T) {
	store := newFakeStore()
	store.dueErr = errors.New("db down")
	r := NewRunner(store, PackagePublisher{Dir: t.TempDir()})
	rep := r.RunOnce(context.Background(), "u1")
	if rep.Error == "" || !strings.Contains(rep.Error, "db down") {
		t.Fatalf("扫描失败应在报告里说清楚：%+v", rep)
	}
	if s := r.Status(); s.LastError == "" {
		t.Fatal("扫描失败应记进 Status")
	}
}

func TestRunOnceWithoutStore(t *testing.T) {
	r := NewRunner(nil, nil)
	rep := r.RunOnce(context.Background(), "u1")
	if rep.Error == "" {
		t.Fatal("没配置数据层时应返回明确错误")
	}
}
