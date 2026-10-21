package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"blog-my/internal/agent"
)

// ToolDeps 是装配自进化工具所需的依赖，由 web 层（internal/web/evolve.go）构造。
//
// 本包不读环境变量、不碰数据库：开关（AGENT_EVOLVE）与路径都在 web 层决定后注入，
// 这样包内单测可以完全离线运行。
type ToolDeps struct {
	// Store 是技能库；skill_list/skill_lint/skill_improve/skill_archive 依赖它。
	Store SkillStore
	// Ledger 是技能使用账本；可为 nil（此时统计相关工具返回空统计）。
	Ledger *Ledger
	// Memory 是结构化记忆文件；可为 nil（memory_* 工具会报未初始化）。
	Memory *MemoryFile
	// Recorder 是轨迹记录器；evolution_report 用它定位报告目录。
	Recorder *Recorder
	// UserID 是当前用户；技能库按用户隔离。
	UserID string
	// Dir 是数据目录（报告落盘位置）；为空时按 Recorder 目录、再按 DefaultDir()。
	Dir string
}

// skillListTool 实现 skill_list。
type skillListTool struct{ deps ToolDeps }

// SkillListTool 返回 skill_list 工具：列出技能 + 使用统计 + 体检摘要。
func SkillListTool(deps ToolDeps) agent.Tool { return skillListTool{deps: deps} }

func (skillListTool) Name() string { return "skill_list" }

func (skillListTool) Description() string {
	return "列出你可用的技能，附带使用次数、成功率和体检摘要；可按关键词过滤、只看有风险的。" +
		"想找「有没有做过类似事的技能」时先用它，别凭印象猜技能名。"
}

func (skillListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "可选：按名称/描述/正文关键词过滤。",
			},
			"only_at_risk": map[string]any{
				"type":        "boolean",
				"description": "可选：只列出失败率高或长期未使用的技能（用于决定该修还是该归档）。",
			},
		},
	}
}

