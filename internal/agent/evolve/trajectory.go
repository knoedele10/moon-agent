// Package evolve 实现 AI 助手的「自我进化」层：运行轨迹、技能使用账本与体检、
// 轨迹提炼候选、后台固化（Curator）、结构化记忆与回放评测。
//
// 设计边界（契约 docs/agent/11-hermes-parity.md §4）：
//   - 本包只做确定性逻辑与文件落盘，不连数据库、不调模型；
//   - 技能库通过 SkillStore 接口注入，具体实现留在 web 层（复用 model.AgentSkill）；
//   - 全部能力默认关闭：没有调用方（internal/web/evolve.go）装配时本包不产生任何文件；
//   - 所有数据目录由调用方通过 Dir 参数给出，Dir 为空才回落到用户配置目录，
//     不允许把路径硬编码到某个人的主目录。
package evolve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// dirTrajectories 是轨迹 JSONL 的存放子目录（按天分文件）。
	dirTrajectories = "trajectories"
	// dirArchive 是旧轨迹压缩后的存放子目录（按月聚合成一个文件）。
	dirArchive = "archive"
	// reportFileName 是 Curator 最近一次报告的固定文件名。
	reportFileName = "last-report.json"
	// maxJSONLineBytes 是单行 JSON 的硬上限：超过就截断，避免一条巨型工具结果
	// 把整行撑到几 MB，拖慢后续 List/回放（契约 §4「单行 JSON 不超过 32KB」）。
	maxJSONLineBytes = 32 * 1024
	// maxArgsRunes / maxSummaryRunes / maxNoteRunes 是截断时各字段的保留长度。
	maxArgsRunes    = 800
	maxSummaryRunes = 400
	maxNoteRunes    = 600
	// truncatedMark 是截断标记：追加在字段末尾，读的人一眼能看出内容不完整。
	truncatedMark = "…（已截断）"
)

// DefaultDir 返回自我进化数据的默认目录：用户配置目录下的 blog-my/evolve。
//
// 只有调用方明确不关心位置（Dir 传空）时才该用它；单元测试一律传 t.TempDir()。
func DefaultDir() string {
	if d, err := os.UserConfigDir(); err == nil && strings.TrimSpace(d) != "" {
		return filepath.Join(d, "blog-my", "evolve")
	}
	// 极端环境（容器里没有 HOME）退到临时目录，保证功能可用而不是直接报错。
	return filepath.Join(os.TempDir(), "blog-my", "evolve")
}

// RunMeta 是一次运行的元信息：谁、在哪个会话、用什么模型跑的。
type RunMeta struct {
	RunID          string    `json:"run_id"`
	SessionID      string    `json:"session_id,omitempty"`
	UserID         string    `json:"user_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Model          string    `json:"model,omitempty"`
	Engine         string    `json:"engine,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	StartedAt      time.Time `json:"started_at"`
}

// ToolCallRecord 是一次工具调用的轨迹：调了什么、成没成、花了多久。
type ToolCallRecord struct {
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	OK      bool   `json:"ok"`
	IsError bool   `json:"is_error,omitempty"`
	Summary string `json:"summary,omitempty"`
	MS      int64  `json:"ms,omitempty"`
	Spilled bool   `json:"spilled,omitempty"`
}

// RunRecord 是一次完整运行的轨迹。Outcome 取值：ok | limit | error | interrupted | await_user。
type RunRecord struct {
	RunMeta
	ToolCalls  []ToolCallRecord `json:"tool_calls,omitempty"`
	Outcome    string           `json:"outcome"`
	Note       string           `json:"note,omitempty"`
	FinishedAt time.Time        `json:"finished_at"`
	Rounds     int              `json:"rounds,omitempty"`
	Tokens     int              `json:"tokens,omitempty"`
}

// Level 是 AGENT_EVOLVE 的档位。关闭档下 web 层不装配任何 Recorder，
// 对应 RecorderFor(LevelOff, ...) 返回 nil。
type Level string

const (
	// LevelOff 完全关闭：不记轨迹、不体检、不固化。
	LevelOff Level = "off"
	// LevelRecord 只记轨迹（为后续评测/提炼积累数据）。
	LevelRecord Level = "record"
	// LevelSuggest 记轨迹 + 后台产出建议报告，但不改技能库。
	LevelSuggest Level = "suggest"
	// LevelAuto 记轨迹 + 自动固化（受 Curator.MaxMutates 限制）。
	LevelAuto Level = "auto"
)

// ParseLevel 把 AGENT_EVOLVE 的原始字符串解析成档位；无法识别一律按关闭处理
// （默认关闭是契约铁律：拼错环境变量不能让功能意外打开）。
func ParseLevel(raw string) Level {
	switch Level(strings.ToLower(strings.TrimSpace(raw))) {
	case LevelRecord:
		return LevelRecord
	case LevelSuggest:
		return LevelSuggest
	case LevelAuto:
		return LevelAuto
	default:
		return LevelOff
	}
}

