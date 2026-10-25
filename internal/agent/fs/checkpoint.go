package fs

// 文件检查点：在 write/edit/move/delete 真正改动磁盘之前，把旧内容留一份，
// 让模型（或用户让模型）能执行「恢复上一个版本」。
//
// 设计取舍：
//   - 只做**文件级**快照，不做影子 git。够用、跨平台、没有外部进程依赖；
//     代价是不能回滚对话、不能原子回滚一整轮多文件操作。
//   - 存储走用户缓存目录（或 AGENT_CHECKPOINT_DIR），按会话/目标哈希分目录，
//     不把绝对路径写进目录名，避免路径穿越与隐私泄漏。
//   - **best-effort**：检查点失败绝不阻断真正的写操作。用户的任务优先于审计副本。
//   - 单文件上限 5MB、每个目标保留最近 10 份，避免把缓存目录撑爆。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
)

const (
	checkpointMaxBytes     = 5 << 20
	checkpointKeepPerPath  = 10
	checkpointListMaxItems = 20
	checkpointEnvDir       = "AGENT_CHECKPOINT_DIR"
)

// checkpointMeta 是落盘的一份元数据（与 .bak 同名、同目录）。
type checkpointMeta struct {
	ID       string `json:"id"`
	TurnID   string `json:"turn_id,omitempty"` // 同一轮用户请求的改动可整组回滚
	FilePath string `json:"file_path"`         // 展示用路径
	// Target 是绝对路径，供 restore_turn 精确恢复（展示路径可能带 workspace2/ 前缀，
	// 不能直接用来 resolve）。目录权限 0700、文件 0600，只存在本机缓存里。
	Target    string    `json:"target"`
	CreatedAt time.Time `json:"created_at"`
	Size      int       `json:"size"`
}

// turnIDOf 读 Exec 上的轮次标识；旧调用方没有 TurnID 时退化为空串（仍可按会话单文件恢复）。
func turnIDOf(ex *agent.Exec) string {
	if ex == nil {
		return ""
	}
	return strings.TrimSpace(ex.TurnID)
}

// checkpointStore 按「会话 → 目标路径」隔离检查点。
type checkpointStore struct {
	mu sync.Mutex
	// lastNano 保证同一进程内连续保存的 ID 严格递增（Windows 时钟分辨率可能不够细）。
	lastNano int64
}

var checkpoints = &checkpointStore{}

// dir 解析检查点根目录：显式环境变量 > 用户缓存目录 > 系统临时目录。
func (s *checkpointStore) dir() string {
	if custom := strings.TrimSpace(os.Getenv(checkpointEnvDir)); custom != "" {
		return custom
	}
	if base, err := os.UserCacheDir(); err == nil && strings.TrimSpace(base) != "" {
		return filepath.Join(base, "blog-my", "checkpoints")
	}
	return filepath.Join(os.TempDir(), "blog-my-checkpoints")
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

func checkpointTargetDir(root, session, target string) string {
	return filepath.Join(root, hashKey(session), hashKey(filepath.Clean(target)))
}

// save 写入一份检查点；超过上限时静默跳过。返回错误仅供测试观察。
func (s *checkpointStore) save(session, turnID, target, display string, data []byte) error {
	if len(data) > checkpointMaxBytes {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := checkpointTargetDir(s.dir(), session, target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now().UTC()
	// 固定宽度纳秒 ID：既可排序又几乎不会重名；时钟回拨或同纳秒连续写入时
	// 用 lastNano+1 兜底，保证「最新」的判定始终稳定。
	nano := now.UnixNano()
	if nano <= s.lastNano {
		nano = s.lastNano + 1
	}
	s.lastNano = nano
	id := fmt.Sprintf("%019d", nano)
	meta := checkpointMeta{ID: id, TurnID: strings.TrimSpace(turnID), FilePath: display, Target: filepath.Clean(target), CreatedAt: now, Size: len(data)}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), metaRaw, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, id+".bak"), data, 0o600); err != nil {
		_ = os.Remove(filepath.Join(dir, id+".json"))
		return err
	}
	s.pruneLocked(dir)
	return nil
}

// pruneLocked 只保留最近 checkpointKeepPerPath 份（调用方已持锁）。
func (s *checkpointStore) pruneLocked(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".bak"))
	}
	if len(ids) <= checkpointKeepPerPath {
		return
	}
	sort.Strings(ids) // 纳秒时间戳前缀 = 时间顺序
	for _, id := range ids[:len(ids)-checkpointKeepPerPath] {
		_ = os.Remove(filepath.Join(dir, id+".bak"))
		_ = os.Remove(filepath.Join(dir, id+".json"))
	}
}