func (t skillListTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Store == nil {
		return agent.Result{}, errors.New("evolve: 技能库未初始化，无法列出技能")
	}
	var args struct {
		Query      string `json:"query"`
		OnlyAtRisk bool   `json:"only_at_risk"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	items, err := t.deps.Store.List(t.deps.UserID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 读取技能库失败: %w", err)
	}
	atRisk := map[string]struct{}{}
	if args.OnlyAtRisk {
		if t.deps.Ledger == nil {
			return agent.Result{IsError: true, Content: "技能使用账本未初始化，无法判断风险技能。"}, nil
		}
		risky, err := t.deps.Ledger.AtRisk(curatorAtRiskMinUses, 0)
		if err != nil {
			return agent.Result{}, fmt.Errorf("evolve: 读取技能账本失败: %w", err)
		}
		for _, st := range risky {
			atRisk[st.Name] = struct{}{}
		}
	}
	stats := map[string]SkillStat{}
	if t.deps.Ledger != nil {
		if got, err := t.deps.Ledger.Stats(); err == nil {
			for _, st := range got {
				stats[st.Name] = st
			}
		}
	}
	query := strings.ToLower(strings.TrimSpace(args.Query))
	lines := make([]string, 0, len(items))
	for _, it := range items {
		if args.OnlyAtRisk {
			if _, ok := atRisk[it.Name]; !ok {
				continue
			}
		}
		if query != "" {
			haystack := strings.ToLower(it.Name + "\n" + it.Description + "\n" + it.Body)
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		lines = append(lines, "- "+formatSkillLine(it, stats[it.Name]))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return agent.Result{Content: "没有匹配的技能。"}, nil
	}
	header := fmt.Sprintf("共 %d 条技能，匹配 %d 条：", len(items), len(lines))
	return agent.Result{Content: header + "\n" + strings.Join(lines, "\n")}, nil
}

// skillStatsTool 实现 skill_stats。
type skillStatsTool struct{ deps ToolDeps }

// SkillStatsTool 返回 skill_stats 工具：单条或全部技能的 uses/成功率/最近使用。
func SkillStatsTool(deps ToolDeps) agent.Tool { return skillStatsTool{deps: deps} }

func (skillStatsTool) Name() string { return "skill_stats" }

func (skillStatsTool) Description() string {
	return "查看技能的使用统计：使用次数、成功次数、失败次数、成功率、最近使用时间。" +
		"判断一条技能「还有没有用」时用它；技能名不传则返回全部统计。"
}

func (skillStatsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "可选：技能名；不传则返回全部。",
			},
		},
	}
}

func (t skillStatsTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Ledger == nil {
		return agent.Result{}, errors.New("evolve: 技能使用账本未初始化，无法查看统计")
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	name := strings.TrimSpace(args.Name)
	if name != "" {
		st, ok, err := t.deps.Ledger.Stat(name)
		if err != nil {
			return agent.Result{}, err
		}
		if !ok {
			return agent.Result{IsError: true, Content: fmt.Sprintf("技能 %q 还没有任何使用记录。", name)}, nil
		}
		return agent.Result{Content: formatStat(st)}, nil
	}
	stats, err := t.deps.Ledger.Stats()
	if err != nil {
		return agent.Result{}, err
	}
	if len(stats) == 0 {
		return agent.Result{Content: "账本里还没有任何技能使用记录。"}, nil
	}
	lines := make([]string, 0, len(stats))
	for _, st := range stats {
		lines = append(lines, formatStat(st))
	}
	return agent.Result{Content: strings.Join(lines, "\n")}, nil
}

// skillLintTool 实现 skill_lint。
type skillLintTool struct{ deps ToolDeps }

// SkillLintTool 返回 skill_lint 工具：体检一条技能并返回问题清单。
func SkillLintTool(deps ToolDeps) agent.Tool { return skillLintTool{deps: deps} }

func (skillLintTool) Name() string { return "skill_lint" }

func (skillLintTool) Description() string {
	return "体检一条技能：检查名字格式、描述长度、正文结构（缺少「何时使用/怎么做」）、正文大小，" +
		"以及正文里是否写了危险命令或明文密钥。skill_improve 写入前会自动做同样的检查；" +
		"用户问「这条技能有没有问题」时用它。"
}

func (skillLintTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "要体检的技能名。",
			},
		},
		"required": []string{"name"},
	}
}

func (t skillLintTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Store == nil {
		return agent.Result{}, errors.New("evolve: 技能库未初始化，无法体检技能")
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return agent.Result{IsError: true, Content: "参数无效：name 不能为空。"}, nil
	}
	it, ok, err := t.deps.Store.Get(t.deps.UserID, name)
	if err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 读取技能失败: %w", err)
	}
	if !ok {
		return agent.Result{IsError: true, Content: fmt.Sprintf("技能 %q 不存在。", name)}, nil
	}
	issues := Lint(it)
	if len(issues) == 0 {
		return agent.Result{Content: fmt.Sprintf("技能 %q 体检通过，没有发现问题。", name)}, nil
	}
	errorsN, warningsN := countIssues(issues)
	return agent.Result{
		Content: fmt.Sprintf("技能 %q 体检结果：%d 个 error、%d 个 warning。\n%s",
			name, errorsN, warningsN, formatIssues(issues)),
		Display: map[string]any{"errors": errorsN, "warnings": warningsN},
	}, nil
}

// skillImproveTool 实现 skill_improve。
type skillImproveTool struct{ deps ToolDeps }

// SkillImproveTool 返回 skill_improve 工具：在旧正文基础上打补丁改技能，写前先 lint。
func SkillImproveTool(deps ToolDeps) agent.Tool { return skillImproveTool{deps: deps} }

func (skillImproveTool) Name() string { return "skill_improve" }

func (skillImproveTool) Description() string {
	return "在**旧正文基础上**改进一条已有技能：patch 里用 `## 章节标题` 给出要改的内容，" +
		"同名章节被替换、新章节追加；不含 `## ` 的 patch 会追加到「## 补充」章节。" +
		"写入前会自动体检，出现 error（危险命令、明文密钥、正文过大等）时拒绝写入。" +
		"改技能优先用它而不是整条覆盖，避免把旧正文里有用的内容写没。"
}

func (skillImproveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "要改进的已有技能名。",
			},
			"patch": map[string]any{
				"type":        "string",
				"description": "补丁正文（Markdown）。用 `## 章节标题` 替换旧正文里的同名章节；新章节会追加；不含章节标题时追加到「## 补充」。",
			},
		},
		"required": []string{"name", "patch"},
	}
}

