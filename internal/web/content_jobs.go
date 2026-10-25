package web

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/content"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 本文件把 content 包的「排期到点自动导出」接成常驻任务：
//
//	GET  /api/content/jobs       -> contentJobsStatus   （状态 + 当前用户待导出数 + 最近记录）
//	POST /api/content/jobs/run   -> contentJobsRun      （立刻跑一轮，只跑当前用户）
//
// 调度节拍、成品包目录、批量与重试上限都来自环境变量（见 .env.example）。
// 一次跑批做三件事：领取到期排期 → 生成成品包落盘 → 台账推进到 exported。
// 项目明确不做真实平台发帖，所以这里不会替用户「点发布」，也不会写 published_at。
//
// 设计取舍：
//   - 后台调度扫全库（多用户共用一份库时各自到点都能导出），但它不向任何人回传数据；
//   - HTTP 侧强制按 currentUserID 过滤，手动触发也只处理自己的排期；
//   - 运行记录只在内存里（最近 200 条），台账本身在数据库里，重启不丢「发没发」这个事实。

const (
	contentJobDefaultIntervalMinutes = 5
	contentJobsAPILimit              = 20
)

// ---------------------------------------------------------------------------
// 环境变量
// ---------------------------------------------------------------------------

// contentJobIntervalMinutes 读 CONTENT_PUBLISH_INTERVAL_MINUTES，默认 5 分钟。
// <=0 表示关掉后台调度（手动 /api/content/jobs/run 仍然可用）。
func contentJobIntervalMinutes() int {
	raw := strings.TrimSpace(os.Getenv("CONTENT_PUBLISH_INTERVAL_MINUTES"))
	if raw == "" {
		return contentJobDefaultIntervalMinutes
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return contentJobDefaultIntervalMinutes
	}
	return n
}

// contentExportDir 读 CONTENT_EXPORT_DIR；默认放在用户配置目录下的 blog-my/content-publish。
//
// 不用仓库里的相对路径当默认：安装版的后端工作目录可能在 Program Files 下，
// 那里不可写，自动任务会一路失败到 failed。配置目录（与 evolve/工具长结果落盘一致）
// 在开发版和安装版都可写，个别环境拿不到时退到临时目录保证功能可用。
func contentExportDir() string {
	if dir := strings.TrimSpace(os.Getenv("CONTENT_EXPORT_DIR")); dir != "" {
		return dir
	}
	if d, err := os.UserConfigDir(); err == nil && strings.TrimSpace(d) != "" {
		return filepath.Join(d, "blog-my", "content-publish")
	}
	return filepath.Join(os.TempDir(), "blog-my", "content-publish")
}

