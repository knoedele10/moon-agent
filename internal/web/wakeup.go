package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 定时唤醒：到点让 agent 在指定会话里自己跑一轮。
//
// 与主动聊天（proactive）的区别：
//   - proactive 是「全局策略决定现在要不要找用户」，用户没有指定时间；
//   - 唤醒是「某会话、某个时间点、做某件事」，一次性、可取消、有台账。
//
// 执行复用 web/agent.go 的后台轮次（headless=true）：不写自动记忆、不刷画像，
// assistant 的回答照常落库，用户打开会话就能看到。子 agent 唤醒父级走的也是这条路
// （wakeParentAfterSubagents），所以工具集、压缩、落库行为天然一致。
//
// 失败与并发：
//   - next_attempt_at 一列两用：失败退避 + 租约。进程崩了，租约到期会被重新捡起；
//   - 会话正忙不重试计数，只推后 2 分钟（用户正在对话时不该抢）；
//   - 同一会话沿用 wakeAllowed 的时间闸（30 秒一次、一小时 6 次），
//     防止「唤醒 → agent 又定下一次唤醒」形成快环。

const (
	wakeupMinLead        = 15 * time.Second
	wakeupMaxHorizon     = 365 * 24 * time.Hour
	wakeupMaxPromptRunes = 4000
	wakeupMaxTitleRunes  = 120
	// 每个会话最多挂 5 条待唤醒：再多就不是「提醒」而是刷屏了。
	wakeupMaxPendingPerConversation = 5

	wakeupDefaultIntervalSeconds = 30
	wakeupDefaultBatch           = 10
	wakeupDefaultMaxAttempts     = 3
	wakeupDefaultLease           = 15 * time.Minute
	wakeupBusyDefer              = 2 * time.Minute
	wakeupRateDefer              = 5 * time.Minute
)

var (
	// errWakeupBusy 表示会话正有轮次在跑：调用方只推后，不计失败次数。
	errWakeupBusy = errors.New("会话正忙")
	// errWakeupRateLimited 表示撞上了同会话的后台轮次时间闸。
	errWakeupRateLimited = errors.New("唤醒过于频繁")
	// errWakeupNotFound 表示「对当前用户不可见」。
	errWakeupNotFound = errors.New("唤醒不存在")
)

// ---------------------------------------------------------------------------
// 环境配置
// ---------------------------------------------------------------------------

// wakeupIntervalSeconds 读 AGENT_WAKEUP_INTERVAL_SECONDS，默认 30 秒；<=0 关闭调度。
func wakeupIntervalSeconds() int {
	raw := strings.TrimSpace(os.Getenv("AGENT_WAKEUP_INTERVAL_SECONDS"))
	if raw == "" {
		return wakeupDefaultIntervalSeconds
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return wakeupDefaultIntervalSeconds
	}
	return n
}

func wakeupMaxAttempts() int {
	n := wakeupIntEnv("AGENT_WAKEUP_MAX_ATTEMPTS", wakeupDefaultMaxAttempts)
	if n < 0 {
		return 0
	}
	return n
}

func wakeupBatch() int {
	n := wakeupIntEnv("AGENT_WAKEUP_BATCH", wakeupDefaultBatch)
	if n <= 0 {
		return wakeupDefaultBatch
	}
	if n > 50 {
		return 50
	}
	return n
}

func wakeupIntEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// ---------------------------------------------------------------------------
// 校验与文案（纯函数，测试直接跑）
// ---------------------------------------------------------------------------