func (t skillImproveTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Store == nil {
		return agent.Result{}, errors.New("evolve: 技能库未初始化，无法改进技能")
	}
	var args struct {
		Name  string `json:"name"`
		Patch string `json:"patch"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	name := strings.TrimSpace(args.Name)
	patch := strings.TrimSpace(args.Patch)
	if name == "" || patch == "" {
		return agent.Result{IsError: true, Content: "参数无效：name 与 patch 都不能为空。"}, nil
	}
	old, ok, err := t.deps.Store.Get(t.deps.UserID, name)
	if err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 读取技能失败: %w", err)
	}
	if !ok {
		return agent.Result{IsError: true, Content: fmt.Sprintf("技能 %q 不存在；新建技能请用 skill_write。", name)}, nil
	}
	newBody := mergeSkillBody(old.Body, patch)
	it := SkillItem{
		Name:        old.Name,
		Description: old.Description,
		Body:        newBody,
		Source:      firstNonEmpty(old.Source, "evolve"),
		Enabled:     old.Enabled,
	}
	if issues := Lint(it); HasError(issues) {
		return agent.Result{IsError: true, Content: "补丁未写入：新正文体检不通过。\n" + formatIssues(issues)}, nil
	}
	if _, err := t.deps.Store.Upsert(t.deps.UserID, it); err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 保存技能失败: %w", err)
	}
	verb := "已更新"
	if !it.Enabled {
		verb = "已更新（该技能处于归档状态，本次不会自动启用）"
	}
	return agent.Result{
		Content: fmt.Sprintf("%s技能 %q，正文 %d 字。", verb, name, len([]rune(newBody))),
		Display: map[string]any{"name": name, "runes": len([]rune(newBody))},
	}, nil
}

// skillArchiveTool 实现 skill_archive。
type skillArchiveTool struct{ deps ToolDeps }

// SkillArchiveTool 返回 skill_archive 工具：归档（enabled=false）而不删除技能。
func SkillArchiveTool(deps ToolDeps) agent.Tool { return skillArchiveTool{deps: deps} }

func (skillArchiveTool) Name() string { return "skill_archive" }

func (skillArchiveTool) Description() string {
	return "归档一条技能：标记为停用（不再进入提示词），但正文保留、随时可以恢复，不会删除数据。" +
		"技能长期没用、内容过时或与别的技能重复时用它；删除是用户才能做的决定，你只归档。"
}

func (skillArchiveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "要归档的技能名。",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "归档原因（一句话，会回报给用户）。",
			},
		},
		"required": []string{"name"},
	}
}

func (t skillArchiveTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Store == nil {
		return agent.Result{}, errors.New("evolve: 技能库未初始化，无法归档技能")
	}
	var args struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return agent.Result{IsError: true, Content: "参数无效：name 不能为空。"}, nil
	}
	it, ok, err := t.deps.Store.Get(t.deps.UserID, name)
	if err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 读取技能失败: %w", err)
	}
	if !ok {
		return agent.Result{IsError: true, Content: fmt.Sprintf("技能 %q 不存在。", name)}, nil
	}
	if !it.Enabled {
		return agent.Result{Content: fmt.Sprintf("技能 %q 已经处于归档状态。", name)}, nil
	}
	it.Enabled = false
	it.Source = firstNonEmpty(it.Source, "evolve")
	if _, err := t.deps.Store.Upsert(t.deps.UserID, it); err != nil {
		return agent.Result{}, fmt.Errorf("evolve: 归档技能失败: %w", err)
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		reason = "未说明原因"
	}
	return agent.Result{Content: fmt.Sprintf("已归档技能 %q（原因：%s）。正文仍保留，需要时可以恢复。", name, reason)}, nil
}

// memoryNoteTool 实现 memory_note。
type memoryNoteTool struct{ deps ToolDeps }

// MemoryNoteTool 返回 memory_note 工具：写一条结构化记忆（自动去重）。
func MemoryNoteTool(deps ToolDeps) agent.Tool { return memoryNoteTool{deps: deps} }

func (memoryNoteTool) Name() string { return "memory_note" }

func (memoryNoteTool) Description() string {
	return "记一条需要长期记住的事项（偏好、约定、重要事实）。" +
		"适合「用户明确说以后都这样」「某个环境/账号的固定约定」这类信息；" +
		"一次性任务细节不要记。内容规范化后相同会自动去重，不会重复写入。"
}

func (memoryNoteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"topic": map[string]any{
				"type":        "string",
				"description": "主题（如「开发约定」「用户偏好」），不传归到「通用」。",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "要记住的内容，一句话说清，别写成长篇。",
			},
		},
		"required": []string{"text"},
	}
}

func (t memoryNoteTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Memory == nil {
		return agent.Result{}, errors.New("evolve: 结构化记忆未启用，无法写入记忆")
	}
	var args struct {
		Topic string `json:"topic"`
		Text  string `json:"text"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	created, err := t.deps.Memory.Add(args.Topic, args.Text)
	if err != nil {
		return agent.Result{}, err
	}
	if created {
		return agent.Result{Content: "已记住这条信息。"}, nil
	}
	return agent.Result{Content: "这条信息已经记过了（内容相同，未重复写入）。"}, nil
}

// memoryListTool 实现 memory_list。
type memoryListTool struct{ deps ToolDeps }

// MemoryListTool 返回 memory_list 工具：读结构化记忆。
func MemoryListTool(deps ToolDeps) agent.Tool { return memoryListTool{deps: deps} }

func (memoryListTool) Name() string { return "memory_list" }

func (memoryListTool) Description() string {
	return "列出结构化记忆（可只列某个主题）。这些是你明确记下、每轮都会注入提示词的稳定信息；" +
		"不确定「以前有没有记过某件事」时先看这里，而不是回答「我不记得」。"
}

func (memoryListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"topic": map[string]any{
				"type":        "string",
				"description": "可选：只看某个主题。",
			},
		},
	}
}

