package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/agent/evolve"
	"blog-my/internal/agent/skill"
	"blog-my/internal/model"

	"gorm.io/gorm"
)

// 本文件是「自我进化」的接线层：把 agent/evolve 的纯逻辑装配成
// 「按用户隔离的技能库 + 轨迹记录 + 使用账本 + 结构化记忆 + 后台固化」。
//
// 三条设计原则：
//  1. 默认全关：AGENT_EVOLVE 未配置时这里返回 nil 装配，对话行为与改造前一致。
//  2. 进化不能弄坏对话：记录失败只打日志/降级，绝不阻断 SSE；Curator 只在
//     显式调用（HTTP）或 suggest 档的后台协程里跑。
//  3. 技能库沿用现有 agent_skill 表（不新增迁移），账本/轨迹/记忆落文件。

// hermesDataDir 返回进化数据目录：AGENT_EVOLVE_DIR 优先，否则用户配置目录。
func hermesDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("AGENT_EVOLVE_DIR")); dir != "" {
		return dir
	}
	return evolve.DefaultDir()
}

// hermesUserDataDir 不直接拼接用户输入，也不继承此前混存的全局历史。
func hermesUserDataDir(userID string) string {
	hash := sha256.Sum256([]byte(userID))
	return filepath.Join(hermesDataDir(), "users", fmt.Sprintf("%x", hash[:16]))
}

// hermesSkillStore 用现有 agent_skill 表实现 evolve.SkillStore。
// 不新增表、不改列：技能正文、描述、启用状态都是现成的。
type hermesSkillStore struct{}

func (hermesSkillStore) List(userID string) ([]evolve.SkillItem, error) {
	if model.DB == nil {
		return nil, errSkillsNoDB
	}
	var rows []model.AgentSkill
	if err := model.DB.Where("user_id = ?", userID).Order("enabled desc, name asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]evolve.SkillItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, evolve.SkillItem{
			Name: row.Name, Description: row.Description, Body: row.Body,
			Source: row.Source, Enabled: row.Enabled,
		})
	}
	return out, nil
}

func (hermesSkillStore) Get(userID, name string) (evolve.SkillItem, bool, error) {
	if model.DB == nil {
		return evolve.SkillItem{}, false, errSkillsNoDB
	}
	var row model.AgentSkill
	err := model.DB.Where("user_id = ? AND name = ?", userID, name).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return evolve.SkillItem{}, false, nil
		}
		return evolve.SkillItem{}, false, err
	}
	return evolve.SkillItem{
		Name: row.Name, Description: row.Description, Body: row.Body,
		Source: row.Source, Enabled: row.Enabled,
	}, true, nil
}

func (hermesSkillStore) Upsert(userID string, it evolve.SkillItem) (bool, error) {
	if model.DB == nil {
		return false, errSkillsNoDB
	}
	enabled := it.Enabled
	return model.UpsertSkill(userID, skill.Item{
		Name: it.Name, Description: it.Description, Body: it.Body, Source: it.Source,
	}, &enabled)
}

// hermesEvolve 是一次会话里进化能力的装配对象。nil 表示未启用。
type hermesEvolve struct {
	mode     string
	dir      string
	userID   string
	store    evolve.SkillStore
	ledger   *evolve.Ledger
	memory   *evolve.MemoryFile
	recorder *evolve.Recorder

	mu        sync.Mutex
	run       *evolve.Run
	callStart map[string]time.Time
	callArgs  map[string]string
	skillRuns map[string]bool
	finished  bool
}

// newHermesEvolve 按 AGENT_EVOLVE 装配；返回 nil 表示这一轮不做任何进化动作。
func newHermesEvolve(userID string) *hermesEvolve {
	mode := evolveMode()
	if mode == "off" || strings.TrimSpace(userID) == "" {
		return nil
	}
	dir := hermesUserDataDir(userID)
	level := evolve.ParseLevel(mode)
	e := &hermesEvolve{
		mode: mode, dir: dir, userID: userID,
		store:     hermesSkillStore{},
		ledger:    &evolve.Ledger{Path: filepath.Join(dir, "skill-usage.jsonl")},
		memory:    &evolve.MemoryFile{Path: filepath.Join(dir, "memory.json")},
		recorder:  evolve.RecorderFor(level, dir),
		callStart: make(map[string]time.Time),
		callArgs:  make(map[string]string),
		skillRuns: make(map[string]bool),
	}
	return e
}

// Enabled 表示这一轮是否装配了进化能力。
func (e *hermesEvolve) Enabled() bool { return e != nil }