func contentJobIntEnv(key string, fallback int) int {
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
// 执行器单例
// ---------------------------------------------------------------------------

var (
	contentRunnerOnce sync.Once
	contentRunnerInst *content.Runner
)

// contentJobRunner 懒加载执行器：数据库没初始化时返回 nil，接口如实报「不可用」，
// 而不是启动失败（与 kb()/proactive 的取舍一致：可选能力不该拦住登录）。
func contentJobRunner() *content.Runner {
	contentRunnerOnce.Do(func() {
		if model.DB == nil {
			return
		}
		r := content.NewRunner(content.NewStore(model.DB), content.PackagePublisher{Dir: contentExportDir()})
		r.Batch = contentJobIntEnv("CONTENT_PUBLISH_BATCH", 20)
		r.MaxAttempts = contentJobIntEnv("CONTENT_PUBLISH_MAX_ATTEMPTS", 3)
		contentRunnerInst = r
	})
	return contentRunnerInst
}

// ---------------------------------------------------------------------------
// 后台调度
// ---------------------------------------------------------------------------

var (
	contentSchedMu      sync.Mutex
	contentSchedCancel  context.CancelFunc
	contentSchedStarted bool
)

// StartContentPublishScheduler 启动排期自动导出；幂等。
// 间隔来自 CONTENT_PUBLISH_INTERVAL_MINUTES（默认 5 分钟，<=0 关闭）。
func StartContentPublishScheduler(ctx context.Context) {
	mins := contentJobIntervalMinutes()
	if mins <= 0 {
		log.Printf("[content-job] CONTENT_PUBLISH_INTERVAL_MINUTES=%d，排期自动导出已关闭（可手动触发）", mins)
		return
	}
	r := contentJobRunner()
	if r == nil {
		log.Printf("[content-job] 数据库未初始化，排期自动导出不启动")
		return
	}
	contentSchedMu.Lock()
	defer contentSchedMu.Unlock()
	if contentSchedCancel != nil {
		return // 已经在跑
	}
	runCtx, cancel := context.WithCancel(ctx)
	contentSchedCancel = cancel
	contentSchedStarted = true
	go contentJobLoop(runCtx, r, time.Duration(mins)*time.Minute)
}

// StopContentPublishScheduler 停掉后台调度（测试 / 优雅退出用）。
func StopContentPublishScheduler() {
	contentSchedMu.Lock()
	cancel := contentSchedCancel
	contentSchedCancel = nil
	contentSchedStarted = false
	contentSchedMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func contentSchedulerRunning() bool {
	contentSchedMu.Lock()
	defer contentSchedMu.Unlock()
	return contentSchedStarted
}

func contentJobLoop(ctx context.Context, r *content.Runner, interval time.Duration) {
	log.Printf("[content-job] 调度已启动，每 %s 检查一次，成品包目录 %s", interval, contentExportDir())
	// 启动后先等几秒再跑第一轮：桌面壳刚起来时优先让登录/首屏就绪，
	// 而且历史排期晚几秒导出没有任何影响。
	first := time.NewTimer(3 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
		runContentJobRound(ctx, r)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[content-job] 调度已停止")
			return
		case <-ticker.C:
			runContentJobRound(ctx, r)
		}
	}
}

func runContentJobRound(ctx context.Context, r *content.Runner) {
	rep := r.RunOnce(ctx, "")
	if len(rep.Items) == 0 && rep.Error == "" {
		return
	}
	log.Printf("[content-job] 本轮到期 %d 条，导出 %d，失败 %d，跳过 %d%s",
		rep.Due, rep.Exported, rep.Failed, rep.Skipped, contentJobErrSuffix(rep.Error))
}

func contentJobErrSuffix(errText string) string {
	if strings.TrimSpace(errText) == "" {
		return ""
	}
	return "，错误：" + errText
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// contentJobsStatus 返回调度状态。永远 200：界面据此决定显示「运行中 / 已关闭 / 不可用」。
func contentJobsStatus(c *gin.Context) {
	userID := currentUserID(c)
	interval := contentJobIntervalMinutes()
	payload := gin.H{
		"mode":             "export", // 只导出成品包，不代替用户发平台
		"enabled":          contentSchedulerRunning(),
		"configured":       false,
		"interval_seconds": interval * 60,
		"export_dir":       contentExportDir(),
		"due_count":        0,
		"recent":           []content.JobResult{},
	}
	r := contentJobRunner()
	if r == nil {
		payload["reason"] = "数据库未初始化"
		c.JSON(http.StatusOK, payload)
		return
	}
	payload["configured"] = true
	if due, err := r.CountDue(userID); err == nil {
		payload["due_count"] = due
	} else {
		payload["due_error"] = err.Error()
	}
	st := r.Status()
	if !st.LastRunAt.IsZero() {
		payload["last_run_at"] = st.LastRunAt
	}
	if st.LastError != "" {
		payload["last_error"] = st.LastError
	}
	payload["recent"] = r.Recent(userID, contentJobsAPILimit)
	c.JSON(http.StatusOK, payload)
}

// contentJobsRun 立刻跑一轮，只处理当前用户的到期排期。
//
// 上一轮还没跑完时返回 409 + busy=true（不排队）：手动触发是「现在就要结果」，
// 排队等一轮反而让人以为卡住了。
func contentJobsRun(c *gin.Context) {
	r := contentJobRunner()
	if r == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "数据库未初始化，自动任务不可用"})
		return
	}
	rep := r.RunOnce(c.Request.Context(), currentUserID(c))
	if rep.Busy {
		c.JSON(http.StatusConflict, rep)
		return
	}
	c.JSON(http.StatusOK, rep)
}