// parseWakeupRunAt 解析唤醒时间。接受 RFC3339（推荐）与几种本地时间写法。
//
// 要求至少领先 15 秒、最多一年：过去的时间没有意义，太远的时间基本是模型算错了。
func parseWakeupRunAt(raw string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, errors.New("run_at 不能为空（RFC3339，例如 2026-10-25T09:00:00+08:00）")
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	var t time.Time
	var err error
	for _, layout := range layouts {
		if strings.Contains(layout, "Z07") || strings.Contains(layout, "-07") {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, time.Local)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("时间格式不对：%q（用 RFC3339，例如 2026-10-25T09:00:00+08:00）", raw)
	}
	// 只有日期时按当天 00:00 处理，多半已经过去；提示里给出实际解析结果，模型好纠正。
	if !t.After(now.Add(wakeupMinLead)) {
		return time.Time{}, fmt.Errorf("时间已经过去（解析为 %s，现在是 %s）",
			t.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04"))
	}
	if t.After(now.Add(wakeupMaxHorizon)) {
		return time.Time{}, errors.New("时间太远：最多提前一年")
	}
	return t, nil
}

// cleanWakeupText 归一说明与标题，按字符截断（不会把汉字切坏）。
func cleanWakeupText(prompt, title string) (string, string, error) {
	prompt = strings.TrimSpace(prompt)
	title = strings.TrimSpace(title)
	if prompt == "" {
		return "", "", errors.New("prompt 不能为空：到点后 agent 要靠它知道要做什么")
	}
	if utf8.RuneCountInString(prompt) > wakeupMaxPromptRunes {
		return "", "", fmt.Errorf("prompt 太长（上限 %d 字）", wakeupMaxPromptRunes)
	}
	if title == "" {
		title = firstRunes(prompt, 40)
	}
	if utf8.RuneCountInString(title) > wakeupMaxTitleRunes {
		title = firstRunes(title, wakeupMaxTitleRunes)
	}
	return prompt, title, nil
}

func firstRunes(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if n <= 0 {
		return ""
	}
	if len(rs) <= n {
		return string(rs)
	}
	if n == 1 {
		return "…"
	}
	// 截断标记算在长度里：调用方（标题列、error 列）都按上限校验。
	return string(rs[:n-1]) + "…"
}

// wakeupPrompt 是到点交给 agent 的那句话。
//
// 刻意写清「这是你自己定的」和「不需要做就直说」：否则模型容易把定时唤醒
// 当成用户刚发的消息，硬编一段并不存在的进展。
func wakeupPrompt(w model.AgentWakeup, now time.Time) string {
	var b strings.Builder
	b.WriteString("（系统）定时唤醒：你在 ")
	b.WriteString(w.RunAt.Local().Format("2006-01-02 15:04"))
	b.WriteString(" 给自己安排了一次唤醒，现在时间 ")
	b.WriteString(now.Local().Format("2006-01-02 15:04"))
	b.WriteString("。\n")
	if strings.TrimSpace(w.Title) != "" {
		b.WriteString("唤醒标题：")
		b.WriteString(strings.TrimSpace(w.Title))
		b.WriteString("\n")
	}
	b.WriteString("当时留下的说明：\n")
	b.WriteString(strings.TrimSpace(w.Prompt))
	b.WriteString("\n\n请按说明继续；先看对话历史和当前时间，判断这件事还需不需要做。")
	b.WriteString("已经不需要就一句话说明，不要编造进展；需要用户决定的事就明确问，不要替他做主。")
	return b.String()
}

func wakeupBackoff(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Minute
	}
	if max <= 0 {
		max = time.Hour
	}
	if max < base {
		max = base
	}
	if attempts <= 1 {
		return base
	}
	if attempts > 16 {
		return max
	}
	d := base << (attempts - 1)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// ---------------------------------------------------------------------------
// 数据访问
// ---------------------------------------------------------------------------

// createWakeup 落一条待唤醒。调用方负责校验会话归属与数量上限。
func createWakeup(userID string, convID uint, title, prompt string, runAt time.Time, createdBy string) (model.AgentWakeup, error) {
	var w model.AgentWakeup
	if model.DB == nil {
		return w, errors.New("数据库未初始化")
	}
	if strings.TrimSpace(userID) == "" || convID == 0 {
		return w, errors.New("userID 与会话不能为空")
	}
	w = model.AgentWakeup{
		UserID: userID, ConversationID: convID, Title: title, Prompt: prompt,
		RunAt: runAt, Status: model.WakeupPending, CreatedBy: createdBy,
	}
	if err := model.DB.Create(&w).Error; err != nil {
		return w, err
	}
	return w, nil
}

// dueWakeups 扫到点的待唤醒（全局口径，后台调度专用）。
func dueWakeups(now time.Time, limit int) ([]model.AgentWakeup, error) {
	out := make([]model.AgentWakeup, 0, 8)
	if model.DB == nil {
		return out, errors.New("数据库未初始化")
	}
	if limit <= 0 {
		limit = wakeupDefaultBatch
	}
	q := model.DB.Model(&model.AgentWakeup{}).
		Where("status = ? AND run_at <= ?", model.WakeupPending, now).
		Where("(next_attempt_at IS NULL OR next_attempt_at <= ?)", now)
	return out, q.Order("run_at asc, id asc").Limit(limit).Find(&out).Error
}

// claimWakeup 原子领取：把租约推到 leaseUntil，领不到说明别人先拿了或状态变了。
func claimWakeup(id uint, now, leaseUntil time.Time) (model.AgentWakeup, bool, error) {
	var w model.AgentWakeup
	if model.DB == nil {
		return w, false, errors.New("数据库未初始化")
	}
	if !leaseUntil.After(now) {
		leaseUntil = now.Add(wakeupDefaultLease)
	}
	res := model.DB.Model(&model.AgentWakeup{}).
		Where("id = ? AND status = ? AND run_at <= ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)",
			id, model.WakeupPending, now, now).
		Updates(map[string]any{"next_attempt_at": leaseUntil})
	if res.Error != nil {
		return w, false, res.Error
	}
	if res.RowsAffected == 0 {
		return w, false, nil
	}
	if err := model.DB.Where("id = ?", id).First(&w).Error; err != nil {
		return w, false, err
	}
	return w, true, nil
}

// finishWakeup 标记成功。只推进 pending 的行，避免覆盖用户刚点的取消。
func finishWakeup(id uint, at time.Time) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	return model.DB.Model(&model.AgentWakeup{}).
		Where("id = ? AND status = ?", id, model.WakeupPending).
		Updates(map[string]any{
			"status": model.WakeupDone, "fired_at": at, "error": "", "next_attempt_at": nil,
		}).Error
}

