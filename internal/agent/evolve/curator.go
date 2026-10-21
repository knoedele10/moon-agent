package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// curatorLookbackDays 是固化时回看的轨迹天数：太短没有统计意义，太长会把
	// 早已过时的经验重新捞出来。
	curatorLookbackDays = 7
	// curatorDefaultMaxMutates 是单次固化默认最多改几条（契约 §4 默认 3）。
	curatorDefaultMaxMutates = 3
	// curatorAtRiskMinUses / curatorAtRiskLimit 是报告里「该关注技能」的门槛与上限。
	curatorAtRiskMinUses = 3
	curatorAtRiskLimit   = 10
	// curatorDuplicateThreshold 是技能去重的 Jaccard 阈值。
	curatorDuplicateThreshold = 0.8
	// curatorCandidateMax 是单次报告最多带多少条候选（去重后）。
	curatorCandidateMax = 20
)

// Report 是一次后台固化的结果：先给证据，再列改动。
type Report struct {
	At         time.Time   `json:"at"`
	Runs       int         `json:"runs"`
	Skills     int         `json:"skills"`
	Duplicates [][2]string `json:"duplicates,omitempty"`
	AtRisk     []SkillStat `json:"at_risk,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Applied    []string    `json:"applied,omitempty"`
}

// Curator 是后台固化器：读轨迹 + 账本 + 技能库，产出一份报告；
// Apply=true 时按报告改技能库（受 MaxMutates 限制）。
//
// 安全默认：Apply=false（只建议不落库）。重复运行必须幂等——
// 已在上一轮归档的重复技能、已与候选正文一致的技能都不会被再改一次。
type Curator struct {
	// Store 是技能库；为 nil 时只做轨迹/账本侧的分析与报告。
	Store SkillStore
	// Ledger 是技能使用账本；可为 nil。
	Ledger *Ledger
	// Recorder 是轨迹记录器；可为 nil。
	Recorder *Recorder
	// Apply 为 false 时只报告（suggest），true 时真的落库（auto）。
	Apply bool
	// MaxMutates 是单次固化最多改几条，<=0 时取默认 3。
	MaxMutates int
	// Journal 是变更日志；非 nil 时每次真的改库都记一条，可据此回滚。
	Journal *Journal
	// ManagedOnly 保护用户创作和导入技能，自动固化只维护 Source=evolve 的条目。
	ManagedOnly bool

	journalMu  sync.Mutex
	journalErr error
}

// Run 执行一次固化。ctx 只用于提前取消（长列表读取时能及时退出）。
func (c *Curator) Run(ctx context.Context, userID string) (Report, error) {
	var rep Report
	if c == nil {
		return rep, errors.New("evolve: Curator 未初始化")
	}
	// 同一用户的前台请求和后台固化串行，避免重复创建和相互覆盖报告。
	if dir := c.dataDir(); dir != "" {
		lock := lockFor(filepath.Join(dir, ".curation"))
		lock.Lock()
		defer lock.Unlock()
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		default:
		}
	}
	rep.At = c.now()

	var runs []RunRecord
	if c.Recorder != nil {
		got, err := c.Recorder.List(curatorLookbackDays)
		if err != nil {
			return rep, err
		}
		for _, run := range got {
			if run.UserID == "" || run.UserID == userID {
				runs = append(runs, run)
			}
		}
	}
	rep.Runs = len(runs)

	var items []SkillItem
	if c.Store != nil {
		got, err := c.Store.List(userID)
		if err != nil {
			return rep, fmt.Errorf("evolve: 读取技能库失败: %w", err)
		}
		items = got
	}
	rep.Skills = len(items)
	rep.Duplicates = Duplicates(items, curatorDuplicateThreshold)

	if c.Ledger != nil {
		if stats, err := c.Ledger.AtRisk(curatorAtRiskMinUses, curatorAtRiskLimit); err == nil {
			rep.AtRisk = stats
		}
	}
	rep.Candidates = collectCandidates(runs)

	if c.Apply && c.Store != nil {
		applied, err := c.apply(userID, items, rep.Duplicates, rep.Candidates, rep.At.Format(time.RFC3339Nano))
		if err != nil {
			// 已经改成功的条目照实留在报告里，方便排查「改了一半」。
			rep.Applied = applied
			return rep, err
		}
		rep.Applied = applied
	}

	if dir := c.dataDir(); dir != "" {
		if err := writeReport(dir, rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// LastReport 读最近一次固化报告（Dir/last-report.json）。第二个返回值表示报告是否存在。
func (c *Curator) LastReport(dir string) (Report, bool, error) {
	var rep Report
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	raw, err := os.ReadFile(filepath.Join(dir, reportFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return rep, false, nil
	}
	if err != nil {
		return rep, false, fmt.Errorf("evolve: 读取固化报告失败: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, false, fmt.Errorf("evolve: 解析固化报告失败: %w", err)
	}
	return rep, true, nil
}

// now 返回当前时间（优先用 Recorder 注入的时钟，单测里可固定）。
func (c *Curator) now() time.Time {
	if c != nil && c.Recorder != nil && c.Recorder.Now != nil {
		return c.Recorder.Now()
	}
	return time.Now()
}

// dataDir 返回报告落盘目录：优先轨迹目录（它与轨迹同源），其次账本所在目录。
func (c *Curator) dataDir() string {
	if c.Recorder != nil {
		return c.Recorder.dir()
	}
	if c.Ledger != nil && strings.TrimSpace(c.Ledger.Path) != "" {
		return filepath.Dir(c.Ledger.Path)
	}
	return ""
}

// apply 真正改技能库，返回改动摘要（供报告与幂等校验用）。
func (c *Curator) apply(userID string, items []SkillItem, duplicates [][2]string, candidates []Candidate, batches ...string) ([]string, error) {
	batch := c.now().Format(time.RFC3339Nano)
	if len(batches) > 0 && batches[0] != "" {
		batch = batches[0]
	}
	maxMutates := c.MaxMutates
	if maxMutates <= 0 {
		maxMutates = curatorDefaultMaxMutates
	}
	applied := make([]string, 0, maxMutates)
	itemsByName := make(map[string]SkillItem, len(items))
	for _, it := range items {
		itemsByName[it.Name] = it
	}
	// 1) 先归档重复技能：保留「使用次数更多」的那条，同分时按名字保留字典序小的，
	//    保证同一份技能库重复运行得到同样的结论。
	for _, pair := range duplicates {
		if len(applied) >= maxMutates {
			break
		}
		// 词面相近只值得报告。不同参数的技能会共享大量模板正文，不能据此
		// 自动归档；只有正文一致的副本才可确定地去重。
		if strings.TrimSpace(itemsByName[pair[0]].Body) != strings.TrimSpace(itemsByName[pair[1]].Body) {
			continue
		}
		victim, ok := pickDuplicateVictim(c.Ledger, itemsByName[pair[0]], itemsByName[pair[1]])
		if !ok {
			continue
		}
		it := itemsByName[victim]
		if HasError(Dangerous(it.Body + "\n" + it.Description)) {
			continue // 不把已有危险内容或凭证复制进自动变更日志。
		}
		if c.ManagedOnly && it.Source != "evolve" {
			continue
		}
		if !it.Enabled {
			continue // 已经归档过：幂等跳过
		}
		before := it
		it.Enabled = false
		it.Source = firstNonEmpty(it.Source, "evolve")
		if _, err := c.Store.Upsert(userID, it); err != nil {
			return applied, fmt.Errorf("evolve: 归档重复技能 %q 失败: %w", victim, err)
		}
		c.logChange(JournalEntry{Batch: batch, Kind: JournalArchive, Name: victim, Before: &before, After: it})
		applied = append(applied, "archive:"+victim)
		itemsByName[victim] = it
	}
	// 2) 再应用技能候选：正文与描述都没变就跳过（幂等）；有 error 级体检问题不落库。
	for _, cand := range candidates {
		if len(applied) >= maxMutates {
			break
		}
		if cand.Kind != "skill" || strings.TrimSpace(cand.Name) == "" || !cand.AutoApplicable {
			continue
		}
		existing, exists, err := c.Store.Get(userID, cand.Name)
		if err != nil {
			return applied, fmt.Errorf("evolve: 读取技能 %q 失败: %w", cand.Name, err)
		}
		if exists {
			if c.ManagedOnly && existing.Source != "evolve" {
				continue
			}
			if !existing.Enabled {
				continue // 已被归档的技能不再自动复活
			}
			if existing.Body == cand.Body && existing.Description == cand.Description {
				continue // 上一轮已经写过同样的内容
			}
		}
		var before *SkillItem
		if exists {
			b := existing
			before = &b
		}
		it := SkillItem{
			Name:        cand.Name,
			Description: cand.Description,
			Body:        cand.Body,
			Source:      "evolve",
			Enabled:     true,
		}
		if HasError(Lint(it)) {
			continue // 候选自身体检不过（比如正文含危险命令）就不落地
		}
		if _, err := c.Store.Upsert(userID, it); err != nil {
			return applied, fmt.Errorf("evolve: 写入技能候选 %q 失败: %w", cand.Name, err)
		}
		c.logChange(JournalEntry{Batch: batch, Kind: JournalSkill, Name: cand.Name, Before: before, After: it})
		applied = append(applied, "skill:"+cand.Name)
	}
	sort.Strings(applied)
	return applied, nil
}

// logChange 记一条变更日志。
//
// 写日志失败**不**回滚已经完成的技能库改动、也不返回错误：改库已经发生了，
// 报错只会让调用方误以为没改。失败信息留在 LastJournalError()，状态接口能看见。
func (c *Curator) logChange(e JournalEntry) {
	if c == nil || c.Journal == nil {
		return
	}
	if err := c.Journal.Append(e); err != nil {
		c.journalMu.Lock()
		c.journalErr = err
		c.journalMu.Unlock()
	}
}

// LastJournalError 返回最近一次变更日志写入错误（没有则 nil）。
func (c *Curator) LastJournalError() error {
	if c == nil {
		return nil
	}
	c.journalMu.Lock()
	defer c.journalMu.Unlock()
	return c.journalErr
}

// pickDuplicateVictim 决定一对重复技能里该归档哪一条。
//
// 返回 false 表示这对不处理：任一条已停用（说明已经处理过），或名字相同（脏数据）。
func pickDuplicateVictim(ledger *Ledger, a, b SkillItem) (string, bool) {
	if a.Enabled != b.Enabled || !a.Enabled || a.Name == b.Name {
		return "", false
	}
	usesOf := func(name string) int {
		if ledger == nil {
			return 0
		}
		st, ok, err := ledger.Stat(name)
		if err != nil || !ok {
			return 0
		}
		return st.Uses
	}
	ua, ub := usesOf(a.Name), usesOf(b.Name)
	switch {
	case ua < ub:
		return a.Name, true
	case ub < ua:
		return b.Name, true
	case a.Name > b.Name:
		return a.Name, true // 同分：归档字典序大的，保留更早出现的名字
	default:
		return b.Name, true
	}
}

// collectCandidates 把多条轨迹的候选合并去重（同 Kind+Name 只留置信度最高的一条）。
func collectCandidates(runs []RunRecord) []Candidate {
	byKey := map[string]Candidate{}
	order := make([]string, 0, len(runs))
	for _, run := range runs {
		for _, cand := range Candidates(run) {
			key := cand.Kind + "\x00" + cand.Name
			cur, ok := byKey[key]
			if !ok {
				order = append(order, key)
				byKey[key] = cand
				continue
			}
			if cand.Confidence > cur.Confidence {
				byKey[key] = cand
			}
		}
	}
	for _, cand := range CrossRunCandidates(runs) {
		key := cand.Kind + "\x00" + cand.Name
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
			byKey[key] = cand
		}
	}
	out := make([]Candidate, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > curatorCandidateMax {
		out = out[:curatorCandidateMax]
	}
	return out
}

// writeReport 原子写报告文件（界面/接口读它，不能读到半截 JSON）。
func writeReport(dir string, rep Report) error {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("evolve: 序列化固化报告失败: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, reportFileName), raw, 0o600); err != nil {
		return fmt.Errorf("evolve: 写固化报告失败: %w", err)
	}
	return nil
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
