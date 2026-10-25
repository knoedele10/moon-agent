package agent

// 任务计划（todo / plan）状态。
//
// 为什么要它：长任务跑到第 20 轮时，模型最容易丢的不是「工具怎么用」，而是
// 「我到底做到哪一步了」。Claude Code 的 TodoWrite、Codex 的 update_plan 都在解决
// 同一个问题：把计划外化成一份可更新的状态，而不是让它只存在于上下文里。
//
// 设计边界：
//   - 状态在进程内、按会话（Exec.SessionID）隔离，重启即清空；不新增数据库迁移。
//   - 工具在 web 层注册，本文件只提供存储与渲染，避免 agent 包反向依赖工具实现。
//   - 计划会作为一条**瞬态 system 消息**在每轮模型调用前注入，不写进历史，
//     因此不会随轮次累积，也不会污染会话记录。

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"blog-my/internal/llm"
)

// 计划步骤状态（归一化后的取值）。
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanCompleted  = "completed"
	PlanBlocked    = "blocked"
)

const (
	// PlanMaxSteps 限制计划长度：计划越长，模型越会忘记更新。
	PlanMaxSteps = 20
	// planBlockPrefix 是瞬态注入消息的前缀，用于每轮替换旧的计划块。
	planBlockPrefix = "【当前任务计划】"
	// planStoreMaxSessions 限制内存里的会话数，防止长期运行时无界增长。
	planStoreMaxSessions = 256
)