// readMetasLocked 读取一个目标目录下的全部有效元数据（新→旧）。
func (s *checkpointStore) readMetasLocked(dir string) []checkpointMeta {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []checkpointMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var meta checkpointMeta
		if json.Unmarshal(raw, &meta) != nil || meta.ID == "" {
			continue
		}
		if _, serr := os.Stat(filepath.Join(dir, meta.ID+".bak")); serr != nil {
			continue // 元数据在、正文丢了：不把它算作可恢复版本
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// list 返回某个路径的检查点（新→旧），最多 checkpointListMaxItems 条。
func (s *checkpointStore) list(session, target string) []checkpointMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.readMetasLocked(checkpointTargetDir(s.dir(), session, target))
	if len(out) > checkpointListMaxItems {
		out = out[:checkpointListMaxItems]
	}
	return out
}

// latestTurn 返回该会话最近一次留下检查点的轮次，以及这一轮的全部检查点。
//
// 一次用户请求可能改多个文件；按 TurnID 分组后，restore_turn 才能整组撤回。
// 没有 TurnID 的旧检查点不参与（避免把历史无关文件一起回滚）。
func (s *checkpointStore) latestTurn(session string) (string, []checkpointMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionDir := filepath.Join(s.dir(), hashKey(session))
	dirs, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", nil
	}
	var all []checkpointMeta
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		all = append(all, s.readMetasLocked(filepath.Join(sessionDir, d.Name()))...)
	}
	// 没有 TurnID 的旧检查点不参与整轮回滚，也不能挡住有 TurnID 的轮次。
	withTurn := make([]checkpointMeta, 0, len(all))
	for _, m := range all {
		if m.TurnID != "" {
			withTurn = append(withTurn, m)
		}
	}
	if len(withTurn) == 0 {
		return "", nil
	}
	sort.Slice(withTurn, func(i, j int) bool { return withTurn[i].ID > withTurn[j].ID })
	turnID := withTurn[0].TurnID
	out := make([]checkpointMeta, 0, len(withTurn))
	for _, m := range withTurn {
		if m.TurnID == turnID {
			out = append(out, m)
		}
	}
	return turnID, out
}

// latest 读取最近一份检查点正文。
func (s *checkpointStore) latest(session, target string) (checkpointMeta, []byte, bool) {
	metas := s.list(session, target)
	if len(metas) == 0 {
		return checkpointMeta{}, nil, false
	}
	meta := metas[0]
	dir := checkpointTargetDir(s.dir(), session, target)
	data, err := os.ReadFile(filepath.Join(dir, meta.ID+".bak"))
	if err != nil {
		return checkpointMeta{}, nil, false
	}
	return meta, data, true
}

// data 读取指定检查点的正文。
func (s *checkpointStore) data(session string, meta checkpointMeta) ([]byte, bool) {
	dir := checkpointTargetDir(s.dir(), session, meta.Target)
	data, err := os.ReadFile(filepath.Join(dir, meta.ID+".bak"))
	if err != nil {
		return nil, false
	}
	return data, true
}

// saveCheckpointForTarget 是写类工具的统一入口：目标存在、是普通文件、没超限时留一份。
// 任何失败都吞掉：检查点不能影响真正的写操作。
func saveCheckpointForTarget(ex *agent.Exec, target, display string) {
	fi, err := os.Stat(target)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > checkpointMaxBytes {
		return
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return
	}
	_ = checkpoints.save(sessionOf(ex), turnIDOf(ex), target, display, data)
}

// ---------- restore ----------

// RestoreTool 把文件恢复到最近一次修改前的检查点。
//
// 它是一个**写工具**：confirm 档会先问用户；plan 档不注册。
// 恢复后不要求先 read（这正是它的用途），但仍走工作区沙箱与原子写。
type RestoreTool struct{}

func (RestoreTool) Name() string { return "restore" }

func (RestoreTool) Description() string {
	return "把工作区里的一个文件恢复到最近一次检查点。write/edit/move/delete 在改动前会自动留一份副本；" +
		"删除或改错文件后用它撤回。没有检查点时明确报错。恢复后建议 read 核对内容。"
}

func (RestoreTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "要恢复的文件路径（相对根目录或根内绝对路径）"},
		},
		"required": []string{"file_path"},
	}
}

