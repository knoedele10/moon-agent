package evolve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// 变更日志：每次**真的改了技能库**都记一条，并提供按批次回滚。
//
// 为什么要它：契约 §4 的固化器会归档重复技能、按候选改技能正文。这些动作
// 都是自动做的，一旦判错，用户需要能撤销——只留一份「固化报告」不够，
// 报告说改了什么，日志才记着「改之前长什么样」。
//
// 存储：Dir/journal.jsonl，一行一条，追加写（0600）。不引入数据库、
// 不做迁移，符合契约铁律「能落文件就不动表」。
const journalFileName = "journal.jsonl"

// 变更种类。
const (
	// JournalArchive 是把一条技能归档（Enabled=false）。
	JournalArchive = "archive"
	// JournalSkill 是写入/覆盖一条技能（候选落地）。
	JournalSkill = "skill"
	// JournalRollback 是「这一批已经回滚过」的标记，防止重复回滚。
	JournalRollback = "rollback"
)

// journalKeepLines 是读取时的上限：单批最多 3 条，200 行足够覆盖最近几十批。
const journalKeepLines = 200

// JournalEntry 是一次技能库变更。
type JournalEntry struct {
	At     time.Time  `json:"at"`
	Batch  string     `json:"batch"` // 同一批（同一次固化）共用一个批次号
	Kind   string     `json:"kind"`  // archive | skill | rollback
	Name   string     `json:"name"`
	Before *SkillItem `json:"before,omitempty"` // 变更前；新建时为 nil
	After  SkillItem  `json:"after"`
}

// Journal 是追加写的变更日志。
type Journal struct {
	Path string
	Now  func() time.Time
}

// NewJournal 在数据目录下建日志（dir 为空时用 DefaultDir()）。
func NewJournal(dir string) *Journal {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	return &Journal{Path: filepath.Join(dir, journalFileName)}
}

func (j *Journal) now() time.Time {
	if j != nil && j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Journal) path() string {
	if j == nil || strings.TrimSpace(j.Path) == "" {
		return filepath.Join(DefaultDir(), journalFileName)
	}
	return j.Path
}

// Append 追加一条变更。时间由 Journal 自己盖（调用方不用关心）。
func (j *Journal) Append(e JournalEntry) error {
	if j == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = j.now()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("evolve: 序列化变更日志失败: %w", err)
	}
	return appendJSONLine(j.path(), raw)
}

// List 返回最近的变更（最多 limit 条，按时间正序）。日志不存在时返回空。
func (j *Journal) List(limit int) ([]JournalEntry, error) {
	if limit <= 0 || limit > journalKeepLines {
		limit = journalKeepLines
	}
	p := j.path()
	lock := lockFor(p)
	lock.Lock()
	defer lock.Unlock()
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("evolve: 读取变更日志失败: %w", err)
	}
	defer f.Close()

	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// 一行坏了不该让整份日志不可读：跳过并继续。
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("evolve: 扫描变更日志失败: %w", err)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// ErrNothingToRollback 表示没有可回滚的批次。
var ErrNothingToRollback = errors.New("evolve: 没有可回滚的变更")

// ErrAlreadyRolledBack 表示最近一批已经回滚过。
var ErrAlreadyRolledBack = errors.New("evolve: 最近一批已经回滚过")

// RollbackLast 回滚最近一批**未回滚过**的改动，返回逐条动作说明。
//
// 语义刻意保守：
//   - 归档 → 恢复成变更前的样子（Enabled 等字段原样写回）；
//   - 新建技能 → 不物理删除（SkillStore 没有删除语义），改为归档并标来源 rolledback；
//   - 覆盖已有技能 → 写回变更前的正文/描述；
//   - 回滚本身也写一条 rollback 标记，重复调用会返回 ErrAlreadyRolledBack。
func RollbackLast(j *Journal, store SkillStore, userID string) ([]string, error) {
	if store == nil {
		return nil, errors.New("evolve: 没有技能库，无法回滚")
	}
	// 与同目录 Curator 共用锁，回滚期间不能插入新的自动变更。
	lock := lockFor(filepath.Join(filepath.Dir(j.path()), ".curation"))
	lock.Lock()
	defer lock.Unlock()
	entries, err := j.List(journalKeepLines)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrNothingToRollback
	}
	last := entries[len(entries)-1]
	if last.Kind == JournalRollback {
		return nil, ErrAlreadyRolledBack
	}
	batch := last.Batch
	var changes []JournalEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Kind == JournalRollback || e.Batch != batch {
			break
		}
		if e.Kind == JournalArchive || e.Kind == JournalSkill {
			changes = append(changes, e)
		}
		if batch == "" {
			break
		} // 无批次老日志只能回滚最后一条。
	}
	// 先验证整批：用户在固化后编辑过技能时，不能以旧日志覆盖新内容。
	simulated := make(map[string]SkillItem)
	for _, e := range changes {
		current, exists := simulated[e.Name]
		if !exists {
			var err error
			current, exists, err = store.Get(userID, e.Name)
			if err != nil {
				return nil, err
			}
		}
		if !exists || !reflect.DeepEqual(current, e.After) {
			return nil, fmt.Errorf("evolve: 技能 %q 在固化后已被修改，未执行回滚", e.Name)
		}
		if e.Before != nil {
			simulated[e.Name] = *e.Before
		}
	}

	applied := make([]string, 0, 4)
	// 倒序回滚：同一技能先写后被覆盖时，先恢复后写的再恢复先写的。
	for _, e := range changes {
		switch e.Kind {
		case JournalArchive, JournalSkill:
			it := e.After
			if e.Before != nil {
				it = *e.Before
			} else {
				// 新建的技能没有「变更前」：归档它，保留可追溯来源。
				it.Enabled = false
				it.Source = "evolve:rolledback"
			}
			if _, err := store.Upsert(userID, it); err != nil {
				return applied, fmt.Errorf("evolve: 回滚 %q 失败: %w", e.Name, err)
			}
			action := "restore:" + e.Name
			if e.Before == nil {
				action = "disable:" + e.Name
			}
			applied = append(applied, action)
		}
	}
	if len(applied) == 0 {
		return nil, ErrNothingToRollback
	}
	// 写回滚标记：同一批不会被回滚第二次。
	if err := j.Append(JournalEntry{Batch: batch, Kind: JournalRollback, Name: "*"}); err != nil {
		return applied, fmt.Errorf("evolve: 技能已回滚，但回滚标记写入失败: %w", err)
	}
	return applied, nil
}
