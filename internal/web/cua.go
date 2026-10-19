package web

// CUA 电脑操控的 Go 侧网关（契约 docs/cua/06-integration-contract.md 第 2 节）。
//
// 分工：Go 只做「进程管家 + 事件转发」，不重写 CUA（契约 0.3）。
//   - 每个 run 起一个 CUA 子进程（优先 bin/cua-sidecar.exe，回落 python -m cua.main）；
//   - 逐行读子进程 stdout 的 JSONL，原样广播给所有 SSE 订阅者，并保留历史供刷新重连补发；
//   - stop 杀进程树；同一时刻只允许一个 run。
//
// 测试/运维用的环境变量（都不改变对外 HTTP 形状）：
//   CUA_SIDECAR     显式指定 sidecar 可执行文件路径
//   CUA_PYTHON      显式指定 python 可执行文件（默认 python）
//   CUA_EXTRA_ARGS  追加给子进程的额外参数（空格分隔），例如 CUA_EXTRA_ARGS=--mock 可离线安全跑通

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"blog-my/internal/llm"

	"github.com/gin-gonic/gin"
)

const (
	cuaTaskMaxLen        = 2000 // 契约 2.2：task 长度上限
	cuaMaxStepsDefault   = 20
	cuaMaxStepsMin       = 1
	cuaMaxStepsMax       = 200
	cuaMaxSecondsDefault = 600
	cuaMaxSecondsMin     = 10
	cuaMaxSecondsMax     = 3600
	cuaHistoryLimit      = 50  // 契约 2.5：内存保留最近 50 条
	cuaSubBuffer         = 512 // 单个 SSE 订阅者的缓冲事件数
	cuaStderrKeepLines   = 40  // 诊断用的 stderr 尾行数
	cuaProbeTTL          = 30 * time.Second
)

// ---------------------------------------------------------------------------
// 事件与 run
// ---------------------------------------------------------------------------

// cuaEvent 是要推给 SSE 的一条事件：Name 直接取 CUA 的 kind（契约 2.3），
// Data 是原样透传的 CUA JSON 行。
type cuaEvent struct {
	Name string
	Data []byte
}

// cuaRun 是一次 CUA 运行。进程与 SSE 是两条独立生命周期：
// 客户端断开只影响订阅，绝不影响子进程（契约 2.3）。
type cuaRun struct {
	ID     string
	UserID string
	Task   string
	Model  string
	DryRun bool
	Token  string // 每 run 一次性 Bearer token（契约 3）

	mu            sync.Mutex
	status        string // running | done | failed | stopped
	code          string // CUA 的终止码 E_xxx，正常结束为空
	steps         int
	exitCode      int
	stopRequested bool
	finished      bool
	startedAt     time.Time
	endedAt       time.Time
	events        []cuaEvent
	imageSlots    []int // events 里 frame_image 的下标，用于裁剪历史截图
	subs          map[chan cuaEvent]struct{}
	done          chan struct{}
	cmd           *exec.Cmd
	stderrTail    []string
}

func newCUARun(id, userID, task, model, token string, dryRun bool) *cuaRun {
	return &cuaRun{
		ID: id, UserID: userID, Task: task, Model: model, Token: token, DryRun: dryRun,
		status: "running", startedAt: time.Now(),
		subs: map[chan cuaEvent]struct{}{}, done: make(chan struct{}),
	}
}

func (r *cuaRun) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// cuaMaxFrameImages 是每个 run 的历史里最多保留的截图条数（契约 2.3）。
// 单帧约 9.4KB，200 步的 run 事件约 1.9MB；50 条 run 全留最坏约 95MB，对桌面应用过大。
// frame_image 在契约第 1 节是可选事件，前端收不到也要能正常工作，所以只裁历史。
const cuaMaxFrameImages = 10

// cuaFrameImageEvent 是截图事件的 kind。
const cuaFrameImageEvent = "frame_image"