// failWakeup 记一次失败：次数用尽落 failed，否则保持 pending 并排下一次。
func failWakeup(w model.AgentWakeup, cause string, retryAt *time.Time, maxAttempts int) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	cause = firstRunes(strings.TrimSpace(cause), 500)
	updates := map[string]any{"attempts": w.Attempts + 1, "error": cause, "next_attempt_at": retryAt}
	if maxAttempts > 0 && w.Attempts+1 >= maxAttempts {
		updates["status"] = model.WakeupFailed
		updates["next_attempt_at"] = nil
	}
	return model.DB.Model(&model.AgentWakeup{}).
		Where("id = ? AND status = ?", w.Id, model.WakeupPending).
		Updates(updates).Error
}

// deferWakeup 不动失败次数，只把下一次尝试推后（会话忙 / 限流）。
func deferWakeup(id uint, until time.Time) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	return model.DB.Model(&model.AgentWakeup{}).
		Where("id = ? AND status = ?", id, model.WakeupPending).
		Updates(map[string]any{"next_attempt_at": until}).Error
}

// listUserWakeups 列当前用户的唤醒；convID=0 表示全部会话。
func listUserWakeups(userID string, convID uint, status string, limit int) ([]model.AgentWakeup, error) {
	out := make([]model.AgentWakeup, 0, 8)
	if model.DB == nil {
		return out, errors.New("数据库未初始化")
	}
	if strings.TrimSpace(userID) == "" {
		return out, errors.New("userID 不能为空")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := model.DB.Model(&model.AgentWakeup{}).Where("user_id = ?", userID)
	if convID != 0 {
		q = q.Where("conversation_id = ?", convID)
	}
	if s := strings.TrimSpace(status); s != "" {
		q = q.Where("status = ?", s)
	}
	order := "created_at desc, id desc"
	if status == model.WakeupPending {
		order = "run_at asc, id asc"
	}
	return out, q.Order(order).Limit(limit).Find(&out).Error
}

// cancelUserWakeup 取消一条待唤醒；已结束的返回 409 语义的错误。
func cancelUserWakeup(userID string, id uint) (model.AgentWakeup, error) {
	var w model.AgentWakeup
	if model.DB == nil {
		return w, errors.New("数据库未初始化")
	}
	res := model.DB.Model(&model.AgentWakeup{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userID, model.WakeupPending).
		Updates(map[string]any{"status": model.WakeupCanceled, "next_attempt_at": nil})
	if res.Error != nil {
		return w, res.Error
	}
	if res.RowsAffected == 0 {
		err := model.DB.Where("id = ? AND user_id = ?", id, userID).First(&w).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return w, errWakeupNotFound
		}
		if err != nil {
			return w, err
		}
		return w, fmt.Errorf("这条唤醒已经是 %s，不能再取消", w.Status)
	}
	return w, model.DB.Where("id = ? AND user_id = ?", id, userID).First(&w).Error
}

func countPendingWakeups(convID uint) (int64, error) {
	var n int64
	if model.DB == nil {
		return 0, errors.New("数据库未初始化")
	}
	return n, model.DB.Model(&model.AgentWakeup{}).
		Where("conversation_id = ? AND status = ?", convID, model.WakeupPending).Count(&n).Error
}

// ---------------------------------------------------------------------------
// 执行器
// ---------------------------------------------------------------------------

// wakeupSource 是执行器依赖的数据面；生产实现走 model.DB，测试用内存实现。
type wakeupSource interface {
	Due(now time.Time, limit int) ([]model.AgentWakeup, error)
	Claim(id uint, now, leaseUntil time.Time) (model.AgentWakeup, bool, error)
	Done(id uint, at time.Time) error
	Fail(w model.AgentWakeup, cause string, retryAt *time.Time, maxAttempts int) error
	Defer(id uint, until time.Time) error
}

type dbWakeupSource struct{}

func (dbWakeupSource) Due(now time.Time, limit int) ([]model.AgentWakeup, error) {
	return dueWakeups(now, limit)
}

func (dbWakeupSource) Claim(id uint, now, leaseUntil time.Time) (model.AgentWakeup, bool, error) {
	return claimWakeup(id, now, leaseUntil)
}

func (dbWakeupSource) Done(id uint, at time.Time) error { return finishWakeup(id, at) }

func (dbWakeupSource) Fail(w model.AgentWakeup, cause string, retryAt *time.Time, maxAttempts int) error {
	return failWakeup(w, cause, retryAt, maxAttempts)
}

func (dbWakeupSource) Defer(id uint, until time.Time) error { return deferWakeup(id, until) }

// wakeupExecutor 真正跑一次唤醒；测试注入假实现。
type wakeupExecutor func(ctx context.Context, w model.AgentWakeup) error

// wakeupReport 是一轮的汇总（只进日志，不对外暴露）。
type wakeupReport struct {
	Scanned  int
	Fired    int
	Failed   int
	Deferred int
	Err      string
}

type wakeupRunner struct {
	src         wakeupSource
	exec        wakeupExecutor
	now         func() time.Time
	batch       int
	maxAttempts int
	lease       time.Duration
	backoffBase time.Duration
	backoffMax  time.Duration

	runMu sync.Mutex
}

func newWakeupRunner(exec wakeupExecutor) *wakeupRunner {
	return &wakeupRunner{
		src:         dbWakeupSource{},
		exec:        exec,
		now:         time.Now,
		batch:       wakeupBatch(),
		maxAttempts: wakeupMaxAttempts(),
		lease:       wakeupDefaultLease,
		backoffBase: time.Minute,
		backoffMax:  time.Hour,
	}
}

func (r *wakeupRunner) RunOnce(ctx context.Context) wakeupReport {
	var rep wakeupReport
	if r == nil || r.src == nil {
		rep.Err = "执行器未配置"
		return rep
	}
	if !r.runMu.TryLock() {
		return rep // 上一轮还在跑，这一拍跳过
	}
	defer r.runMu.Unlock()

	now := r.now()
	due, err := r.src.Due(now, r.batch)
	if err != nil {
		rep.Err = "扫描唤醒失败：" + err.Error()
		return rep
	}
	rep.Scanned = len(due)
	for _, w := range due {
		if ctx.Err() != nil {
			rep.Err = "已取消：" + ctx.Err().Error()
			break
		}
		claimed, ok, err := r.src.Claim(w.Id, now, now.Add(r.lease))
		if err != nil {
			rep.Err = "领取唤醒失败：" + err.Error()
			continue
		}
		if !ok {
			continue // 别人领走了 / 用户取消了
		}
		err = r.exec(ctx, claimed)
		switch {
		case err == nil:
			if e := r.src.Done(claimed.Id, r.now()); e != nil {
				rep.Err = "标记完成失败：" + e.Error()
			}
			rep.Fired++
			log.Printf("[wakeup] 已唤醒会话 %d（唤醒 %d）：%s", claimed.ConversationID, claimed.Id, claimed.Title)
		case errors.Is(err, errWakeupBusy):
			_ = r.src.Defer(claimed.Id, r.now().Add(wakeupBusyDefer))
			rep.Deferred++
			log.Printf("[wakeup] 会话 %d 正忙，%s 后再试（唤醒 %d）", claimed.ConversationID, wakeupBusyDefer, claimed.Id)
		case errors.Is(err, errWakeupRateLimited):
			_ = r.src.Defer(claimed.Id, r.now().Add(wakeupRateDefer))
			rep.Deferred++
			log.Printf("[wakeup] 会话 %d 触发限流，%s 后再试（唤醒 %d）", claimed.ConversationID, wakeupRateDefer, claimed.Id)
		default:
			attempts := claimed.Attempts + 1
			var retryAt *time.Time
			if r.maxAttempts <= 0 || attempts < r.maxAttempts {
				t := r.now().Add(wakeupBackoff(attempts, r.backoffBase, r.backoffMax))
				retryAt = &t
			}
			if e := r.src.Fail(claimed, err.Error(), retryAt, r.maxAttempts); e != nil {
				rep.Err = "记录失败原因失败：" + e.Error()
			}
			rep.Failed++
			log.Printf("[wakeup] 唤醒 %d 失败（第 %d 次）：%v", claimed.Id, attempts, err)
		}
	}
	return rep
}

// ---------------------------------------------------------------------------
// 真正跑一轮
// ---------------------------------------------------------------------------

// fireWakeup 在目标会话里跑一次后台 agent 轮次。
func fireWakeup(ctx context.Context, w model.AgentWakeup) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	if registry == nil {
		return errors.New("模型注册表未就绪")
	}
	var conv model.Conversation
	err := model.DB.Where("id = ? AND user_id = ?", w.ConversationID, w.UserID).First(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errors.New("会话不存在或不属于该用户")
	}
	if err != nil {
		return err
	}
	if conversationBusy(conv.Id) {
		return errWakeupBusy
	}
	modelID := strings.TrimSpace(conv.Model)
	if modelID == "" {
		return errors.New("会话没有选择模型")
	}
	ep, ok := registry.FindByModel(modelID)
	if !ok {
		return fmt.Errorf("找不到模型 %s 的端点", modelID)
	}
	if !wakeAllowed(conv.Id, time.Now()) {
		return errWakeupRateLimited
	}
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&rows).Error; err != nil {
		return err
	}
	runAgentChat(turnInput{
		ctx:       ctx,
		conv:      &conv,
		userID:    conv.UserID,
		modelID:   modelID,
		key:       resolveKey(conv.UserID, ep),
		content:   wakeupPrompt(w, time.Now()),
		history:   rebuildLLMHistory(rows),
		kind:      normalizeKind(conv.Kind),
		sendEvent: func(string, any) {},
		headless:  true,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 后台调度
// ---------------------------------------------------------------------------

var (
	wakeupRunnerOnce sync.Once
	wakeupRunnerInst *wakeupRunner

	wakeupSchedMu      sync.Mutex
	wakeupSchedCancel  context.CancelFunc
	wakeupSchedStarted bool
)

func wakeupRunnerInstance() *wakeupRunner {
	wakeupRunnerOnce.Do(func() {
		wakeupRunnerInst = newWakeupRunner(fireWakeup)
	})
	return wakeupRunnerInst
}

// StartWakeupScheduler 启动定时唤醒调度；幂等。
// 间隔来自 AGENT_WAKEUP_INTERVAL_SECONDS（默认 30，<=0 关闭；HTTP 建单仍然可用）。
func StartWakeupScheduler(ctx context.Context) {
	secs := wakeupIntervalSeconds()
	if secs <= 0 {
		log.Printf("[wakeup] AGENT_WAKEUP_INTERVAL_SECONDS=%d，定时唤醒调度已关闭（建单仍可用）", secs)
		return
	}
	wakeupSchedMu.Lock()
	defer wakeupSchedMu.Unlock()
	if wakeupSchedCancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	wakeupSchedCancel = cancel
	wakeupSchedStarted = true
	go wakeupLoop(runCtx, wakeupRunnerInstance(), time.Duration(secs)*time.Second)
}

// StopWakeupScheduler 停掉调度（测试 / 优雅退出）。
func StopWakeupScheduler() {
	wakeupSchedMu.Lock()
	cancel := wakeupSchedCancel
	wakeupSchedCancel = nil
	wakeupSchedStarted = false
	wakeupSchedMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func wakeupSchedulerRunning() bool {
	wakeupSchedMu.Lock()
	defer wakeupSchedMu.Unlock()
	return wakeupSchedStarted
}

func wakeupLoop(ctx context.Context, r *wakeupRunner, interval time.Duration) {
	log.Printf("[wakeup] 调度已启动，每 %s 检查一次", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[wakeup] 调度已停止")
			return
		case <-ticker.C:
			rep := r.RunOnce(ctx)
			if rep.Err != "" {
				log.Printf("[wakeup] 本轮异常：%s", rep.Err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type wakeupRequest struct {
	ConversationID uint   `json:"conversation_id"`
	RunAt          string `json:"run_at"`
	Prompt         string `json:"prompt"`
	Title          string `json:"title"`
}

// createWakeupAPI 建一条定时唤醒。
func createWakeupAPI(c *gin.Context) {
	userID := currentUserID(c)
	var req wakeupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
		return
	}
	if req.ConversationID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id 不能为空"})
		return
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", req.ConversationID, userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	now := time.Now()
	runAt, err := parseWakeupRunAt(req.RunAt, now)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	prompt, title, err := cleanWakeupText(req.Prompt, req.Title)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if n, err := countPendingWakeups(conv.Id); err == nil && n >= wakeupMaxPendingPerConversation {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("这个会话已经挂了 %d 条待唤醒，先取消几条再加", n)})
		return
	}
	w, err := createWakeup(userID, conv.Id, title, prompt, runAt, "user")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存唤醒失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, w)
}

// listWakeupsAPI 列当前用户的唤醒。?conversation_id=&status=&limit=
func listWakeupsAPI(c *gin.Context) {
	userID := currentUserID(c)
	convID := uint(0)
	if raw := strings.TrimSpace(c.Query("conversation_id")); raw != "" {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
			convID = uint(v)
		}
	}
	limit := 20
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = v
		}
	}
	rows, err := listUserWakeups(userID, convID, c.Query("status"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取唤醒失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"running": wakeupSchedulerRunning(),
		"items":   rows,
	})
}

// cancelWakeupAPI 取消一条待唤醒。
func cancelWakeupAPI(c *gin.Context) {
	userID := currentUserID(c)
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 不合法"})
		return
	}
	w, err := cancelUserWakeup(userID, uint(id64))
	switch {
	case errors.Is(err, errWakeupNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "唤醒不存在"})
	case err != nil:
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, w)
	}
}