// PlanStep 是计划里的一步。
type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// Plan 是一个会话当前的任务计划。
type Plan struct {
	Explanation string     `json:"explanation,omitempty"`
	Steps       []PlanStep `json:"plan"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// normalizePlanStatus 把模型常用的同义词收敛到四个状态。
func normalizePlanStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending", "todo", "not_started", "waiting", "未开始", "待办":
		return PlanPending, true
	case "in_progress", "in-progress", "doing", "active", "current", "进行中", "处理中":
		return PlanInProgress, true
	case "completed", "complete", "done", "finished", "完成", "已完成":
		return PlanCompleted, true
	case "blocked", "blocked_by", "failed", "stuck", "受阻", "阻塞", "失败":
		return PlanBlocked, true
	default:
		return "", false
	}
}

// normalizePlan 校验并归一化一个计划。
func normalizePlan(p Plan) (Plan, error) {
	if len(p.Steps) == 0 {
		return Plan{}, fmt.Errorf("plan 不能为空：至少给出一个步骤")
	}
	if len(p.Steps) > PlanMaxSteps {
		return Plan{}, fmt.Errorf("plan 最多 %d 步（收到 %d 步）：请合并相近步骤", PlanMaxSteps, len(p.Steps))
	}
	seen := make(map[string]bool, len(p.Steps))
	out := Plan{Explanation: strings.TrimSpace(p.Explanation), UpdatedAt: time.Now()}
	inProgress := 0
	for i, raw := range p.Steps {
		step := strings.TrimSpace(raw.Step)
		if step == "" {
			return Plan{}, fmt.Errorf("第 %d 步内容为空：每一步都要能看出要做什么", i+1)
		}
		step = clipRunes(step, 200)
		if seen[step] {
			return Plan{}, fmt.Errorf("步骤重复：%q；请合并重复步骤", step)
		}
		seen[step] = true
		status, ok := normalizePlanStatus(raw.Status)
		if !ok {
			return Plan{}, fmt.Errorf("第 %d 步状态 %q 不合法：只支持 pending / in_progress / completed / blocked", i+1, raw.Status)
		}
		if status == PlanInProgress {
			inProgress++
		}
		out.Steps = append(out.Steps, PlanStep{Step: step, Status: status})
	}
	if inProgress > 1 {
		return Plan{}, fmt.Errorf("同时有 %d 步处于 in_progress：一次只保留一个进行中的步骤，其余用 pending", inProgress)
	}
	return out, nil
}

// planStore 是按会话隔离的计划表。
type planStore struct {
	mu      sync.Mutex
	plans   map[string]Plan
	order   []string // 最近更新的会话在末尾，用于淘汰
	maxSize int
}

var plans = &planStore{plans: make(map[string]Plan), maxSize: planStoreMaxSessions}

// UpdatePlan 校验并替换一个会话的计划；返回归一化后的计划。
//
// 语义是**整体替换**（与 Claude Code 的 TodoWrite 一致）：模型每次给出完整清单，
// 而不是做增量 patch——增量更新在长上下文里极易与真实状态脱节。
func UpdatePlan(session string, p Plan) (Plan, error) {
	normalized, err := normalizePlan(p)
	if err != nil {
		return Plan{}, err
	}
	plans.mu.Lock()
	defer plans.mu.Unlock()
	if _, exists := plans.plans[session]; !exists {
		plans.order = append(plans.order, session)
	}
	plans.plans[session] = normalized
	// 淘汰最久未更新的会话（简单 FIFO 足够：计划很小，且只影响内存占用）。
	for len(plans.order) > plans.maxSize {
		oldest := plans.order[0]
		plans.order = plans.order[1:]
		delete(plans.plans, oldest)
	}
	return normalized, nil
}

// CurrentPlan 读取一个会话的计划。
func CurrentPlan(session string) (Plan, bool) {
	plans.mu.Lock()
	defer plans.mu.Unlock()
	p, ok := plans.plans[session]
	return p, ok
}

// ClearPlan 清掉一个会话的计划（会话删除或测试用）。
func ClearPlan(session string) {
	plans.mu.Lock()
	defer plans.mu.Unlock()
	delete(plans.plans, session)
	for i, s := range plans.order {
		if s == session {
			plans.order = append(plans.order[:i], plans.order[i+1:]...)
			break
		}
	}
}

// ResetPlans 清空全部计划（测试用）。
func ResetPlans() {
	plans.mu.Lock()
	defer plans.mu.Unlock()
	plans.plans = make(map[string]Plan)
	plans.order = nil
}

// PlanBlock 把计划渲染成注入模型上下文的瞬态文本；没有计划时返回空串。
func PlanBlock(session string) string {
	p, ok := CurrentPlan(session)
	if !ok || len(p.Steps) == 0 {
		return ""
	}
	return planBlockPrefix + "\n" + renderPlan(p)
}

// RenderPlan 是 renderPlan 的导出形式，供 web 层的工具回执复用同一套格式。
func RenderPlan(p Plan) string { return renderPlan(p) }

// renderPlan 渲染计划清单：状态用 ASCII 勾选框，跨模型都容易读。
func renderPlan(p Plan) string {
	done := 0
	for _, s := range p.Steps {
		if s.Status == PlanCompleted {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "进度 %d/%d", done, len(p.Steps))
	if p.Explanation != "" {
		b.WriteString("；说明：" + clipRunes(p.Explanation, 200))
	}
	b.WriteByte('\n')
	for _, s := range p.Steps {
		b.WriteString(planCheckbox(s.Status))
		b.WriteByte(' ')
		b.WriteString(s.Step)
		b.WriteByte('\n')
	}
	if next := nextPlanStep(p); next != "" {
		b.WriteString("下一步：" + next + "\n")
	}
	b.WriteString("（这是一份活的状态。完成一步就调用 update_plan 更新；未验证的步骤不要标 completed。）")
	return b.String()
}

// planCheckbox 是各状态的视觉标记。
func planCheckbox(status string) string {
	switch status {
	case PlanCompleted:
		return "[x]"
	case PlanInProgress:
		return "[>]"
	case PlanBlocked:
		return "[!]"
	default:
		return "[ ]"
	}
}

// nextPlanStep 返回下一个应处理的步骤：优先 in_progress，其次第一个 pending。
func nextPlanStep(p Plan) string {
	for _, s := range p.Steps {
		if s.Status == PlanInProgress {
			return s.Step
		}
	}
	for _, s := range p.Steps {
		if s.Status == PlanPending {
			return s.Step
		}
	}
	return ""
}

// planInjected 判断一条 system 消息是否是旧的瞬态计划块。
func planInjected(content string) bool {
	return strings.HasPrefix(content, planBlockPrefix)
}

// withPlanContext 在每轮模型调用前注入最新计划。
//
// 返回新切片，不修改调用方历史：瞬态状态不能落进 msgs，否则会一轮轮累积。
// 旧计划块（如果调用方历史里意外存在）会被先剔除，保证永远只有一份最新计划。
func withPlanContext(msgs []llm.Message, session string) []llm.Message {
	block := PlanBlock(session)
	var out []llm.Message
	for _, m := range msgs {
		if m.Role == llm.RoleSystem && planInjected(m.Content) {
			continue
		}
		out = append(out, m)
	}
	if block == "" {
		return out
	}
	return append(out, llm.Message{Role: llm.RoleSystem, Content: block})
}