// trimImageHistoryLocked 丢掉最旧的截图，直到历史里只剩 cuaMaxFrameImages 张。
// 只动「已存储的历史快照」：实时订阅者在上面的广播里已经收到过这一帧，
// 正在跑的 run 的实时流不受任何影响。非图片事件一条都不动。
func (r *cuaRun) trimImageHistoryLocked() {
	if len(r.imageSlots) <= cuaMaxFrameImages {
		return
	}
	drop := r.imageSlots[0]
	r.imageSlots = r.imageSlots[1:]
	kept := make([]cuaEvent, 0, len(r.events)-1)
	for i, ev := range r.events {
		if i == drop {
			continue
		}
		kept = append(kept, ev)
	}
	r.events = kept
	for i := range r.imageSlots {
		r.imageSlots[i]--
	}
}

// emit 记录并广播一条事件。持有锁做非阻塞投递：订阅者积压时丢这一条，
// 而不是把读 stdout 的循环堵死；客户端重连后能从历史里补发。
func (r *cuaRun) emit(name string, data []byte) {
	ev := cuaEvent{Name: name, Data: data}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.events = append(r.events, ev)
	if name == cuaFrameImageEvent {
		r.imageSlots = append(r.imageSlots, len(r.events)-1)
		r.trimImageHistoryLocked()
	}
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
			log.Printf("cua: run %s 的 SSE 订阅者积压，丢弃一条 %s 事件（重连可补发）", r.ID, name)
		}
	}
}

// subscribe 注册订阅者并取历史快照。两件事在同一把锁里完成，
// 保证不漏事件也不重复。已结束的 run 只给快照（其中已含 exit）。
func (r *cuaRun) subscribe() (chan cuaEvent, []cuaEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	history := append([]cuaEvent(nil), r.events...)
	if r.finished {
		return nil, history, true
	}
	ch := make(chan cuaEvent, cuaSubBuffer)
	r.subs[ch] = struct{}{}
	return ch, history, false
}

func (r *cuaRun) unsubscribe(ch chan cuaEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, ch)
}

// finish 收尾：补一条合成的 exit 事件（契约 2.3 要求它固定是最后一个），
// 广播后关闭 done 让订阅者排空退出。只生效一次。
func (r *cuaRun) finish(status, code string, exitCode int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.status = status
	r.code = code
	r.exitCode = exitCode
	r.endedAt = time.Now()
	data := jsonMustMarshal(gin.H{"run_id": r.ID, "exit_code": exitCode, "code": code})
	exitEv := cuaEvent{Name: "exit", Data: data}
	r.events = append(r.events, exitEv)
	for ch := range r.subs {
		select {
		case ch <- exitEv:
		default:
		}
	}
	r.finished = true
	close(r.done)
}

func (r *cuaRun) item() cuaRunItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	it := cuaRunItem{
		RunID: r.ID, Task: r.Task, Model: r.Model,
		Status: r.status, Code: r.code, Steps: r.steps,
		StartedAt: r.startedAt.Format(time.RFC3339),
	}
	if !r.endedAt.IsZero() {
		it.EndedAt = r.endedAt.Format(time.RFC3339)
	}
	return it
}

type cuaRunItem struct {
	RunID     string `json:"run_id"`
	Task      string `json:"task"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Code      string `json:"code"`
	Steps     int    `json:"steps"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
}

// ---------------------------------------------------------------------------
// run 管理器（内存，最近 50 条）
// ---------------------------------------------------------------------------

type cuaManager struct {
	mu      sync.Mutex
	runs    []*cuaRun // 新的在前
	byToken map[string]*cuaRun
	current *cuaRun
}

var cuaMgr = &cuaManager{byToken: map[string]*cuaRun{}}

// reserve 抢唯一的运行位。已有 run 在跑则返回 false（契约 2.2 → 409）。
func (m *cuaManager) reserve(r *cuaRun) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && !m.current.isFinished() {
		return false
	}
	m.runs = append([]*cuaRun{r}, m.runs...)
	m.byToken[r.Token] = r
	m.current = r
	m.trimLocked()
	return true
}