func (RestoreTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a struct {
		FilePath string `json:"file_path"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}
	meta, data, ok := checkpoints.latest(sessionOf(ex), target)
	if !ok {
		return agent.Result{}, &Error{
			Code: CodeNotFound, Path: display,
			Reason: fmt.Sprintf("cannot restore \"%s\": no checkpoint found — only files changed by write/edit/move/delete keep a checkpoint", display),
		}
	}

	ver, _, err := restoreFileFromCheckpoint(ex, target, display, data)
	if err != nil {
		return agent.Result{}, err
	}

	preview, shown, total, truncated := numberLines(string(data), 1, 20)
	if total == 0 {
		preview = "(文件为空)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已恢复 %s 到检查点 %s（%s，%d 字节）\n", display, meta.ID, meta.CreatedAt.Local().Format("2006-01-02 15:04:05"), len(data))
	if shown > 0 {
		b.WriteString("恢复后的前几行：\n")
		b.WriteString(preview)
		if truncated {
			b.WriteString("…（还有更多内容，请 read 核对）\n")
		}
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{
			"file_path": display, "checkpoint": meta.ID,
			"restored_at": meta.CreatedAt.Format(time.RFC3339),
			"bytes":       len(data), "version": ver,
		},
	}, nil
}

// restoreFileFromCheckpoint 把一份检查点正文原子写回目标，并更新观察表。
func restoreFileFromCheckpoint(ex *agent.Exec, target, display string, data []byte) (string, bool, error) {
	exists := false
	if fi, statErr := os.Stat(target); statErr == nil {
		if !fi.Mode().IsRegular() {
			return "", false, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备）"}
		}
		exists = true
	} else if !os.IsNotExist(statErr) {
		return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(statErr, target, display)}
	}

	if !exists && ex != nil && IsWithinWorkspaces(ex.Root, ex.Roots, filepath.Dir(target)) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", false, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
		}
	}

	ver, created, err := writeFileAtomic(target, display, data, !exists)
	if err != nil {
		return "", false, err
	}
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: ver})
	return ver, created, nil
}

// RestoreTurnTool 撤销**最近一轮**由 write/edit/move/delete 造成的文件改动。
//
// 与 restore 的区别：restore 要一个路径、恢复它最近一次修改前的内容；
// restore_turn 不需要路径，按 TurnID 把这一轮碰过的所有文件一起恢复。
// 只回滚文件，不回滚对话；本轮新建的文件没有「改动前」版本，因此不会被删除。
type RestoreTurnTool struct{}

func (RestoreTurnTool) Name() string { return "restore_turn" }

func (RestoreTurnTool) Description() string {
	return "撤销最近一轮已经执行的文件改动（整组回滚）。不需要路径：会把这一轮 write/edit/move/delete " +
		"碰过的文件恢复到改动前的内容。只回滚文件，不回滚对话；本轮新建的文件没有旧版本，不会被删除。" +
		"撤回单个文件请用 restore。"
}

func (RestoreTurnTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (RestoreTurnTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	session := sessionOf(ex)
	turnID, metas := checkpoints.latestTurn(session)
	if len(metas) == 0 {
		return agent.Result{
			IsError: true,
			Content: "没有可回滚的文件改动：write/edit/move/delete 之前才会自动留检查点；本轮可能只是读取或新建文件。",
			Display: map[string]any{"code": "NO_CHECKPOINT", "restored": 0},
		}, nil
	}

	// 同一文件一轮内改多次时，取**最早**一份检查点，才是这一轮开始前的状态。
	byTarget := make(map[string]checkpointMeta, len(metas))
	for _, m := range metas {
		if strings.TrimSpace(m.Target) == "" {
			continue
		}
		if existing, ok := byTarget[m.Target]; !ok || m.ID < existing.ID {
			byTarget[m.Target] = m
		}
	}
	targets := make([]string, 0, len(byTarget))
	for target := range byTarget {
		targets = append(targets, target)
	}
	sort.Strings(targets)

	allowOutside := ex != nil && ex.AllowOutside
	var restored, skipped []string
	for _, target := range targets {
		meta := byTarget[target]
		if !allowOutside && (ex == nil || !IsWithinWorkspaces(ex.Root, ex.Roots, target)) {
			skipped = append(skipped, meta.FilePath+"（不在活动工作区内）")
			continue
		}
		data, ok := checkpoints.data(session, meta)
		if !ok {
			skipped = append(skipped, meta.FilePath+"（检查点正文缺失）")
			continue
		}
		if _, _, err := restoreFileFromCheckpoint(ex, target, meta.FilePath, data); err != nil {
			skipped = append(skipped, meta.FilePath+"（"+err.Error()+"）")
			continue
		}
		restored = append(restored, meta.FilePath)
	}

	if len(restored) == 0 {
		return agent.Result{
			IsError: true,
			Content: "本轮有检查点，但没有文件被恢复：" + strings.Join(skipped, "；"),
			Display: map[string]any{"code": "RESTORE_TURN_FAILED", "turn_id": turnID, "restored": 0, "skipped": skipped},
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "已回滚最近一轮（turn=%s）的 %d 个文件：\n", turnID, len(restored))
	for _, path := range restored {
		b.WriteString("- " + path + "\n")
	}
	if len(skipped) > 0 {
		b.WriteString("未恢复：\n")
		for _, item := range skipped {
			b.WriteString("- " + item + "\n")
		}
	}
	b.WriteString("只回滚了文件内容，对话记录未回滚；恢复后建议 read 或用命令重新验证。")
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{
			"turn_id": turnID, "restored": len(restored), "skipped": skipped,
		},
	}, nil
}

// RecoveryTools 返回恢复类工具（web 接线层用）。
func RecoveryTools() []agent.Tool { return []agent.Tool{RestoreTool{}, RestoreTurnTool{}} }