// Recorder 把运行轨迹写成按天分文件的 JSONL。
//
// 写失败只返回 error，绝不 panic：轨迹是旁路数据，坏了不能拖垮对话。
type Recorder struct {
	// Dir 是数据根目录（下面会有 trajectories/ 与 archive/）。为空时用 DefaultDir()。
	Dir string
	// Now 是可注入的时钟，单测里用它固定日期；为空时用 time.Now。
	Now func() time.Time
}

// NewRecorder 建一个轨迹记录器。dir 为空时回落到 DefaultDir()。
func NewRecorder(dir string) *Recorder { return &Recorder{Dir: dir} }

// RecorderFor 按档位取记录器：LevelOff 返回 nil（表示「不产生任何轨迹文件」）。
//
// 返回的 *Recorder 的所有方法都做了 nil 接收者保护，因此调用方可以放心地
// rec.Start(...) / run.Finish(...) 而不用到处判空。
func RecorderFor(level Level, dir string) *Recorder {
	if level == LevelOff {
		return nil
	}
	return NewRecorder(dir)
}

// dir 返回解析后的数据根目录。
func (r *Recorder) dir() string {
	if r != nil && strings.TrimSpace(r.Dir) != "" {
		return r.Dir
	}
	return DefaultDir()
}

// now 返回当前时间（优先用注入的时钟）。
func (r *Recorder) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run 是一次进行中的运行。方法并发安全（模型可能并行调用多个工具）。
type Run struct {
	mu       sync.Mutex
	recorder *Recorder
	meta     RunMeta
	path     string
	calls    []ToolCallRecord
	rounds   int
	tokens   int
	finished bool
}

// Start 开始记录一次运行。r 为 nil（关闭档）时返回 nil，后续调用全部是安全的空操作。
func (r *Recorder) Start(meta RunMeta) *Run {
	if r == nil {
		return nil
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = r.now()
	}
	day := meta.StartedAt.Format("2006-01-02")
	return &Run{
		recorder: r,
		meta:     meta,
		path:     filepath.Join(r.dir(), dirTrajectories, day+".jsonl"),
	}
}

// Tool 追加一条工具调用记录。run 为 nil 时是空操作。
func (run *Run) Tool(rec ToolCallRecord) {
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.finished {
		return
	}
	rec.Args = RedactArguments(rec.Args)
	rec.Summary = Redact(rec.Summary)
	run.calls = append(run.calls, rec)
}

// SetTotals 补充本轮运行的轮数与 token 用量（由接线层在结束时调用）。
func (run *Run) SetTotals(rounds, tokens int) {
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	run.rounds = rounds
	run.tokens = tokens
}

// Finish 结束一次运行并把整条记录原子追加到 Dir/trajectories/YYYY-MM-DD.jsonl。
//
// 返回的 error 只代表「轨迹没写成」，调用方应当忽略它继续对话（契约 §4 要求
// 写失败不得影响调用方）。重复 Finish 只写一次。
func (run *Run) Finish(outcome, note string) error {
	if run == nil {
		return nil
	}
	run.mu.Lock()
	if run.finished {
		run.mu.Unlock()
		return nil
	}
	run.finished = true
	if strings.TrimSpace(outcome) == "" {
		outcome = "ok"
	}
	rec := RunRecord{
		RunMeta:   run.meta,
		Outcome:   outcome,
		Note:      Redact(note),
		Rounds:    run.rounds,
		Tokens:    run.tokens,
		ToolCalls: append([]ToolCallRecord(nil), run.calls...),
	}
	rec.FinishedAt = run.recorder.now()
	path := run.path
	run.mu.Unlock()

	line, err := marshalRunLine(rec)
	if err != nil {
		return err
	}
	return appendJSONLine(path, line)
}