func (m *cuaManager) clearCurrent(r *cuaRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == r {
		m.current = nil
	}
}

func (m *cuaManager) find(id string) *cuaRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (m *cuaManager) findByToken(token string) *cuaRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byToken[token]
}

func (m *cuaManager) currentID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.isFinished() {
		return ""
	}
	return m.current.ID
}

func (m *cuaManager) list() []*cuaRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*cuaRun(nil), m.runs...)
}

// trimLocked 只淘汰已结束的旧 run；运行中的那条绝不淘汰。
func (m *cuaManager) trimLocked() {
	if len(m.runs) <= cuaHistoryLimit {
		return
	}
	over := len(m.runs) - cuaHistoryLimit
	kept := make([]*cuaRun, 0, cuaHistoryLimit)
	for _, r := range m.runs {
		if over > 0 && r.isFinished() {
			delete(m.byToken, r.Token)
			over--
			continue
		}
		kept = append(kept, r)
	}
	m.runs = kept
}

// ---------------------------------------------------------------------------
// 子进程
// ---------------------------------------------------------------------------

func cuaPythonPath() string {
	if p := os.Getenv("CUA_PYTHON"); p != "" {
		return p
	}
	return "python"
}

// cuaSidecarPath 找 bin/cua-sidecar.exe（契约 5）。找不到返回空串。
func cuaSidecarPath() string {
	if p := os.Getenv("CUA_SIDECAR"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	name := "cua-sidecar"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidates := []string{filepath.Join("bin", name)}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "bin", name))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// cuaLogCommandChoice 启动时把「用哪个 CUA 子进程」打进日志。
//
// 优先级（契约 5）：CUA_SIDECAR（显式指定，文件存在才认）> bin/cua-sidecar.exe > python -m cua.main。
// 踩过的坑：bin/cua-sidecar.exe 一落盘就会盖过 CUA_PYTHON，验证「python 回落」时必须用
// CUA_SIDECAR 显式指到别的路径（或临时移走 sidecar），否则你以为在测 Python，其实跑的是 sidecar。
func cuaLogCommandChoice() {
	const order = "优先级 CUA_SIDECAR > bin/cua-sidecar.exe > python -m cua.main"
	if p := cuaSidecarPath(); p != "" {
		src := "bin/cua-sidecar.exe"
		if os.Getenv("CUA_SIDECAR") != "" {
			src = "CUA_SIDECAR"
		}
		log.Printf("cua: CUA 子进程 = %s（来源 %s；%s）", p, src, order)
		return
	}
	log.Printf("cua: 未找到 bin/cua-sidecar.exe，CUA 子进程回落 %s -m cua.main（%s）", cuaPythonPath(), order)
}

// cuaCommand 组装子进程命令：优先 sidecar，回落 python -m cua.main。
func cuaCommand(args []string) *exec.Cmd {
	if p := cuaSidecarPath(); p != "" {
		return exec.Command(p, args...)
	}
	return exec.Command(cuaPythonPath(), append([]string{"-m", "cua.main"}, args...)...)
}

type cuaRunOptions struct {
	MaxSteps              int
	MaxSeconds            int
	AllowDangerousHotkeys bool
}

