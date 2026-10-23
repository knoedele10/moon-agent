// Package toolsys 是「工具编排层」：声明式工具集目录、渐进披露（tool_search）、
// 参数健壮化装饰器、大结果落盘与只读工具并行批处理。
//
// 设计边界（契约 docs/agent/11-hermes-parity.md §3）：
//   - 本包只做工具编排与执行辅助，不认识 HTTP/SSE，也不改主循环；
//   - 工具集是**声明式**的：一次装配给出全量目录，运行期再按需激活；
//   - 所有导出类型与方法都并发安全，可以同时被主循环、HTTP 层与后台任务使用。
package toolsys

import (
	"sort"
	"strings"
	"sync"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

const (
	// defaultSearchLimit 是 Catalog.Search 的默认条数（tool_search 缺省也是 5）。
	defaultSearchLimit = 5
	// maxSearchLimit 是 tool_search 的硬上限：一次给模型太多候选反而不会用。
	maxSearchLimit = 12
	// coreToolsetName 是常驻工具集的保留名：NewCatalog 会自动激活它。
	coreToolsetName = "core"
)

// normalizeRisk 将空值/未知值按写入处理，避免未声明的外部副作用被误判成可并发的只读操作。
func normalizeRisk(r Risk) Risk {
	if r.Valid() {
		return r
	}
	return RiskWrite
}

// RiskAnnotated 是工具的可选接口：实现它就能覆盖所属 toolset 的默认风险。
//
// 契约 §3 的 Toolset.Tools 只接收 agent.Tool，单工具覆盖没有别的入口；
// 用可选接口而不是扩 interface，是为了不改动已冻结的 agent.Tool。
type RiskAnnotated interface {
	Risk() Risk
}

// TagsAnnotated 是工具的可选接口：给 tool_search 补中文别名（如 read→["查看","打开","文件"]）。
type TagsAnnotated interface {
	Tags() []string
}

// Toolset 是一组工具的声明式分组。
type Toolset struct {
	Name    string       // 如 "fs"、"memory"、"evolve"
	Desc    string       // 一句话，进提示词的工具集清单
	Depends []string     // 依赖的其它 toolset（激活本组时会自动带上）
	Risk    Risk         // 组标签（提示词/HTTP）。单工具风险见 RiskAnnotated，否则 LookupMeta
	Tools   []agent.Tool // 组内工具，顺序即暴露给模型的顺序
}

// Entry 是目录里的一个工具。
type Entry struct {
	Tool    agent.Tool // 已套参数装饰器的工具（Sanitize/Coerce/Validate）
	Toolset string
	Risk    Risk
	Tags    []string // 检索用别名
}

// Match 是 Catalog.Search 的一条命中。
type Match struct {
	Name        string `json:"name"`
	Toolset     string `json:"toolset"`
	Description string `json:"description"` // 一句话摘要，给模型看
	Risk        Risk   `json:"risk"`
	Active      bool   `json:"active"` // 是否已激活（激活后下一轮可直接调用）
	Score       int    `json:"score"`  // 越大越相关；同分按声明顺序稳定
}

// ToolsetInfo 是工具集的对外描述（提示词清单与 HTTP 接口共用）。
type ToolsetInfo struct {
	Name      string   `json:"name"`
	Desc      string   `json:"desc"`
	Risk      Risk     `json:"risk"`
	Depends   []string `json:"depends,omitempty"`
	Tools     []string `json:"tools"`     // 组内全部工具名（声明顺序）
	Active    int      `json:"active"`    // 已激活的工具数
	Activated bool     `json:"activated"` // 整组是否都已激活
}

// Catalog 是运行期工具目录：全量工具 + 已激活集合。
//
// 默认只有名为 "core" 的工具集是激活的（见 coreToolsetName）；
// 其它工具集要通过 Activate/ActivateToolset 显式打开——这就是渐进披露的落点。
// 空目录也可以用：Defs 返回空切片，Lookup 永远 false。
type Catalog struct {
	mu    sync.RWMutex
	reg   *agent.Registry
	sets  []Toolset
	order []string // 全量工具名，toolset 声明顺序 + 组内顺序
	// entries 用工具名索引；同名工具先声明者优先（后续重复项静默忽略）。
	entries map[string]Entry
	active  map[string]bool
	// activeOrder 记录激活顺序，Defs 按它输出（保证提示词顺序稳定）。
	activeOrder []string
}

// NewCatalog 按声明顺序登记工具集，返回一个运行期目录。
//
// 每个工具会自动套上 Sanitize → Coerce → Validate 三层装饰器：
// 交给模型的 schema 已净化，执行前会先纠偏再预校验。接线层不需要重复包装。
// 工具集或工具的重名、空名字、nil 工具会被静默跳过（契约签名没有 error 返回）。
func NewCatalog(sets ...Toolset) *Catalog {
	c := &Catalog{
		reg:     agent.NewRegistry(),
		sets:    make([]Toolset, 0, len(sets)),
		entries: make(map[string]Entry, len(sets)),
		active:  make(map[string]bool, len(sets)),
	}
	for _, raw := range sets {
		set := raw
		set.Name = strings.TrimSpace(set.Name)
		if set.Name != "" && c.hasSet(set.Name) {
			continue // 同名工具集：先声明者优先
		}
		kept := make([]agent.Tool, 0, len(set.Tools))
		for _, t := range set.Tools {
			if t == nil {
				continue
			}
			name := strings.TrimSpace(t.Name())
			if name == "" {
				continue
			}
			if _, dup := c.entries[name]; dup {
				continue
			}
			decorated := Decorate(t)
			c.entries[name] = Entry{
				Tool:    decorated,
				Toolset: set.Name,
				Risk:    riskOf(t, set.Risk),
				Tags:    tagsOf(t),
			}
			c.order = append(c.order, name)
			kept = append(kept, decorated)
			_ = c.reg.Register(decorated) // 名字已预过滤，这里不会失败
		}
		set.Tools = kept
		set.Risk = normalizeRisk(set.Risk)
		c.sets = append(c.sets, set)
	}
	// 核心集常驻：没有它模型连 tool_search 都调不到。
	if c.hasSet(coreToolsetName) {
		c.activateToolsetLocked([]string{coreToolsetName})
	}
	return c
}

// hasSet 判断工具集是否存在（构造期调用，无锁）。
func (c *Catalog) hasSet(name string) bool {
	for _, set := range c.sets {
		if set.Name == name {
			return true
		}
	}
	return false
}

// Registry 返回交给循环执行用的注册表（工具已装饰、按声明顺序登记）。
func (c *Catalog) Registry() *agent.Registry {
	if c == nil {
		return nil
	}
	return c.reg
}

// Defs 返回交给模型的工具定义，只含已激活的工具。
//
// 顺序 = 激活顺序优先、toolset 声明顺序次之；这个顺序必须稳定，
// 否则每轮提示词都变，上游的提示词缓存会全部失效。
func (c *Catalog) Defs() []llm.ToolDef {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]llm.ToolDef, 0, len(c.activeOrder))
	for _, name := range c.activeOrder {
		entry, ok := c.entries[name]
		if !ok {
			continue
		}
		params := entry.Tool.Parameters()
		if params == nil {
			// 部分后端要求 parameters 必须是对象，缺字段直接被拒。
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, llm.ToolDef{
			Name:        name,
			Description: entry.Tool.Description(),
			Parameters:  params,
		})
	}
	return out
}