// List 读回最近 days 天的轨迹（days <= 0 表示全部）。坏行会跳过而不是让整个读取失败。
func (r *Recorder) List(days int) ([]RunRecord, error) {
	if r == nil {
		return nil, nil
	}
	dir := filepath.Join(r.dir(), dirTrajectories)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evolve: 读取轨迹目录失败: %w", err)
	}
	var cutoff time.Time
	if days > 0 {
		cutoff = r.now().AddDate(0, 0, -days)
	}
	out := make([]RunRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		recs, err := readRunRecords(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			if !cutoff.IsZero() && !rec.StartedAt.IsZero() && rec.StartedAt.Before(cutoff) {
				continue
			}
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].RunID < out[j].RunID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

// CompressReport 是一次旧轨迹压缩的结果。
type CompressReport struct {
	ArchiveDir string `json:"archive_dir"`
	Files      int    `json:"files"`
	Runs       int    `json:"runs"`
	Bytes      int64  `json:"bytes"`
}

// CompressBefore 把 cutoff 之前的按天轨迹聚合成 Dir/archive/YYYY-MM.jsonl，并删掉原文件。
//
// 为什么压缩而不是直接删：旧轨迹是评测与提炼的原料，只是不再需要按天分片；
// 按月归档既省 inode，又保留回放能力。摘要写进 archive/summary.json，便于界面展示。
func (r *Recorder) CompressBefore(cutoff time.Time) (CompressReport, error) {
	rep := CompressReport{}
	if r == nil {
		return rep, nil
	}
	archiveDir := filepath.Join(r.dir(), dirArchive)
	rep.ArchiveDir = archiveDir
	trajDir := filepath.Join(r.dir(), dirTrajectories)
	entries, err := os.ReadDir(trajDir)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("evolve: 读取轨迹目录失败: %w", err)
	}
	if cutoff.IsZero() {
		cutoff = r.now()
	}
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())

	byMonth := map[string][]string{}
	monthStats := map[string]*archiveMonth{}
	oldFiles := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(e.Name(), ".jsonl"), time.Local)
		if err != nil || !day.Before(cutoffDay) {
			continue
		}
		path := filepath.Join(trajDir, e.Name())
		lines, err := readValidJSONLines(path)
		if err != nil {
			return rep, err
		}
		month := day.Format("2006-01")
		if monthStats[month] == nil {
			monthStats[month] = &archiveMonth{}
		}
		monthStats[month].Files++
		rep.Files++
		oldFiles = append(oldFiles, path)
		for _, line := range lines {
			byMonth[month] = append(byMonth[month], string(line))
			monthStats[month].Runs++
			monthStats[month].Bytes += int64(len(line)) + 1
			rep.Runs++
			rep.Bytes += int64(len(line)) + 1
		}
	}
	if len(oldFiles) == 0 {
		return rep, nil
	}
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return rep, fmt.Errorf("evolve: 创建归档目录失败: %w", err)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, m := range months {
		path := filepath.Join(archiveDir, m+".jsonl")
		for _, line := range byMonth[m] {
			if err := appendJSONLine(path, []byte(line)); err != nil {
				return rep, fmt.Errorf("evolve: 写归档失败: %w", err)
			}
		}
	}
	// 只有归档全部落盘成功后才删原文件：中途失败宁可重复归档，也不丢数据。
	for _, path := range oldFiles {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("evolve: 删除旧轨迹失败: %w", err)
		}
	}
	if err := mergeArchiveSummary(archiveDir, monthStats); err != nil {
		return rep, err
	}
	return rep, nil
}

// archiveMonth 是 archive/summary.json 里单月的统计。
type archiveMonth struct {
	Files int   `json:"files"`
	Runs  int   `json:"runs"`
	Bytes int64 `json:"bytes"`
}

// archiveSummary 是归档摘要文件的内容。
type archiveSummary struct {
	UpdatedAt time.Time                `json:"updated_at"`
	Months    map[string]*archiveMonth `json:"months"`
}

// mergeArchiveSummary 合并写入归档摘要（已有月份累加，保留历史）。
func mergeArchiveSummary(archiveDir string, add map[string]*archiveMonth) error {
	path := filepath.Join(archiveDir, "summary.json")
	sum := archiveSummary{Months: map[string]*archiveMonth{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &sum); err != nil || sum.Months == nil {
			sum = archiveSummary{Months: map[string]*archiveMonth{}}
		}
	}
	for month, st := range add {
		cur := sum.Months[month]
		if cur == nil {
			cur = &archiveMonth{}
			sum.Months[month] = cur
		}
		cur.Files += st.Files
		cur.Runs += st.Runs
		cur.Bytes += st.Bytes
	}
	sum.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		return fmt.Errorf("evolve: 写归档摘要失败: %w", err)
	}
	return nil
}

// readRunRecords 读一个 JSONL 文件里的运行记录，坏行跳过。
func readRunRecords(path string) ([]RunRecord, error) {
	lines, err := readValidJSONLines(path)
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(lines))
	for _, line := range lines {
		var rec RunRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // 半行/脏数据：跳过这一行，其余照读
		}
		out = append(out, rec)
	}
	return out, nil
}

// readValidJSONLines 读文件里的非空且是合法 JSON 对象的行。
func readValidJSONLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evolve: 打开 %s 失败: %w", filepath.Base(path), err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		out = append(out, []byte(line))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("evolve: 读取 %s 失败: %w", filepath.Base(path), err)
	}
	return out, nil
}