// spawn 起子进程并挂上读取协程。进程与请求生命周期解耦：
// 这里刻意不用 exec.CommandContext，POST 返回后进程继续跑（契约 2.3）。
func (r *cuaRun) spawn(proxyBaseURL string, opts cuaRunOptions) error {
	args := []string{
		"--task", r.Task,
		"--model", r.Model,
		"--max-steps", strconv.Itoa(opts.MaxSteps),
		"--max-seconds", strconv.Itoa(opts.MaxSeconds),
		"--events", "stdout",
		"--events-images",
	}
	if r.DryRun {
		args = append(args, "--dry-run")
	}
	if extra := strings.Fields(os.Getenv("CUA_EXTRA_ARGS")); len(extra) > 0 {
		args = append(args, extra...)
	}

	cmd := cuaCommand(args)
	allow := "0"
	if opts.AllowDangerousHotkeys {
		allow = "1"
	}
	cmd.Env = append(os.Environ(),
		"CUA_MODEL_BASE_URL="+proxyBaseURL,
		"CUA_MODEL_API_KEY="+r.Token,
		"CUA_LLM_TOKEN="+r.Token,
		"CUA_ALLOW_DANGEROUS_HOTKEYS="+allow,
		// Python 管道下 stdout 默认走系统 locale（本机实测 gbk），中文会坏成 U+FFFD（emoji 更会直接
		// UnicodeEncodeError 崩溃）。两侧都钉成 UTF-8，保证 Go 读到的 JSONL 与子进程输出逐字一致。
		// 注：argv 不经此路径（Windows 走宽字符 API GetCommandLineW），无需处理。
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8",
		"PYTHONUNBUFFERED=1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("获取 CUA stdout 管道失败：%w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("获取 CUA stderr 管道失败：%w", err)
	}

	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 CUA 子进程失败：%w", err)
	}

	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.readStdout(stdout) }()
		go func() { defer wg.Done(); r.readStderr(stderr) }()
		// 必须等两个读取协程到 EOF 再 Wait：cmd.Wait 会关掉管道读端，
		// 先 Wait 有截断最后几行 JSONL 的风险。
		wg.Wait()
		r.reap(cmd)
	}()
	return nil
}

func (r *cuaRun) reap(cmd *exec.Cmd) {
	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
			log.Printf("cua: run %s 等待子进程失败：%v", r.ID, err)
		}
	}

	r.mu.Lock()
	stopped := r.stopRequested
	code := r.code
	tail := append([]string(nil), r.stderrTail...)
	r.mu.Unlock()

	if exitCode != 0 && len(tail) > 0 {
		log.Printf("cua: run %s 退出码 %d，stderr 尾部：%s", r.ID, exitCode, strings.Join(tail, " | "))
	}

	status := "done"
	switch {
	case stopped:
		status = "stopped"
	case exitCode != 0:
		status = "failed"
	}
	r.finish(status, code, exitCode)
	cuaMgr.clearCurrent(r)
}

// cuaWireEvent 只取分发需要的字段，原始行照原样透传给 SSE。
type cuaWireEvent struct {
	Kind  string  `json:"kind"`
	Step  int     `json:"step"`
	Steps int     `json:"steps"`
	Code  *string `json:"code"`
}

func (r *cuaRun) readStdout(rc io.ReadCloser) {
	defer rc.Close()
	reader := bufio.NewReaderSize(rc, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			r.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (r *cuaRun) handleLine(raw []byte) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 {
		return
	}
	var ev cuaWireEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Kind == "" {
		// 契约 1 要求 stdout 只允许 JSONL；真出现脏行也不能把整条流带崩。
		log.Printf("cua: run %s stdout 出现非 JSONL 行（已跳过）：%.200s", r.ID, line)
		return
	}

	r.mu.Lock()
	if ev.Step > r.steps {
		r.steps = ev.Step
	}
	if ev.Kind == "run_end" {
		if ev.Steps > r.steps {
			r.steps = ev.Steps
		}
		if ev.Code != nil {
			r.code = *ev.Code
		}
	}
	r.mu.Unlock()

	r.emit(ev.Kind, line)
}

func (r *cuaRun) readStderr(rc io.ReadCloser) {
	defer rc.Close()
	reader := bufio.NewReaderSize(rc, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if text := strings.TrimSpace(string(line)); text != "" {
			r.mu.Lock()
			r.stderrTail = append(r.stderrTail, text)
			if len(r.stderrTail) > cuaStderrKeepLines {
				r.stderrTail = r.stderrTail[len(r.stderrTail)-cuaStderrKeepLines:]
			}
			r.mu.Unlock()
			log.Printf("cua[%s] stderr: %s", r.ID, text)
		}
		if err != nil {
			return
		}
	}
}

func (r *cuaRun) requestStop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return false
	}
	r.stopRequested = true
	return true
}