// Lookup 按名字查工具（包含未激活的工具；是否允许调用由调用方决定）。
func (c *Catalog) Lookup(name string) (agent.Tool, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[strings.TrimSpace(name)]
	if !ok {
		return nil, false
	}
	return entry.Tool, true
}

// Activate 激活指定工具（幂等），返回**本次新激活**的工具名。
// 未知名字与已激活名字会被跳过：tool_search 可以放心地把一批候选交给它。
func (c *Catalog) Activate(names ...string) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activateLocked(names)
}

// activateLocked 是 Activate 的无锁版本（调用方已持写锁）。
func (c *Catalog) activateLocked(names []string) []string {
	var activated []string
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || c.active[name] {
			continue
		}
		if _, ok := c.entries[name]; !ok {
			continue
		}
		c.active[name] = true
		c.activeOrder = append(c.activeOrder, name)
		activated = append(activated, name)
	}
	return activated
}

// ActivateToolset 激活若干工具集，返回本次新激活的工具名。
//
// Depends 会被递归带上（按声明顺序先依赖后自身）；循环依赖用访问集兜住。
// 未知工具集静默跳过，方便直接喂环境变量解析结果。
func (c *Catalog) ActivateToolset(names ...string) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activateToolsetLocked(names)
}

// activateToolsetLocked 是 ActivateToolset 的无锁版本（调用方已持写锁）。
func (c *Catalog) activateToolsetLocked(names []string) []string {
	visiting := make(map[string]bool, len(c.sets))
	var activated []string
	var activateSet func(name string)
	activateSet = func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || visiting[name] {
			return
		}
		visiting[name] = true
		set, ok := c.set(name)
		if !ok {
			return
		}
		for _, dep := range set.Depends {
			activateSet(dep)
		}
		tools := make([]string, 0, len(set.Tools))
		for _, t := range set.Tools {
			tools = append(tools, t.Name())
		}
		activated = append(activated, c.activateLocked(tools)...)
	}
	for _, name := range names {
		activateSet(name)
	}
	return activated
}

