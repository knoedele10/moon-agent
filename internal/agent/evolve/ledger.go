package evolve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ledgerFileName 是技能使用账本的默认文件名（JSONL，一行一次使用）。
	ledgerFileName = "ledger.jsonl"
	// ledgerStaleDays：超过这么多天没用过的技能进入「该归档」名单。
	ledgerStaleDays = 30
	// ledgerFailRate：使用次数达到门槛后，成功率低于它进入「该修」名单。
	ledgerFailRate = 0.6
)

// SkillUse 是一次技能被使用（或被尝试使用）的记录。
type SkillUse struct {
	Name   string    `json:"name"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// SkillStat 是一条技能的聚合使用统计。
type SkillStat struct {
	Name        string    `json:"name"`
	Uses        int       `json:"uses"`
	OK          int       `json:"ok"`
	Fail        int       `json:"fail"`
	LastUsed    time.Time `json:"last_used"`
	SuccessRate float64   `json:"success_rate"`
}

// Ledger 是技能使用账本：追加写 JSONL，读取时聚合。
//
// 为什么不做随机写：账本是旁路数据，追加最便宜也最抗崩溃（写坏一行只丢一条）。
type Ledger struct {
	// Path 是账本文件路径。为空时用 DefaultDir()/ledger.jsonl。
	Path string
}

// path 返回解析后的账本文件路径。
func (l *Ledger) path() string {
	if l != nil && strings.TrimSpace(l.Path) != "" {
		return l.Path
	}
	return filepath.Join(DefaultDir(), ledgerFileName)
}

// Record 追加一次技能使用记录。Name 为空时报错（没有名字的统计没有意义）。
func (l *Ledger) Record(u SkillUse) error {
	if l == nil {
		return errors.New("evolve: 技能账本未初始化")
	}
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return errors.New("evolve: 技能使用记录缺少技能名")
	}
	if u.At.IsZero() {
		u.At = time.Now()
	}
	line, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("evolve: 序列化技能使用记录失败: %w", err)
	}
	return appendJSONLine(l.path(), line)
}

// Stats 返回全部技能的聚合统计，按技能名排序。账本不存在时返回空切片而不是错误。
func (l *Ledger) Stats() ([]SkillStat, error) {
	uses, err := l.readUses()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*SkillStat, len(uses))
	order := make([]string, 0, len(uses))
	for _, u := range uses {
		st := byName[u.Name]
		if st == nil {
			st = &SkillStat{Name: u.Name}
			byName[u.Name] = st
			order = append(order, u.Name)
		}
		st.Uses++
		if u.OK {
			st.OK++
		} else {
			st.Fail++
		}
		if u.At.After(st.LastUsed) {
			st.LastUsed = u.At
		}
	}
	out := make([]SkillStat, 0, len(order))
	for _, name := range order {
		st := byName[name]
		if st.Uses > 0 {
			st.SuccessRate = float64(st.OK) / float64(st.Uses)
		}
		out = append(out, *st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stat 查单条技能的统计。第二个返回值表示「账本里有这条技能的使用记录」。
func (l *Ledger) Stat(name string) (SkillStat, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return SkillStat{}, false, errors.New("evolve: 查询技能统计时技能名不能为空")
	}
	stats, err := l.Stats()
	if err != nil {
		return SkillStat{}, false, err
	}
	for _, st := range stats {
		if st.Name == name {
			return st, true, nil
		}
	}
	return SkillStat{}, false, nil
}

// AtRisk 返回「该关注」的技能：失败率高或久未使用。
//
// minUses 是失败率统计的最低使用次数（<=0 时取 3）；limit 是返回上限（<=0 不限）。
// 排序稳定：先用成功率升序（最该修的在前），成功率相同按最近使用时间升序。
func (l *Ledger) AtRisk(minUses int, limit int) ([]SkillStat, error) {
	stats, err := l.Stats()
	if err != nil {
		return nil, err
	}
	if minUses <= 0 {
		minUses = 3
	}
	now := time.Now()
	staleBefore := now.AddDate(0, 0, -ledgerStaleDays)
	out := make([]SkillStat, 0, len(stats))
	for _, st := range stats {
		highFail := st.Uses >= minUses && st.SuccessRate < ledgerFailRate
		stale := !st.LastUsed.IsZero() && st.LastUsed.Before(staleBefore)
		if highFail || stale {
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SuccessRate != out[j].SuccessRate {
			return out[i].SuccessRate < out[j].SuccessRate
		}
		return out[i].LastUsed.Before(out[j].LastUsed)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readUses 读账本里的全部使用记录，坏行跳过。
func (l *Ledger) readUses() ([]SkillUse, error) {
	path := l.path()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evolve: 打开技能账本失败: %w", err)
	}
	defer f.Close()
	lines, err := readValidJSONLines(path)
	if err != nil {
		return nil, err
	}
	out := make([]SkillUse, 0, len(lines))
	for _, line := range lines {
		var u SkillUse
		if err := json.Unmarshal(line, &u); err != nil || strings.TrimSpace(u.Name) == "" {
			continue
		}
		u.Name = strings.TrimSpace(u.Name)
		out = append(out, u)
	}
	return out, nil
}