// killTree 杀进程树。Windows 用 taskkill /T /F（契约 2.4）。
func (r *cuaRun) killTree() {
	r.mu.Lock()
	cmd := r.cmd
	var pid int
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	r.mu.Unlock()
	if pid <= 0 {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput(); err != nil {
			log.Printf("cua: taskkill 失败 (pid=%d): %v; output=%s", pid, err, strings.TrimSpace(string(out)))
		}
		return
	}
	// 非 Windows 只保证杀直接子进程；交付目标是 Windows/Electron。
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// ---------------------------------------------------------------------------
// 探测（status 用）
// ---------------------------------------------------------------------------

type cuaProbeInfo struct {
	Found   bool   `json:"found"`
	Version string `json:"version"`
}

type cuaEnvInfo struct {
	ScreenW  int     `json:"screen_w"`
	ScreenH  int     `json:"screen_h"`
	DPIScale float64 `json:"dpi_scale"`
}

type cuaModelInfo struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Vision  bool   `json:"vision"`
}

type cuaStatusInfo struct {
	Available bool
	Python    cuaProbeInfo
	CUA       cuaProbeInfo
	Env       cuaEnvInfo
	Notes     []string
}

var (
	cuaProbeMu    sync.Mutex
	cuaProbeAt    time.Time
	cuaProbeCache cuaStatusInfo
)

