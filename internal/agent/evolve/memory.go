package evolve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// memoryFileName 是结构化记忆的默认文件名。
	memoryFileName = "memory.json"
	// memoryDefaultMaxEntries / memoryDefaultMaxRunes 是默认容量（契约 §4）。
	memoryDefaultMaxEntries = 200
	memoryDefaultMaxRunes   = 8000
	// memoryDefaultSearchLimit 是 Search 不传 limit 时的默认条数。
	memoryDefaultSearchLimit = 10
)

// MemoryEntry 是一条结构化记忆：一个主题 + 一段话。
//
// 与 RAG 并存：RAG 负责「翻旧对话找相关片段」，这里负责「明确记住一条必须一直记得的事」。
type MemoryEntry struct {
	ID        string    `json:"id"`
	Topic     string    `json:"topic"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Hits      int       `json:"hits,omitempty"`
}

// MemoryFile 是文件化的结构化记忆。整个文件原子读写，条数/字数超限自动裁剪。
type MemoryFile struct {
	// Path 是记忆文件路径。为空时用 DefaultDir()/memory.json。
	Path string
	// MaxEntries 是最大条数，<=0 时取 200。
	MaxEntries int
	// MaxRunes 是注入提示词的 <memory> 块最大字数，<=0 时取 8000。
	MaxRunes int
}

// path 返回解析后的记忆文件路径。
func (m *MemoryFile) path() string {
	if m != nil && strings.TrimSpace(m.Path) != "" {
		return m.Path
	}
	return filepath.Join(DefaultDir(), memoryFileName)
}

// maxEntries 返回生效的最大条数。
func (m *MemoryFile) maxEntries() int {
	if m != nil && m.MaxEntries > 0 {
		return m.MaxEntries
	}
	return memoryDefaultMaxEntries
}

// maxRunes 返回生效的 <memory> 块字数上限。
func (m *MemoryFile) maxRunes() int {
	if m != nil && m.MaxRunes > 0 {
		return m.MaxRunes
	}
	return memoryDefaultMaxRunes
}

// Add 新增一条记忆。规范化后与已有条目相同则视为重复，只刷新时间，不重复写入。
//
// 返回值 created=false 表示「已存在，未新增」。
func (m *MemoryFile) Add(topic, text string) (bool, error) {
	if m == nil {
		return false, errors.New("evolve: 记忆文件未初始化")
	}
	text = strings.TrimSpace(Redact(text))
	if text == "" {
		return false, errors.New("evolve: 记忆内容不能为空")
	}
	topic = strings.TrimSpace(Redact(topic))
	if topic == "" {
		topic = "通用"
	}
	if len([]rune(topic)) > 40 {
		topic = string([]rune(topic)[:40])
	}
	if len([]rune(text)) > 2000 {
		text = string([]rune(text)[:2000])
	}
	lock := lockFor(m.path())
	lock.Lock()
	defer lock.Unlock()

	entries, err := m.read()
	if err != nil {
		return false, err
	}
	norm := normalizeMemoryText(text)
	now := time.Now()
	for i := range entries {
		if normalizeMemoryText(entries[i].Text) != norm {
			continue
		}
		entries[i].UpdatedAt = now
		if topic != "通用" {
			entries[i].Topic = topic
		}
		if err := m.save(entries); err != nil {
			return false, err
		}
		return false, nil
	}
	entries = append(entries, MemoryEntry{
		ID:        memoryID(norm),
		Topic:     topic,
		Text:      text,
		CreatedAt: now,
		UpdatedAt: now,
	})
	entries = trimMemoryEntries(entries, m.maxEntries())
	if err := m.save(entries); err != nil {
		return false, err
	}
	return true, nil
}

// Search 按关键词召回记忆，命中的条目会累加 Hits（用于判断哪些记忆真的有用）。
//
// query 为空时返回最近更新的 limit 条。打分规则：命中 token 数优先，
// 其次整串子串命中，最后按更新时间倒序；结果条数上限 100。
func (m *MemoryFile) Search(query string, limit int) []MemoryEntry {
	if m == nil {
		return nil
	}
	if limit <= 0 {
		limit = memoryDefaultSearchLimit
	}
	if limit > 100 {
		limit = 100
	}
	lock := lockFor(m.path())
	lock.Lock()
	defer lock.Unlock()

	entries, err := m.read()
	if err != nil {
		return nil
	}
	query = strings.ToLower(strings.TrimSpace(query))
	queryTokens := make([]string, 0, 8)
	for tok := range textTokens(query) {
		queryTokens = append(queryTokens, tok)
	}
	sort.Strings(queryTokens)

	type scored struct {
		idx   int
		score int
	}
	ranked := make([]scored, 0, len(entries))
	for i := range entries {
		score := 0
		if query != "" {
			haystack := strings.ToLower(entries[i].Topic + "\n" + entries[i].Text)
			for _, tok := range queryTokens {
				if strings.Contains(haystack, tok) {
					score++
				}
			}
			if strings.Contains(haystack, query) {
				score += len(queryTokens) + 1 // 整串命中权重最高
			}
			if score == 0 {
				continue
			}
		}
		ranked = append(ranked, scored{idx: i, score: score})
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		ea, eb := entries[ranked[a].idx], entries[ranked[b].idx]
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		if !ea.UpdatedAt.Equal(eb.UpdatedAt) {
			return ea.UpdatedAt.After(eb.UpdatedAt)
		}
		return ea.ID < eb.ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]MemoryEntry, 0, len(ranked))
	changed := false
	for _, r := range ranked {
		entries[r.idx].Hits++
		changed = true
		out = append(out, entries[r.idx])
	}
	if changed {
		_ = m.save(entries) // Hits 只是统计，写失败不影响召回结果
	}
	return out
}

// Remove 按 ID 删除一条记忆。返回是否真的删掉了。
func (m *MemoryFile) Remove(id string) (bool, error) {
	if m == nil {
		return false, errors.New("evolve: 记忆文件未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("evolve: 删除记忆时 ID 不能为空")
	}
	lock := lockFor(m.path())
	lock.Lock()
	defer lock.Unlock()

	entries, err := m.read()
	if err != nil {
		return false, err
	}
	kept := make([]MemoryEntry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return false, nil
	}
	if err := m.save(kept); err != nil {
		return false, err
	}
	return true, nil
}

// All 返回全部记忆（按创建时间升序，创建时间相同按 ID 排序）。
func (m *MemoryFile) All() ([]MemoryEntry, error) {
	if m == nil {
		return nil, errors.New("evolve: 记忆文件未初始化")
	}
	entries, err := m.read()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	return entries, nil
}

// Block 生成注入系统提示词的 <memory> 块；没有记忆时返回空串（调用方据此不注入）。
//
// 从最新一条往前装，超过 MaxRunes 字数就停：提示词里每多一个字都是每轮的成本，
// 老记忆挤掉新记忆是更糟的取舍。
func (m *MemoryFile) Block() string {
	if m == nil {
		return ""
	}
	entries, err := m.All()
	if err != nil || len(entries) == 0 {
		return ""
	}
	limit := m.maxRunes()
	lines := make([]string, 0, len(entries))
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		line := fmt.Sprintf("- [%s] %s", e.Topic, strings.TrimSpace(e.Text))
		runes := len([]rune(line))
		if total+runes+1 > limit {
			// 一条都装不下时也至少留一条截断版本，避免「明明有记忆却完全没注入」。
			if len(lines) == 0 {
				line = string([]rune(line)[:limit]) + truncatedMark
				lines = append(lines, line)
			}
			break
		}
		lines = append(lines, line)
		total += runes + 1
	}
	if len(lines) == 0 {
		return ""
	}
	// 反转过一次，这里再转回来：越新的记忆越靠前。
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return "<memory>\n" + strings.Join(lines, "\n") + "\n</memory>\n"
}

// read 读取记忆文件；文件不存在返回空切片。
func (m *MemoryFile) read() ([]MemoryEntry, error) {
	raw, err := os.ReadFile(m.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evolve: 读取记忆文件失败: %w", err)
	}
	var entries []MemoryEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("evolve: 解析记忆文件失败: %w", err)
	}
	out := entries[:0]
	for _, e := range entries {
		if strings.TrimSpace(e.Text) == "" {
			continue // 手工编辑留下的空条目直接忽略
		}
		if e.ID == "" {
			e.ID = memoryID(normalizeMemoryText(e.Text))
		}
		if e.Topic == "" {
			e.Topic = "通用"
		}
		out = append(out, e)
	}
	return out, nil
}

// save 原子写回记忆文件。
func (m *MemoryFile) save(entries []MemoryEntry) error {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("evolve: 序列化记忆失败: %w", err)
	}
	return writeFileAtomic(m.path(), raw, 0o600)
}

// trimMemoryEntries 只保留最近更新的 max 条，按创建时间升序返回。
func trimMemoryEntries(entries []MemoryEntry, max int) []MemoryEntry {
	if max <= 0 || len(entries) <= max {
		return entries
	}
	sorted := append([]MemoryEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].UpdatedAt.Equal(sorted[j].UpdatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].UpdatedAt.After(sorted[j].UpdatedAt)
	})
	sorted = sorted[:max]
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	return sorted
}

// normalizeMemoryText 把记忆文本规范化为去重用的键：小写、空白折叠、去掉首尾标点。
func normalizeMemoryText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if unicode.IsPunct(r) && !strings.ContainsRune("-_.:/", r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// memoryID 用规范化文本的哈希做稳定 ID：同一条记忆重装/重建后仍然是同一个 ID。
func memoryID(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return "mem-" + hex.EncodeToString(sum[:8])
}
