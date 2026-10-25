package content

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"blog-my/internal/model"
)

// 本文件是「排期到点自动导出」的执行器（自动任务的核心）。
//
// 一次 RunOnce 做四件事：
//  1. 扫出到期的排期（planned + planned_at <= now + 不在退避/租约里）；
//  2. 逐条用条件更新「领取」（next_attempt_at = now+lease），领不到就跳过；
//  3. 调 Publisher 生成成品包；
//  4. 成功推进到 exported，失败记原因并按次数退避，次数用尽落到 failed。
//
// 它是进程内单例（web 层持有），但并发安全：TryLock 保证同一时刻只有一轮在跑，
// 手动触发撞上后台调度会立刻返回 Busy，而不是排队等。

// 单条排期的处理结果。
const (
	JobExported = "exported"
	JobFailed   = "failed"
	JobSkipped  = "skipped"
)

// JobResult 是一条排期的处理结果（也是 /api/content/jobs 的 recent 项）。
type JobResult struct {
	UserID        string    `json:"-"`
	PublicationID uint      `json:"publication_id"`
	DraftID       uint      `json:"draft_id"`
	Channel       string    `json:"channel"`
	Status        string    `json:"status"` // exported / failed / skipped
	Path          string    `json:"path,omitempty"`
	Error         string    `json:"error,omitempty"`
	Attempts      int       `json:"attempts"`
	At            time.Time `json:"at"`
	DurationMS    int64     `json:"duration_ms"`
}

// Report 是一轮扫描的汇总。
type Report struct {
	UserID     string      `json:"-"` // 空 = 全库（后台调度）
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at"`
	Due        int         `json:"due"`      // 本轮扫到多少条到期
	Exported   int         `json:"exported"` // 成功导出
	Failed     int         `json:"failed"`   // 失败（含已用尽次数落到 failed）
	Skipped    int         `json:"skipped"`  // 被别的 worker 领走 / 不再到期
	Busy       bool        `json:"busy"`     // 上一轮还没跑完
	Error      string      `json:"error,omitempty"`
	Items      []JobResult `json:"items"`
}

// JobStore 是执行器需要的最小数据面；*Store 实现了它，测试用内存假实现。
//
// 刻意不直接依赖 *gorm.DB：这样「退避、次数用尽、跳过、租约」这些容易写错的
// 分支可以在没有 MySQL 的机器上跑测试。
type JobStore interface {
	DuePublications(userID string, now time.Time, limit int) ([]model.ContentPublication, error)
	CountDuePublications(userID string, now time.Time) (int64, error)
	ClaimPublication(id uint, now, leaseUntil time.Time) (model.ContentPublication, bool, error)
	ExportDraft(userID string, draftID uint, channel string) (Export, error)
	MarkExported(p model.ContentPublication, path string) (model.ContentPublication, error)
	RecordExportFailure(p model.ContentPublication, cause string, retryAt *time.Time, maxAttempts int) (model.ContentPublication, error)
}

// Runner 是自动任务执行器。零值不可用，请用 NewRunner。
type Runner struct {
	Store     JobStore
	Publisher Publisher

	// Now 注入时钟（测试用）；为空用 time.Now。
	Now func() time.Time

	Batch       int           // 每轮最多处理多少条
	MaxAttempts int           // 最多尝试几次后落到 failed；<=0 表示不限
	Lease       time.Duration // 领取租约，必须大于单条导出耗时
	BackoffBase time.Duration // 第一次失败等多久
	BackoffMax  time.Duration // 退避上限

	runMu   sync.Mutex // 串行化跑批
	histMu  sync.Mutex
	hist    []JobResult
	last    time.Time
	lastErr string
}

// 历史最多保留多少条（内存态，重启即清空；台账本身是持久化的）。
const jobHistoryLimit = 200

// NewRunner 组装一个执行器，参数按「桌面端单人使用」的常规量级给默认值。
func NewRunner(store JobStore, publisher Publisher) *Runner {
	return &Runner{
		Store:       store,
		Publisher:   publisher,
		Batch:       20,
		MaxAttempts: 3,
		Lease:       10 * time.Minute,
		BackoffBase: time.Minute,
		BackoffMax:  time.Hour,
	}
}

func (r *Runner) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RunOnce 跑一轮。userID 为空表示后台调度（全库），非空表示只处理该用户
// （HTTP 手动触发必须传，避免替别人干活）。
func (r *Runner) RunOnce(ctx context.Context, userID string) Report {
	now := r.now()
	rep := Report{UserID: userID, StartedAt: now, Items: []JobResult{}}
	if r == nil || r.Store == nil {
		rep.Error = "自动任务未配置（数据层不可用）"
		rep.FinishedAt = now
		return rep
	}
	if !r.runMu.TryLock() {
		rep.Busy = true
		rep.Error = "上一轮自动任务还没跑完"
		rep.FinishedAt = now
		return rep
	}
	defer r.runMu.Unlock()
	// 不管是扫描失败还是跑到一半被取消，都把这一轮的时间和错误记进去，
	// 否则状态接口会一直显示上一轮的成功记录。
	defer func() { r.markRun(rep) }()

	due, err := r.Store.DuePublications(userID, now, r.batch())
	if err != nil {
		rep.Error = "扫描到期排期失败：" + err.Error()
		rep.FinishedAt = r.now()
		return rep
	}
	rep.Due = len(due)
	for _, p := range due {
		if err := ctx.Err(); err != nil {
			rep.Error = "自动任务已取消：" + err.Error()
			break
		}
		res := r.process(ctx, p, now)
		rep.Items = append(rep.Items, res)
		switch res.Status {
		case JobExported:
			rep.Exported++
		case JobFailed:
			rep.Failed++
		default:
			rep.Skipped++
		}
		r.remember(res)
	}
	rep.FinishedAt = r.now()
	return rep
}