func (t memoryListTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Memory == nil {
		return agent.Result{}, errors.New("evolve: 结构化记忆未启用，无法读取记忆")
	}
	var args struct {
		Topic string `json:"topic"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	entries, err := t.deps.Memory.All()
	if err != nil {
		return agent.Result{}, err
	}
	topic := strings.TrimSpace(args.Topic)
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if topic != "" && e.Topic != topic {
			continue
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s（id=%s，命中 %d 次）", e.Topic, e.Text, e.ID, e.Hits))
	}
	if len(lines) == 0 {
		if topic != "" {
			return agent.Result{Content: fmt.Sprintf("主题 %q 下没有记忆。", topic)}, nil
		}
		return agent.Result{Content: "结构化记忆还是空的。"}, nil
	}
	return agent.Result{Content: strings.Join(lines, "\n")}, nil
}

// memoryForgetTool 实现 memory_forget。
type memoryForgetTool struct{ deps ToolDeps }

// MemoryForgetTool 返回 memory_forget 工具：按 id 或原文删一条记忆。
func MemoryForgetTool(deps ToolDeps) agent.Tool { return memoryForgetTool{deps: deps} }

func (memoryForgetTool) Name() string { return "memory_forget" }

func (memoryForgetTool) Description() string {
	return "忘掉一条结构化记忆：按 id（memory_list 里能看到）或按原文内容删除。" +
		"用户说「不用记这个了」「这条记错了」时用它；只删一条，不要一次清空。"
}

func (memoryForgetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "要删除的记忆 id（与 text 二选一）。",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "要删除的记忆原文（与 id 二选一）。",
			},
		},
	}
}

func (t memoryForgetTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	if t.deps.Memory == nil {
		return agent.Result{}, errors.New("evolve: 结构化记忆未启用，无法删除记忆")
	}
	var args struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.Result{}, err
	}
	id := strings.TrimSpace(args.ID)
	text := strings.TrimSpace(args.Text)
	if id == "" && text == "" {
		return agent.Result{IsError: true, Content: "参数无效：id 与 text 至少给一个。"}, nil
	}
	if id != "" {
		ok, err := t.deps.Memory.Remove(id)
		if err != nil {
			return agent.Result{}, err
		}
		if !ok {
			return agent.Result{IsError: true, Content: fmt.Sprintf("没有 id 为 %q 的记忆。", id)}, nil
		}
		return agent.Result{Content: "已删除那条记忆。"}, nil
	}
	entries, err := t.deps.Memory.All()
	if err != nil {
		return agent.Result{}, err
	}
	target := normalizeMemoryText(text)
	for _, e := range entries {
		if normalizeMemoryText(e.Text) != target && !strings.Contains(normalizeMemoryText(e.Text), target) {
			continue
		}
		ok, err := t.deps.Memory.Remove(e.ID)
		if err != nil {
			return agent.Result{}, err
		}
		if ok {
			return agent.Result{Content: fmt.Sprintf("已删除记忆：[%s] %s", e.Topic, e.Text)}, nil
		}
	}
	return agent.Result{IsError: true, Content: "没有找到内容匹配的记忆；可以先 memory_list 看看原文。"}, nil
}

// evolutionReportTool 实现 evolution_report。
type evolutionReportTool struct{ deps ToolDeps }

// EvolutionReportTool 返回 evolution_report 工具：读最近一次后台固化报告。
func EvolutionReportTool(deps ToolDeps) agent.Tool { return evolutionReportTool{deps: deps} }

func (evolutionReportTool) Name() string { return "evolution_report" }

func (evolutionReportTool) Description() string {
	return "读取最近一次「自我进化」后台固化的报告：看了多少条轨迹、发现哪些重复技能/高风险技能、" +
		"提炼了哪些候选、自动改了哪些。用户问「你最近学到了什么」「技能库有没有问题」时用它；" +
		"没有报告说明还没跑过固化，如实告诉用户。"
}

func (evolutionReportTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t evolutionReportTool) Execute(ctx context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return agent.Result{}, err
	}
	dir := t.deps.Dir
	if dir == "" && t.deps.Recorder != nil {
		dir = t.deps.Recorder.dir()
	}
	cur := &Curator{Recorder: t.deps.Recorder}
	rep, ok, err := cur.LastReport(dir)
	if err != nil {
		return agent.Result{}, err
	}
	if !ok {
		return agent.Result{Content: "还没有固化报告：后台进化还没跑过，或轨迹数据不足。"}, nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("最近一次固化：%s，回看 %d 条轨迹、%d 条技能。\n",
		rep.At.Format("2006-01-02 15:04"), rep.Runs, rep.Skills))
	if len(rep.Duplicates) > 0 {
		pairs := make([]string, 0, len(rep.Duplicates))
		for _, p := range rep.Duplicates {
			pairs = append(pairs, p[0]+" ≈ "+p[1])
		}
		b.WriteString("疑似重复：" + strings.Join(pairs, "；") + "\n")
	}
	if len(rep.AtRisk) > 0 {
		risks := make([]string, 0, len(rep.AtRisk))
		for _, st := range rep.AtRisk {
			risks = append(risks, fmt.Sprintf("%s（%d 次，成功率 %.0f%%）", st.Name, st.Uses, st.SuccessRate*100))
		}
		b.WriteString("需要关注：" + strings.Join(risks, "；") + "\n")
	}
	if len(rep.Candidates) > 0 {
		lines := make([]string, 0, len(rep.Candidates))
		for _, c := range rep.Candidates {
			lines = append(lines, fmt.Sprintf("- [%s] %s（置信度 %.2f）", c.Kind, c.Name, c.Confidence))
		}
		b.WriteString(fmt.Sprintf("候选 %d 条：\n%s\n", len(rep.Candidates), strings.Join(lines, "\n")))
	}
	if len(rep.Applied) > 0 {
		b.WriteString("已自动改动：" + strings.Join(rep.Applied, "；") + "\n")
	} else {
		b.WriteString("本次没有改动技能库（只报告模式，或没有需要改的）。\n")
	}
	return agent.Result{Content: strings.TrimSpace(b.String())}, nil
}

// decodeArgs 解析工具参数；空参数按空对象处理（可选参数工具用）。
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	return nil
}

// ctxErr 在 ctx 已取消时返回错误（长列表操作在取消后尽快退出）。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// countIssues 统计 error/warning 数量。
func countIssues(issues []Issue) (errorsN, warningsN int) {
	for _, it := range issues {
		if it.Severity == SeverityError {
			errorsN++
		} else {
			warningsN++
		}
	}
	return errorsN, warningsN
}

// formatIssues 把体检结果排版成模型能直接读的清单。
func formatIssues(issues []Issue) string {
	if len(issues) == 0 {
		return "（没有发现问题）"
	}
	lines := make([]string, 0, len(issues))
	for _, it := range issues {
		lines = append(lines, fmt.Sprintf("- [%s] %s: %s", it.Severity, it.Code, it.Message))
	}
	return strings.Join(lines, "\n")
}

// formatSkillLine 排版 skill_list 的一行。
func formatSkillLine(it SkillItem, st SkillStat) string {
	state := "启用"
	if !it.Enabled {
		state = "已归档"
	}
	desc := strings.TrimSpace(it.Description)
	if desc == "" {
		desc = "（无描述）"
	}
	if len([]rune(desc)) > 60 {
		desc = string([]rune(desc)[:60]) + "…"
	}
	parts := []string{fmt.Sprintf("%s（%s）：%s", it.Name, state, desc)}
	if st.Uses > 0 {
		parts = append(parts, fmt.Sprintf("使用 %d 次，成功率 %.0f%%，最近 %s",
			st.Uses, st.SuccessRate*100, formatTime(st.LastUsed)))
	} else {
		parts = append(parts, "还没有使用记录")
	}
	if errorsN, warningsN := countIssues(Lint(it)); errorsN > 0 || warningsN > 0 {
		parts = append(parts, fmt.Sprintf("体检 %d error / %d warning", errorsN, warningsN))
	}
	return "- " + strings.Join(parts, "；")
}

// formatStat 排版一条技能统计。
func formatStat(st SkillStat) string {
	return fmt.Sprintf("- %s：使用 %d 次（成功 %d / 失败 %d），成功率 %.0f%%，最近 %s",
		st.Name, st.Uses, st.OK, st.Fail, st.SuccessRate*100, formatTime(st.LastUsed))
}

// formatTime 把时间格式化成「2006-01-02 15:04」，零值显示「从未」。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "从未"
	}
	return t.Format("2006-01-02 15:04")
}

// mdSection 是 Markdown 正文里的一个二级章节。
type mdSection struct {
	title string
	lines []string
}

// mergeSkillBody 在旧正文基础上合并补丁：同名章节替换、新章节追加、无章节补丁进「## 补充」。
//
// 这是 skill_improve 的核心：绝不整段覆盖，避免模型把旧正文里有用的内容写没。
func mergeSkillBody(oldBody, patch string) string {
	oldBody = strings.TrimRight(oldBody, "\n")
	patch = strings.TrimSpace(patch)
	if patch == "" {
		return oldBody
	}
	if strings.TrimSpace(oldBody) == "" {
		return patch + "\n"
	}
	head, oldSecs := splitMarkdownSections(oldBody)
	_, patchSecs := splitMarkdownSections(patch)
	if len(patchSecs) == 0 {
		return oldBody + "\n\n## 补充\n\n" + patch + "\n"
	}
	idx := make(map[string]int, len(oldSecs))
	for i, sec := range oldSecs {
		idx[sec.title] = i
	}
	for _, sec := range patchSecs {
		if i, ok := idx[sec.title]; ok {
			oldSecs[i] = sec
			continue
		}
		idx[sec.title] = len(oldSecs)
		oldSecs = append(oldSecs, sec)
	}
	var b strings.Builder
	if strings.TrimSpace(strings.Join(head, "\n")) != "" {
		b.WriteString(strings.TrimRight(strings.Join(head, "\n"), "\n"))
		b.WriteString("\n\n")
	}
	for i, sec := range oldSecs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## ")
		b.WriteString(sec.title)
		b.WriteString("\n")
		if body := strings.TrimRight(strings.Join(sec.lines, "\n"), "\n"); strings.TrimSpace(body) != "" {
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// splitMarkdownSections 把正文切成「首个 ## 之前的内容」+ 二级章节列表。
func splitMarkdownSections(body string) (head []string, secs []mdSection) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	cur := -1
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			secs = append(secs, mdSection{title: title})
			cur = len(secs) - 1
			continue
		}
		if cur < 0 {
			head = append(head, line)
			continue
		}
		secs[cur].lines = append(secs[cur].lines, line)
	}
	return head, secs
}