// PromptBlock 返回要注入系统提示词的内容：结构化记忆 + 自进化使用须知。
// 记忆为空且没有可用技能工具时返回空串（不留痕迹）。
func (e *hermesEvolve) PromptBlock() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	if block := e.memory.Block(); strings.TrimSpace(block) != "" {
		b.WriteString("\n")
		b.WriteString(block)
	}
	b.WriteString("\n【关于自我进化】你可以把这次真正跑通的流程沉淀成技能（skill_write），" +
		"把用户明确说过、以后还用得上的事实写进记忆（memory_note）。" +
		"沉淀前先用 skill_search/skill_list 看有没有同类技能：有就更正它，不要重复新建。" +
		"技能正文要写成「下次照这个做」，命令和路径照实写。不要为了凑数而创建技能。\n")
	return b.String()
}

// Tools 返回进化工具。未启用时返回 nil。
func (e *hermesEvolve) Tools() []agent.Tool {
	if e == nil {
		return nil
	}
	deps := evolve.ToolDeps{
		Store: e.store, Ledger: e.ledger, Memory: e.memory,
		Recorder: e.recorder, UserID: e.userID, Dir: e.dir,
	}
	return []agent.Tool{
		evolve.SkillListTool(deps),
		evolve.SkillStatsTool(deps),
		evolve.SkillLintTool(deps),
		evolve.SkillImproveTool(deps),
		evolve.SkillArchiveTool(deps),
		evolve.MemoryNoteTool(deps),
		evolve.MemoryListTool(deps),
		evolve.MemoryForgetTool(deps),
		evolve.EvolutionReportTool(deps),
	}
}

// BeginRun 开始记录一次运行；meta 里的 RunID 为空时自动生成。
func (e *hermesEvolve) BeginRun(meta evolve.RunMeta) {
	if e == nil || e.recorder == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	meta.UserID = e.userID
	if meta.RunID == "" {
		meta.RunID = meta.SessionID + "-" + time.Now().Format("20060102-150405.000000")
	}
	e.run = e.recorder.Start(meta)
}

// ObserveToolCall 记录一次工具调用的开始时间（用于计算耗时）。
func (e *hermesEvolve) ObserveToolCall(id, name string, args ...string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return
	}
	e.callStart[id] = time.Now()
	if e.callArgs == nil {
		e.callArgs = make(map[string]string)
	}
	if len(args) > 0 {
		e.callArgs[id] = evolve.RedactArguments(args[0])
	}
}

// ObserveToolResult 记录一次工具调用的结果。
func (e *hermesEvolve) ObserveToolResult(id, name string, ok, isError bool, summary string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.run == nil {
		e.mu.Unlock()
		return
	}
	start := e.callStart[id]
	args := e.callArgs[id]
	delete(e.callStart, id)
	delete(e.callArgs, id)
	run := e.run
	e.mu.Unlock()
	var ms int64
	if !start.IsZero() {
		ms = time.Since(start).Milliseconds()
	}
	run.Tool(evolve.ToolCallRecord{
		Name: name, Args: args, OK: ok, IsError: isError, Summary: summary, MS: ms,
	})
	// 读取技能只代表查看；脚本执行才按真实工具结果计入成功/失败。
	if name == "skill_run" && e.ledger != nil {
		var input struct {
			Script string `json:"script"`
		}
		if json.Unmarshal([]byte(args), &input) == nil {
			parts := strings.Split(strings.ReplaceAll(input.Script, "\\", "/"), "/")
			if len(parts) > 1 && parts[0] != "" && parts[0] != "." && parts[0] != ".." {
				e.mu.Lock()
				if e.skillRuns == nil {
					e.skillRuns = make(map[string]bool)
				}
				e.skillRuns[parts[0]] = true
				e.mu.Unlock()
				_ = e.ledger.Record(evolve.SkillUse{Name: parts[0], OK: ok && !isError, Reason: "skill_run 的实际执行结果", At: time.Now()})
			}
		}
	}
}

// RecordSelectedSkills 记录注入技能所关联任务的结局，和脚本执行统计区分来源；
// 等待输入和中断没有充分结局，不判失败；同一技能的脚本已记账则不重复统计。
func (e *hermesEvolve) RecordSelectedSkills(names []string, outcome string) {
	if e == nil || e.ledger == nil || (outcome != "ok" && outcome != "error" && outcome != "limit") {
		return
	}
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.TrimSpace(name)
		e.mu.Lock()
		already := e.skillRuns[name]
		e.mu.Unlock()
		if name == "" || seen[name] || already {
			continue
		}
		seen[name] = true
		_ = e.ledger.Record(evolve.SkillUse{Name: name, OK: outcome == "ok", Reason: "注入技能所关联任务的结局：" + outcome, At: time.Now()})
	}
}

// RecordSkillUse 记一次技能被使用（skill_run / 技能自动注入被采纳时调用）。
// 账本写失败不影响对话。
func (e *hermesEvolve) RecordSkillUse(name string, ok bool) {
	if e == nil || e.ledger == nil || strings.TrimSpace(name) == "" {
		return
	}
	_ = e.ledger.Record(evolve.SkillUse{Name: name, OK: ok, At: time.Now()})
}