// marshalRunLine 把一条运行记录序列化成单行 JSON，超过 32KB 时逐级截断。
//
// 截断顺序：长参数/摘要 → 长备注 → 丢弃较早的工具调用 → 只留元信息。
// 每一步都在被截断的字段末尾加「…（已截断）」标记，保证数据不完整是可见的。
func marshalRunLine(rec RunRecord) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("evolve: 序列化轨迹失败: %w", err)
	}
	if len(b) <= maxJSONLineBytes {
		return b, nil
	}
	trimmed := rec
	trimmed.Note = clipWithMark(trimmed.Note, maxNoteRunes)
	trimmed.ToolCalls = make([]ToolCallRecord, len(rec.ToolCalls))
	for i, c := range rec.ToolCalls {
		c.Args = clipWithMark(c.Args, maxArgsRunes)
		c.Summary = clipWithMark(c.Summary, maxSummaryRunes)
		trimmed.ToolCalls[i] = c
	}
	b, err = json.Marshal(trimmed)
	if err == nil && len(b) <= maxJSONLineBytes {
		return b, nil
	}
	for keep := len(trimmed.ToolCalls) / 2; keep > 0; keep /= 2 {
		t := trimmed
		t.ToolCalls = append([]ToolCallRecord(nil), trimmed.ToolCalls[len(trimmed.ToolCalls)-keep:]...)
		b, err = json.Marshal(t)
		if err == nil && len(b) <= maxJSONLineBytes {
			return b, nil
		}
	}
	minimal := RunRecord{
		RunMeta:    rec.RunMeta,
		Outcome:    rec.Outcome,
		Note:       clipWithMark(rec.Note+"（轨迹过长，工具调用已丢弃）", maxNoteRunes),
		FinishedAt: rec.FinishedAt,
		Rounds:     rec.Rounds,
		Tokens:     rec.Tokens,
	}
	b, err = json.Marshal(minimal)
	if err != nil {
		return nil, fmt.Errorf("evolve: 序列化轨迹失败: %w", err)
	}
	if len(b) > maxJSONLineBytes {
		minimal.Note = truncatedMark
		return json.Marshal(minimal)
	}
	return b, nil
}

// clipWithMark 按 rune 截断并在末尾追加截断标记。
func clipWithMark(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + truncatedMark
}

// appendLocks 按文件路径分锁：同一文件的并发追加必须串行，不同文件互不阻塞。
var appendLocks sync.Map // map[string]*sync.Mutex

// lockFor 取某个路径专属的互斥锁。
func lockFor(path string) *sync.Mutex {
	v, _ := appendLocks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// appendJSONLine 用「一次 Write + O_APPEND」原子追加一行 JSON。
//
// 单次 write 到 O_APPEND 文件在本地文件系统上不会被拆行，因此并发写不会互相插入；
// 进程内再用按路径的互斥锁兜底（Windows 上 O_APPEND 的语义不如 POSIX 强）。
func appendJSONLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("evolve: 创建数据目录失败: %w", err)
	}
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("evolve: 打开数据文件失败: %w", err)
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	_, werr := f.Write(buf)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("evolve: 写入数据文件失败: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("evolve: 关闭数据文件失败: %w", cerr)
	}
	return nil
}

// writeFileAtomic 原子写整个文件：先写同目录临时文件再 rename，避免读到半截内容。
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("evolve: 创建数据目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("evolve: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这里是 no-op
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("evolve: 写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("evolve: 设置文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("evolve: 关闭临时文件失败: %w", err)
	}
	if err := replaceFileWithRetry(tmpName, path, os.Rename, time.Sleep, transientReplaceError); err != nil {
		return fmt.Errorf("evolve: 替换文件失败: %w", err)
	}
	return nil
}

// transientReplaceError 只重试 Windows 的权限/共享/文件锁错误。杀软短暂持有文件
// 时替换会返回这些错误；路径不存在、磁盘满等错误没有重试价值。
func transientReplaceError(err error) bool {
	if runtime.GOOS != "windows" || err == nil {
		return false
	}
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == syscall.Errno(32) || errno == syscall.Errno(33))
}

// replaceFileWithRetry 最多尝试五次，总退避上限 150ms。每次仍由 rename 原子替换，
// 不先删除旧目标，也不把最终失败伪装成成功。依赖作为参数传入，单测无需真等待。
func replaceFileWithRetry(source, target string, rename func(string, string) error, pause func(time.Duration), retryable func(error) bool) error {
	backoff := [...]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := rename(source, target)
		if err == nil {
			return nil
		}
		if attempt >= len(backoff) || !retryable(err) {
			return err
		}
		pause(backoff[attempt])
	}
}