func cuaProbeRun(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// cuaEnvProbeScript 用 ctypes 拿主屏物理分辨率与缩放，不依赖 pyautogui。
const cuaEnvProbeScript = `import ctypes, json
try:
    ctypes.windll.shcore.SetProcessDpiAwareness(2)
except Exception:
    pass
try:
    u = ctypes.windll.user32
    w = int(u.GetSystemMetrics(0)); h = int(u.GetSystemMetrics(1))
except Exception:
    w = 0; h = 0
try:
    s = round(ctypes.windll.shcore.GetScaleFactorForDevice(0) / 100.0, 2)
except Exception:
    s = 1.0
print(json.dumps({"screen_w": w, "screen_h": h, "dpi_scale": s}))`

// cuaProbe 探测 python / cua / 屏幕信息，缓存 30s，避免 status 每次都起进程。
func cuaProbe() cuaStatusInfo {
	cuaProbeMu.Lock()
	defer cuaProbeMu.Unlock()
	if time.Since(cuaProbeAt) < cuaProbeTTL {
		return cuaProbeCache
	}

	info := cuaStatusInfo{Notes: []string{}}
	info.Python, _ = cuaProbePython()

	sidecar := cuaSidecarPath()
	if sidecar != "" {
		if out, err := cuaProbeRun(sidecar, "--version"); err == nil {
			info.CUA = cuaProbeInfo{Found: true, Version: cuaParseVersion(out)}
		} else {
			info.Notes = append(info.Notes, fmt.Sprintf("bin/cua-sidecar 存在但 --version 失败：%v", err))
		}
	} else {
		info.Notes = append(info.Notes, "未找到 bin/cua-sidecar.exe，回落 python -m cua.main（开发态）")
	}
	if !info.CUA.Found {
		if !info.Python.Found {
			info.Notes = append(info.Notes, "既没有 sidecar 也没有可用的 python，CUA 不可用")
		} else if out, err := cuaProbeRun(cuaPythonPath(), "-c", "import cua,sys;sys.stdout.write(cua.__version__)"); err == nil {
			info.CUA = cuaProbeInfo{Found: true, Version: out}
		} else {
			info.Notes = append(info.Notes, "未找到 cua 包（python -c import cua 失败）："+out)
		}
	}
	info.Available = info.CUA.Found

	if info.Python.Found {
		if out, err := cuaProbeRun(cuaPythonPath(), "-c", cuaEnvProbeScript); err == nil {
			var env cuaEnvInfo
			if json.Unmarshal([]byte(out), &env) == nil && env.ScreenW > 0 && env.ScreenH > 0 {
				info.Env = env
			} else {
				info.Notes = append(info.Notes, "屏幕信息探测失败（拿不到主屏分辨率）")
			}
		} else {
			info.Notes = append(info.Notes, "屏幕信息探测失败："+out)
		}
	}

	cuaProbeCache = info
	cuaProbeAt = time.Now()
	return info
}

func cuaProbePython() (cuaProbeInfo, string) {
	out, err := cuaProbeRun(cuaPythonPath(), "--version")
	if err != nil {
		return cuaProbeInfo{}, fmt.Sprintf("未检测到 python（%s --version 失败：%v）", cuaPythonPath(), err)
	}
	return cuaProbeInfo{Found: true, Version: cuaParseVersion(out)}, ""
}

// cuaParseVersion 从 "Python 3.13.5" / "cua 0.1.0" 里取版本号。
func cuaParseVersion(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " "); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// cuaVisionModels 只列支持视觉的模型（CUA 必须有视觉，契约 2.1）。
func cuaVisionModels() []cuaModelInfo {
	items := make([]cuaModelInfo, 0)
	if registry == nil {
		return items
	}
	for _, ep := range registry.Endpoints() {
		for _, m := range ep.Models {
			if !llm.ModelSupportsVision(m) {
				continue
			}
			items = append(items, cuaModelInfo{Name: m, Backend: ep.Backend, Vision: true})
		}
	}
	return items
}

func cuaIsVisionModel(model string) bool {
	for _, m := range cuaVisionModels() {
		if m.Name == model {
			return true
		}
	}
	return false
}

func cuaNewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；用时间戳兜底也要保证 32 位十六进制。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// cuaNewToken 生成每 run 一次性 token（契约 3）。
// 集成测试可用 CUA_LLM_TOKEN 固定它（已设则不覆盖）—— 否则 token 每轮随机，黑盒打不到代理。
func cuaNewToken() string {
	if t := strings.TrimSpace(os.Getenv("CUA_LLM_TOKEN")); t != "" {
		return t
	}
	return cuaNewID()
}

// cuaProxyBaseURL 给子进程指回本进程的模型代理（契约 3 要求 127.0.0.1:<port>）。
// 端口取自本次请求的 Host —— main.go 的监听口不在 web 包里，请求 Host 是最可靠的来源。
func cuaProxyBaseURL(c *gin.Context) string {
	port := ""
	if _, p, err := net.SplitHostPort(c.Request.Host); err == nil {
		port = p
	}
	if port == "" {
		port = os.Getenv("APP_PORT")
	}
	if port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port + "/internal/cua/llm/v1"
}

// ---------------------------------------------------------------------------
// HTTP handler（契约 2）
// ---------------------------------------------------------------------------

// cuaStatus 环境与可用性。不可用不是错误，永远 200（契约 2.1）。
func cuaStatus(c *gin.Context) {
	info := cuaProbe()
	runID := cuaMgr.currentID()
	c.JSON(http.StatusOK, gin.H{
		"available": info.Available,
		"running":   runID != "",
		"run_id":    runID,
		"python":    info.Python,
		"cua":       info.CUA,
		"env":       info.Env,
		"models":    cuaVisionModels(),
		"notes":     info.Notes,
	})
}

// cuaCreateRun 创建并启动一次运行（契约 2.2）。
func cuaCreateRun(c *gin.Context) {
	var body struct {
		Task                  string `json:"task"`
		Model                 string `json:"model"`
		MaxSteps              *int   `json:"max_steps"`
		MaxSeconds            *int   `json:"max_seconds"`
		DryRun                bool   `json:"dry_run"`
		AllowDangerousHotkeys bool   `json:"allow_dangerous_hotkeys"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	task := strings.TrimSpace(body.Task)
	if task == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task 不能为空"})
		return
	}
	if utf8.RuneCountInString(task) > cuaTaskMaxLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task 长度不能超过 %d 字符", cuaTaskMaxLen)})
		return
	}

	model := strings.TrimSpace(body.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model 不能为空（不允许静默用错模型）"})
		return
	}
	if !cuaIsVisionModel(model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("模型 %q 不在可用视觉模型列表里", model)})
		return
	}

	steps := cuaMaxStepsDefault
	if body.MaxSteps != nil {
		steps = *body.MaxSteps
		if steps < cuaMaxStepsMin || steps > cuaMaxStepsMax {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("max_steps 超出范围 %d-%d", cuaMaxStepsMin, cuaMaxStepsMax)})
			return
		}
	}
	seconds := cuaMaxSecondsDefault
	if body.MaxSeconds != nil {
		seconds = *body.MaxSeconds
		if seconds < cuaMaxSecondsMin || seconds > cuaMaxSecondsMax {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("max_seconds 超出范围 %d-%d", cuaMaxSecondsMin, cuaMaxSecondsMax)})
			return
		}
	}

	run := newCUARun(cuaNewID(), currentUserID(c), task, model, cuaNewToken(), body.DryRun)
	if !cuaMgr.reserve(run) {
		c.JSON(http.StatusConflict, gin.H{"error": "已有任务在运行"})
		return
	}
	if err := run.spawn(cuaProxyBaseURL(c), cuaRunOptions{
		MaxSteps:              steps,
		MaxSeconds:            seconds,
		AllowDangerousHotkeys: body.AllowDangerousHotkeys,
	}); err != nil {
		run.finish("failed", "", -1)
		cuaMgr.clearCurrent(run)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"run_id": run.ID, "model": model, "task": task})
}

// cuaRunEvents SSE（契约 2.3）：首个 start、CUA 的 kind 原样透传、最后 exit。
func cuaRunEvents(c *gin.Context) {
	run := cuaMgr.find(c.Param("id"))
	if run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run 不存在"})
		return
	}

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	write := func(name string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		flusher.Flush()
	}

	write("start", jsonMustMarshal(gin.H{"run_id": run.ID, "model": run.Model, "task": run.Task}))

	ch, history, finished := run.subscribe()
	if ch != nil {
		defer run.unsubscribe(ch)
	}
	for _, ev := range history {
		write(ev.Name, ev.Data)
	}
	if finished {
		// 已结束的 run：补发全部历史（含 exit）后关闭，支持刷新页面重连。
		return
	}

	ctx := c.Request.Context()
	for {
		select {
		case ev := <-ch:
			write(ev.Name, ev.Data)
		case <-run.done:
			// exit 已入队，排空后再关；客户端断开只退订，不杀进程。
			for {
				select {
				case ev := <-ch:
					write(ev.Name, ev.Data)
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// cuaStopRun 停止该 run（杀进程树，契约 2.4）。
func cuaStopRun(c *gin.Context) {
	run := cuaMgr.find(c.Param("id"))
	if run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run 不存在"})
		return
	}
	if !run.requestStop() {
		c.JSON(http.StatusOK, gin.H{"stopped": false})
		return
	}
	run.killTree()
	c.JSON(http.StatusOK, gin.H{"stopped": true})
}

// cuaListRuns 历史（内存最近 50 条，契约 2.5），新的在前。
func cuaListRuns(c *gin.Context) {
	runs := cuaMgr.list()
	items := make([]cuaRunItem, 0, len(runs))
	for _, r := range runs {
		items = append(items, r.item())
	}
	c.JSON(http.StatusOK, gin.H{"runs": items})
}