// process 处理单条排期：领取 → 导出 → 记账。
func (r *Runner) process(ctx context.Context, p model.ContentPublication, scanAt time.Time) JobResult {
	res := JobResult{
		UserID:        p.UserID,
		PublicationID: p.Id,
		DraftID:       p.DraftID,
		Channel:       p.Channel,
		At:            r.now(),
		Attempts:      p.Retries + 1,
	}
	claimed, ok, err := r.Store.ClaimPublication(p.Id, scanAt, scanAt.Add(r.lease()))
	if err != nil {
		res.Status = JobFailed
		res.Error = "领取排期失败：" + err.Error()
		return res
	}
	if !ok {
		res.Status = JobSkipped
		res.Error = "已被其它任务领取，或状态已变更"
		return res
	}

	started := r.now()
	e, err := r.Store.ExportDraft(claimed.UserID, claimed.DraftID, claimed.Channel)
	if err != nil {
		return r.fail(claimed, "导出稿件失败："+err.Error(), started)
	}
	path, err := r.Publisher.Publish(ctx, e, claimed)
	if err != nil {
		return r.fail(claimed, "生成成品包失败："+err.Error(), started)
	}
	if _, err := r.Store.MarkExported(claimed, path); err != nil {
		// 成品包已经落盘但台账没推进：当成失败重试，下一轮会覆盖同一个文件。
		return r.fail(claimed, "成品包已生成但台账更新失败："+err.Error(), started)
	}
	res.Status = JobExported
	res.Path = path
	res.DurationMS = r.now().Sub(started).Milliseconds()
	log.Printf("[content-job] 已导出 pub=%d user=%s channel=%s -> %s", claimed.Id, claimed.UserID, claimed.Channel, path)
	return res
}

// fail 记一次失败并决定要不要重试。
func (r *Runner) fail(p model.ContentPublication, cause string, started time.Time) JobResult {
	res := JobResult{
		UserID:        p.UserID,
		PublicationID: p.Id,
		DraftID:       p.DraftID,
		Channel:       p.Channel,
		Status:        JobFailed,
		Error:         cause,
		At:            started,
		Attempts:      p.Retries + 1,
		DurationMS:    r.now().Sub(started).Milliseconds(),
	}
	attempts := p.Retries + 1
	var retryAt *time.Time
	if r.maxAttempts() <= 0 || attempts < r.maxAttempts() {
		t := r.now().Add(BackoffFor(attempts, r.backoffBase(), r.backoffMax()))
		retryAt = &t
	}
	if _, err := r.Store.RecordExportFailure(p, cause, retryAt, r.maxAttempts()); err != nil {
		res.Error = cause + "；原本要记到台账时又失败：" + err.Error()
	}
	log.Printf("[content-job] 导出失败 pub=%d user=%s 第 %d 次：%s（下次 %v）",
		p.Id, p.UserID, attempts, cause, retryAt)
	return res
}

// Status 返回执行器的概览（供 /api/content/jobs 展示）。
type Status struct {
	LastRunAt   time.Time `json:"last_run_at"`
	LastError   string    `json:"last_error,omitempty"`
	HistorySize int       `json:"history_size"`
}

// Status 读最近一轮的时间与错误（内存态）。
func (r *Runner) Status() Status {
	if r == nil {
		return Status{}
	}
	r.histMu.Lock()
	defer r.histMu.Unlock()
	return Status{LastRunAt: r.last, LastError: r.lastErr, HistorySize: len(r.hist)}
}

// CountDue 数某用户当前还有多少条等着导出。
func (r *Runner) CountDue(userID string) (int64, error) {
	if r == nil || r.Store == nil {
		return 0, errors.New("自动任务未配置")
	}
	return r.Store.CountDuePublications(userID, r.now())
}

// Recent 取最近的处理记录；userID 为空表示不过滤（后台调度视角）。
// 只返回给 HTTP 层时务必带 userID，避免把别人的稿件标题/路径透出去。
func (r *Runner) Recent(userID string, limit int) []JobResult {
	out := make([]JobResult, 0, 8)
	if r == nil {
		return out
	}
	if limit <= 0 || limit > jobHistoryLimit {
		limit = 20
	}
	r.histMu.Lock()
	defer r.histMu.Unlock()
	for i := len(r.hist) - 1; i >= 0 && len(out) < limit; i-- {
		item := r.hist[i]
		if strings.TrimSpace(userID) != "" && item.UserID != userID {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (r *Runner) remember(res JobResult) {
	r.histMu.Lock()
	defer r.histMu.Unlock()
	r.hist = append(r.hist, res)
	if len(r.hist) > jobHistoryLimit {
		r.hist = append([]JobResult(nil), r.hist[len(r.hist)-jobHistoryLimit:]...)
	}
}

func (r *Runner) markRun(rep Report) {
	r.histMu.Lock()
	defer r.histMu.Unlock()
	r.last = rep.FinishedAt
	r.lastErr = rep.Error
}

func (r *Runner) batch() int {
	if r.Batch <= 0 {
		return 20
	}
	if r.Batch > 100 {
		return 100
	}
	return r.Batch
}

func (r *Runner) lease() time.Duration {
	if r.Lease <= 0 {
		return 10 * time.Minute
	}
	return r.Lease
}

func (r *Runner) maxAttempts() int {
	if r.MaxAttempts < 0 {
		return 0
	}
	return r.MaxAttempts
}

func (r *Runner) backoffBase() time.Duration {
	if r.BackoffBase <= 0 {
		return time.Minute
	}
	return r.BackoffBase
}

func (r *Runner) backoffMax() time.Duration {
	if r.BackoffMax <= 0 {
		return time.Hour
	}
	return r.BackoffMax
}