// set 按名字取工具集（调用方已持锁）。
func (c *Catalog) set(name string) (Toolset, bool) {
	for _, set := range c.sets {
		if set.Name == name {
			return set, true
		}
	}
	return Toolset{}, false
}

// ActivateAll 激活全部工具，返回新激活的工具名。
//
// 用于保持「AGENT_TOOLSETS 为空 = 全量暴露」的向后兼容路径；
// 开启渐进披露时不要调它。
func (c *Catalog) ActivateAll() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activateLocked(c.order)
}

// RiskOf 按名字查目录里的工具风险；目录里没有的名字返回 false。
//
// 登记时已经按「工具自己的 RiskAnnotated，否则 LookupMeta」解析过（见 riskOf）。
// Plan 的 riskOf 参数可以直接传这个方法的闭包。未登记名字不要当成只读。
func (c *Catalog) RiskOf(name string) (Risk, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[strings.TrimSpace(name)]
	if !ok {
		return "", false
	}
	return entry.Risk, true
}

// Search 按关键词检索工具，返回按相关度降序的前 limit 条。
//
// 打分档位固定：名字精确 > 名字前缀 > 别名精确 > 名字子串 > 别名子串 > 描述子串。
// 多关键词（空格分隔）只会带来不超过 90 分的档内加成，绝不会跨档。
// 同分按 toolset 声明顺序稳定排序。
func (c *Catalog) Search(query string, limit int) []Match {
	if c == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	type hit struct {
		match Match
		index int
	}
	hits := make([]hit, 0, len(c.order))
	for index, name := range c.order {
		entry := c.entries[name]
		score, ok := scoreEntry(entry, name, q)
		if !ok {
			continue
		}
		hits = append(hits, hit{
			match: Match{
				Name:        name,
				Toolset:     entry.Toolset,
				Description: oneLineDesc(entry.Tool.Description(), 80),
				Risk:        entry.Risk,
				Active:      c.active[name],
				Score:       score,
			},
			index: index,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].match.Score != hits[j].match.Score {
			return hits[i].match.Score > hits[j].match.Score
		}
		return hits[i].index < hits[j].index
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Match, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.match)
	}
	return out
}