// Finish 结束这次运行的轨迹记录。outcome 取 ok/limit/error/interrupted/await_user。
func (e *hermesEvolve) Finish(outcome, note string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.finished || e.run == nil {
		e.mu.Unlock()
		return
	}
	e.finished = true
	run := e.run
	e.mu.Unlock()
	if err := run.Finish(outcome, note); err != nil {
		hermesLogf("进化轨迹写入失败（已忽略）：%v", err)
	}
}

// Curate 跑一次固化：Apply=false 只出报告，true 才真的改技能库。
func (e *hermesEvolve) Curate(ctx context.Context, apply bool) (evolve.Report, error) {
	if e == nil {
		return evolve.Report{}, errEvolveDisabled
	}
	c := &evolve.Curator{
		Store: e.store, Ledger: e.ledger, Recorder: e.recorder,
		Apply: apply, MaxMutates: 3,
		Journal: evolve.NewJournal(e.dir), ManagedOnly: true,
	}
	report, err := c.Run(ctx, e.userID)
	if err == nil && c.LastJournalError() != nil {
		return report, fmt.Errorf("部分技能已修改，但变更日志写入失败，无法保证这批修改可撤销：%w", c.LastJournalError())
	}
	return report, err
}

// LastReport 读取最近一次固化报告。
func (e *hermesEvolve) LastReport() (evolve.Report, bool) {
	if e == nil {
		return evolve.Report{}, false
	}
	rep, ok, err := (&evolve.Curator{}).LastReport(e.dir)
	if err != nil || !ok {
		return evolve.Report{}, false
	}
	return rep, true
}

// hermesEvolveReport 给 HTTP/SSE 用的报告视图。
func hermesEvolveReport(e *hermesEvolve) map[string]any {
	out := map[string]any{"enabled": e.Enabled(), "mode": "off"}
	if e == nil {
		return out
	}
	out["mode"] = e.mode
	if rep, ok := e.LastReport(); ok {
		out["report"] = rep
	}
	return out
}

// CurateAsync 在 suggest 档只报告，auto 档按来源保护与单次修改上限自动固化。
// 用独立 context 与超时：这一轮 SSE 可能已经结束，不能把用户请求的生命周期拖进来。
// backgroundCurationDisabled 只由测试置位：后台固化协程会读全局 model.DB，
// 而部分用例会临时把它设为 nil 来做「数据库不可用」断言，两者并发就是数据竞争。
// 生产路径永远为 false；测试里关掉后台协程，不改变任何业务逻辑。
var backgroundCurationDisabled atomic.Bool

func (e *hermesEvolve) CurateAsync(allowApply ...bool) {
	if e == nil || backgroundCurationDisabled.Load() || (e.mode != "suggest" && e.mode != "auto") {
		return
	}
	apply := len(allowApply) > 0 && allowApply[0] && e.mode == "auto" && isOwnerUser(e.userID)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				hermesLogf("固化协程崩溃（不影响对话）：%v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		rep, err := e.Curate(ctx, apply)
		if err != nil {
			hermesLogf("后台固化失败：%v", err)
			return
		}
		hermesLogf("后台固化完成：轨迹 %d 条，技能 %d 条，重复 %d 对，风险 %d 条，候选 %d 条",
			rep.Runs, rep.Skills, len(rep.Duplicates), len(rep.AtRisk), len(rep.Candidates))
	}()
}

// agentEngineName 返回当前使用的执行器名，写进轨迹便于对比两种引擎。
func agentEngineName() string {
	if agentUsesEino() {
		return "eino"
	}
	return "legacy"
}

// trajectoryOutcome 把一次运行的返回值翻译成轨迹结局。
// limit 必须单独识别：它是「预算触顶」而不是普通错误，评测时两者权重不同。
func trajectoryOutcome(runErr error, needInput string, sawDone bool) (string, string) {
	if runErr != nil {
		var reported *agent.LimitReportError
		if errors.As(runErr, &reported) {
			return "limit", "执行预算触顶"
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return "interrupted", "执行上下文已取消或超时，任务结局尚未确认"
		}
		return "error", clipRunes(runErr.Error(), 300)
	}
	if strings.TrimSpace(needInput) != "" {
		return "await_user", clipRunes(needInput, 200)
	}
	if !sawDone {
		return "interrupted", "循环没有给出 done"
	}
	return "ok", ""
}

// hermesLogf 是最小化的日志缝：进化侧失败不该让对话报错，但也不该完全静默。
func hermesLogf(format string, args ...any) {
	log.Printf("[evolve] "+format, args...)
}

var (
	errSkillsNoDB     = errors.New("技能库未初始化（数据库不可用）")
	errEvolveDisabled = errors.New("自我进化未启用（AGENT_EVOLVE 未配置）")
)