// scoreEntry 给一个工具按查询词打分；ok=false 表示完全不相关。
//
// 档位间隔都大于最大档内加成（90），保证契约 §3 的优先级不会被多关键词加成打乱。
func scoreEntry(entry Entry, name, query string) (int, bool) {
	lowerName := strings.ToLower(name)
	lowerDesc := strings.ToLower(entry.Tool.Description())
	lowerTags := make([]string, 0, len(entry.Tags))
	for _, tag := range entry.Tags {
		if t := strings.ToLower(strings.TrimSpace(tag)); t != "" {
			lowerTags = append(lowerTags, t)
		}
	}
	base := 0
	switch {
	case lowerName == query:
		base = 1000
	case strings.HasPrefix(lowerName, query):
		base = 800
	case containsString(lowerTags, query):
		base = 700
	case strings.Contains(lowerName, query):
		base = 600
	case containsSubstring(lowerTags, query):
		base = 500
	case strings.Contains(lowerDesc, query):
		base = 300
	case strings.Contains(strings.ToLower(entry.Toolset), query):
		base = 200
	}
	bonus := 0
	for _, token := range strings.Fields(query) {
		switch {
		case strings.Contains(lowerName, token):
			bonus += 20
		case containsSubstring(lowerTags, token):
			bonus += 15
		case strings.Contains(lowerDesc, token):
			bonus += 10
		}
	}
	if bonus > 90 {
		bonus = 90 // 档内加成封顶，避免跨档
	}
	if base == 0 && bonus == 0 {
		return 0, false
	}
	return base + bonus, true
}

// containsString 判断切片中是否有完全相等的元素（调用方已转小写）。
func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// containsSubstring 判断切片中是否有元素包含子串。
func containsSubstring(items []string, sub string) bool {
	for _, item := range items {
		if strings.Contains(item, sub) {
			return true
		}
	}
	return false
}

// oneLineDesc 把描述压成一句话：只取第一行，超长按 rune 截断。
func oneLineDesc(desc string, max int) string {
	s := strings.TrimSpace(desc)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Toolsets 返回全部工具集的描述（声明顺序），供提示词清单与 HTTP 接口使用。
func (c *Catalog) Toolsets() []ToolsetInfo {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ToolsetInfo, 0, len(c.sets))
	for _, set := range c.sets {
		info := ToolsetInfo{
			Name:    set.Name,
			Desc:    set.Desc,
			Risk:    set.Risk,
			Depends: append([]string(nil), set.Depends...),
			Tools:   []string{},
		}
		for _, name := range c.order {
			entry := c.entries[name]
			if entry.Toolset != set.Name {
				continue
			}
			info.Tools = append(info.Tools, name)
			if c.active[name] {
				info.Active++
			}
		}
		info.Activated = len(info.Tools) > 0 && info.Active == len(info.Tools)
		out = append(out, info)
	}
	return out
}

// Len 返回工具总数与已激活数。
func (c *Catalog) Len() (total, active int) {
	if c == nil {
		return 0, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries), len(c.activeOrder)
}

// riskOf 解析单个工具的风险。
//
// 优先级：工具自己的 RiskAnnotated（合法值）> LookupMeta。
// 装饰器不转发 Risk()，所以必须在 Decorate 之前用原始工具解析，结果记在 Entry 上。
// 表外名字拿到的是 LookupMeta 的保守风险（写入），不再退回工具集的整组默认值——
// 工具集 Risk 只作为组标签（提示词 / HTTP）。fallback 只留给没有名字的工具。
func riskOf(t agent.Tool, fallback Risk) Risk {
	if t != nil {
		if annotated, ok := t.(RiskAnnotated); ok {
			if risk := annotated.Risk(); risk.Valid() {
				return risk
			}
		}
		if name := strings.TrimSpace(t.Name()); name != "" {
			return LookupMeta(name).Risk
		}
	}
	return normalizeRisk(fallback)
}

// tagsOf 取工具的可选检索别名。
func tagsOf(t agent.Tool) []string {
	annotated, ok := t.(TagsAnnotated)
	if !ok {
		return nil
	}
	tags := annotated.Tags()
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
