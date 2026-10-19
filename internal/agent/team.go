package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"blog-my/internal/llm"
)

// 团队层（v2）的工具层：具名成员、成员间消息、等待、共享任务板。
//
// 团队建在 v1 子 agent 委派之上——每个成员就是一个具名、可续跑的子会话，
// 所以 spawn_teammate 最终仍走 Delegator。这一层只做两件事：
//
//  1. 受理：校验参数、把请求翻译成 TeamService 调用，**立即**返回；
//  2. 讲清楚：把结果渲染成模型能扫的一行行文本，同时在 Display 里带结构化数据给前端。
//
// 权限不在这一层判：谁能建成员、谁能完成任务由 TeamService 按调用方判定。
// actor 原样用 Exec.SessionID 交给服务，服务内部再解析成成员名（契约 §1.1）。
// 冻结契约见 docs/agent/07-team.md §1.1 与 §2。

// 团队成员角色（TeamMember.Role 的取值）。
const (
	TeamRoleLead     = "lead"
	TeamRoleTeammate = "teammate"
)

// 团队成员状态（TeamMember.Status 的取值）。
//
// done / aborted 这类子 run 终态由 web 层统一映射成 inactive：对模型来说
// 「跑完了」就是「不在跑」，不需要知道收尾原因（那在 list_subagents 里）。
const (
	TeamStatusRunning  = "running"
	TeamStatusInactive = "inactive"
	TeamStatusFailed   = "failed"
)

// 任务状态（TeamTask.Status 的取值）。deleted 是墓碑：id 不复用，但默认不再列出。
const (
	TeamTaskPending    = "pending"
	TeamTaskInProgress = "in_progress"
	TeamTaskCompleted  = "completed"
	TeamTaskDeleted    = "deleted"
)

// teamTaskActions 是 team_task_update 允许的 action（冻结清单，docs/agent/07-team.md §1.1）。
var teamTaskActions = []string{
	"claim", "release", "complete", "reopen", "edit", "set_dependencies", "reassign", "delete",
}

// teamWaitMinMS / teamWaitMaxMS / teamWaitDefaultMS 是 wait_agent 的 timeout_ms 边界（冻结）。
const (
	teamWaitMinMS     = 10000
	teamWaitMaxMS     = 3600000
	teamWaitDefaultMS = 30000
)

// teamMemberNameRe 是成员名规则（冻结）：小写字母开头，后跟 1-31 个小写字母/数字/连字符。
// 也就是说名字长度 2-32；web 层会再校验一次（重名要查库），这里是给模型的早退报错。
var teamMemberNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// TeamMember 是 roster 里的一行。
type TeamMember struct {
	Name        string `json:"name"` // 小写短名，父会话内唯一，永不复用
	Description string `json:"description"`
	Role        string `json:"role"`   // lead | teammate
	Status      string `json:"status"` // running | inactive | failed
	Depth       int    `json:"depth"`
	ChildID     string `json:"child_id"` // 十进制串；lead 为空
	Model       string `json:"model"`
	Activity    string `json:"activity,omitempty"`
	Progress    string `json:"progress,omitempty"`
}

// TeamTask 是任务板上的一条。
type TeamTask struct {
	ID          string   `json:"id"`       // task-1、task-2…
	Revision    int      `json:"revision"` // CAS 用；每次变更 +1
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"`       // pending | in_progress | completed
	Owner       string   `json:"owner"`        // 成员名；空=无人认领
	BlockedBy   []string `json:"blocked_by"`   // 依赖的任务 id
	WriteScopes []string `json:"write_scopes"` // 提示性路径前缀，不加锁
	Ready       bool     `json:"ready"`        // 依赖是否都完成了
}

// TeamService 由 web 层实现（agent 包不能 import web）。
type TeamService interface {
	SpawnTeammate(ctx context.Context, req TeammateRequest) (TeamMember, error)
	Roster(ctx context.Context, parentSessionID string) ([]TeamMember, error)
	SendMessage(ctx context.Context, parentSessionID, from, target, message string) error
	WaitForChange(ctx context.Context, parentSessionID string, timeoutMS int) (changed bool, err error)
	CreateTask(ctx context.Context, parentSessionID string, t TeamTaskInput) (TeamTask, error)
	ListTasks(ctx context.Context, parentSessionID string, filter TaskFilter) ([]TeamTask, error)
	GetTask(ctx context.Context, parentSessionID, id string) (TeamTask, error)
	UpdateTask(ctx context.Context, parentSessionID, id, actor string, expectedRevision int, action string, patch TeamTaskInput) (TeamTask, error)
}

// TeammateRequest 是一次建成员请求。
type TeammateRequest struct {
	ParentSessionID         string
	RootSessionID           string
	Depth                   int
	Name                    string
	Description             string
	Prompt                  string
	Context                 string // fresh | fork
	History                 []llm.Message
	Model, Kind, Root, Mode string
	Roots                   []string
}

// TeamTaskInput 是建任务 / 更新任务的载荷。
type TeamTaskInput struct {
	Subject, Description string
	BlockedBy            []string
	WriteScopes          []string
	Owner                string
}

// TaskFilter 是任务列表的过滤条件。零值 = 不过滤。
type TaskFilter struct {
	Status string // 空=全部
	Owner  string
	Ready  *bool
}

// ---------------------------------------------------------------------------
// 公共接线与小工具
// ---------------------------------------------------------------------------

// teamToolParent 解析团队所属会话。
//
// web 层在组装本轮工具时可以显式填 ParentSessionID（团队成员的会话不是团队根，
// 但团队工具必须作用于整个团队）；没填就回落到 Exec.SessionID——对 Lead 来说两者相同，
// 对成员来说服务会再把它解析回团队（服务拿到的永远是「某个会话 id」，这是它的输入契约）。
func teamToolParent(explicit string, ex *Exec) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if ex == nil {
		return ""
	}
	return strings.TrimSpace(ex.SessionID)
}

// teamToolActor 是这次调用的调用方会话 id（服务内部解析成成员名）。
func teamToolActor(ex *Exec) string {
	if ex == nil {
		return ""
	}
	return strings.TrimSpace(ex.SessionID)
}

// teamSessionMissingResult 是「拿不到会话 id」时的统一报错。
// 团队工具全都按会话定位团队与成员，没有会话就没法工作——这是接线问题，不是参数问题。
func teamSessionMissingResult() Result {
	return Result{IsError: true, Content: "拿不到当前会话 id，团队工具暂时不可用。"}
}

// decodeTeamArgs 解析工具参数；空参数（有些后端对无参调用会发空字符串）当成 {}。
func decodeTeamArgs(raw json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	return json.Unmarshal(raw, dst)
}

// teamMemberDisplay 是 roster 行的 Display 形状（与 HTTP 接口 §1.3 对齐）。
func teamMemberDisplay(m TeamMember) map[string]any {
	return map[string]any{
		"name":        m.Name,
		"description": m.Description,
		"role":        m.Role,
		"status":      m.Status,
		"depth":       m.Depth,
		"child_id":    m.ChildID,
		"model":       m.Model,
		"activity":    m.Activity,
		"progress":    m.Progress,
	}
}

// teamTaskDisplay 是任务行的 Display 形状（与 HTTP 接口 §1.3 对齐）。
// nil 切片统一成空数组：JSON 里的 null 会让前端多一个判空分支。
func teamTaskDisplay(t TeamTask) map[string]any {
	blocked := t.BlockedBy
	if blocked == nil {
		blocked = []string{}
	}
	scopes := t.WriteScopes
	if scopes == nil {
		scopes = []string{}
	}
	return map[string]any{
		"id":           t.ID,
		"revision":     t.Revision,
		"subject":      t.Subject,
		"description":  t.Description,
		"status":       t.Status,
		"owner":        t.Owner,
		"blocked_by":   blocked,
		"write_scopes": scopes,
		"ready":        t.Ready,
	}
}

// teamStatusLabel 把成员状态翻成给人看的中文。
func teamStatusLabel(status string) string {
	switch strings.TrimSpace(status) {
	case TeamStatusRunning:
		return "运行中"
	case TeamStatusInactive:
		return "已结束"
	case TeamStatusFailed:
		return "失败"
	default:
		if s := strings.TrimSpace(status); s != "" {
			return "状态未知（" + s + "）"
		}
		return "状态未知"
	}
}

// teamTaskStatusLabel 把任务状态翻成给人看的中文。
func teamTaskStatusLabel(status string) string {
	switch strings.TrimSpace(status) {
	case TeamTaskPending:
		return "待办"
	case TeamTaskInProgress:
		return "进行中"
	case TeamTaskCompleted:
		return "已完成"
	case TeamTaskDeleted:
		return "已删除"
	default:
		if s := strings.TrimSpace(status); s != "" {
			return "状态未知（" + s + "）"
		}
		return "状态未知"
	}
}

// teamActionLabel 把 action 翻成给人看的中文（回执文案用）。
func teamActionLabel(action string) string {
	switch strings.TrimSpace(action) {
	case "claim":
		return "认领"
	case "release":
		return "释放"
	case "complete":
		return "完成"
	case "reopen":
		return "重新打开"
	case "edit":
		return "修改"
	case "set_dependencies":
		return "更新依赖"
	case "reassign":
		return "改派"
	case "delete":
		return "删除"
	default:
		return strings.TrimSpace(action)
	}
}

// teamMemberNameOr 给空名字一个可读兜底，避免名单里出现空行。
func teamMemberNameOr(name string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return "未命名成员"
}

// teamMemberRowText 把一条成员渲染成一行（一行一条，模型扫起来最省 token）。
func teamMemberRowText(m TeamMember) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s [%s] %s", teamMemberNameOr(m.Name), teamStatusLabel(m.Status), teamRoleOr(m.Role))
	if desc := clipRunes(firstLine(m.Description), 120); desc != "" {
		fmt.Fprintf(&b, "：%s", desc)
	}
	return b.String()
}

func teamRoleOr(role string) string {
	if s := strings.TrimSpace(role); s != "" {
		return s
	}
	return TeamRoleTeammate
}

// teamTaskSummary 是任务的一句话摘要（列表行与更新回执共用）。
func teamTaskSummary(t TeamTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s", teamTaskIDOr(t.ID), teamTaskStatusLabel(t.Status), clipRunes(firstLine(t.Subject), 80))
	if owner := strings.TrimSpace(t.Owner); owner != "" {
		fmt.Fprintf(&b, "（owner: %s）", owner)
	}
	if len(t.BlockedBy) > 0 && !t.Ready {
		fmt.Fprintf(&b, "（依赖未完成：%s）", strings.Join(t.BlockedBy, ", "))
	}
	return b.String()
}

// teamTaskRowText 把一条任务渲染成列表里的一行。
func teamTaskRowText(t TeamTask) string { return "- " + teamTaskSummary(t) }

func teamTaskIDOr(id string) string {
	if s := strings.TrimSpace(id); s != "" {
		return s
	}
	return "（无 id）"
}

// teamTaskDetailText 是 team_task_get 的正文：把 CAS 需要的 revision 和
// 「下一步能不能 claim」都写明白，模型不用再猜。
func teamTaskDetailText(t TeamTask) string {
	var b strings.Builder
	b.WriteString(teamTaskSummary(t))
	b.WriteString("\n")
	fmt.Fprintf(&b, "revision: %d（更新时用它做 expected_revision）\n", t.Revision)
	if owner := strings.TrimSpace(t.Owner); owner != "" {
		fmt.Fprintf(&b, "owner: %s\n", owner)
	} else {
		b.WriteString("owner: （无人认领）\n")
	}
	switch {
	case len(t.BlockedBy) == 0:
		b.WriteString("依赖: 无\n")
	case t.Ready:
		fmt.Fprintf(&b, "依赖: %s（都已满足）\n", strings.Join(t.BlockedBy, ", "))
	default:
		fmt.Fprintf(&b, "依赖: %s（还有未完成的，现在不能 claim）\n", strings.Join(t.BlockedBy, ", "))
	}
	if len(t.WriteScopes) > 0 {
		fmt.Fprintf(&b, "写范围: %s（只是提示，不加锁）\n", strings.Join(t.WriteScopes, ", "))
	}
	b.WriteString("描述:\n")
	if desc := strings.TrimSpace(t.Description); desc != "" {
		b.WriteString(desc)
	} else {
		b.WriteString("（无描述）")
	}
	return b.String()
}

// normalizeScopes 去掉写范围里的空白项（它只是提示，空项没有意义）。
func normalizeScopes(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if v := strings.TrimSpace(s); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// normalizeBlockedBy 规整依赖 id：空项直接报错（它一定会查不到，早说比晚说清楚）。
func normalizeBlockedBy(in []string) ([]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		v := strings.TrimSpace(s)
		if v == "" {
			return nil, fmt.Errorf("blocked_by 里的任务 id 不能为空")
		}
		out = append(out, v)
	}
	return out, nil
}

// isStaleRevisionError 判断服务报的是不是 CAS 冲突。
//
// 契约里这个错误叫 TEAM_TASK_STALE_REVISION（docs/agent/07-team.md §2 第 5 条），
// 但工具层拿到的是 error，按文案识别；识别不出来就走通用文案（宁可少提示一次，
// 也不要把普通错误伪装成 CAS 冲突）。
func isStaleRevisionError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "stale") || strings.Contains(s, "过期") {
		return true
	}
	if !strings.Contains(s, "revision") {
		return false
	}
	for _, needle := range []string{"冲突", "不匹配", "mismatch", "已经变", "已被改"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// spawn_teammate
// ---------------------------------------------------------------------------

// SpawnTeammateTool 建一个具名成员。
//
// 只有 Lead（根会话）能建成员——这条由 TeamService 在执行时判定（工具层没有会话树信息）；
// 工具层能先判的是参数本身：名字规则、必填项、context 取值、深度。
type SpawnTeammateTool struct {
	Service TeamService

	// 下面几个字段 Exec 里没有，由 web 层在组装本轮工具时填充（与 SubagentTool 同义）；
	// 缺省回落到 Exec 的会话 / 工作区，保证「忘填」也不会把成员挂到空会话上。
	ParentSessionID string
	RootSessionID   string
	History         []llm.Message
	Model           string
	Kind            string
	Root            string
	Roots           []string
	Mode            string

	Depth    int // 调用方（Lead）的深度；成员深度 = Depth + 1
	MaxDepth int // 允许的最大深度；<=0 表示这里不判，交给服务端把关
}

func (SpawnTeammateTool) Name() string { return "spawn_teammate" }

func (SpawnTeammateTool) Description() string {
	return "建一个具名成员（teammate），把一件自足的任务派给它后台跑，立即返回。" +
		"只在用户明确要求「组队 / 分工 / 多个成员」时才用；普通任务不要自己组队，用 subagent 或自己完成。" +
		"只有 Lead（根会话）能建成员；名字小写、团队内唯一且永不复用，重名或非法会报错。" +
		"建完用 list_agents 看状态、send_message 追加指令、wait_agent 等变化。"
}

func (SpawnTeammateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string",
				"description": "成员名：小写字母开头，只能含小写字母/数字/连字符，2-32 个字符（如 alice、front-end）。" +
					"团队内唯一且永不复用，重名或非法会报错；创建失败的名字也会被占住。",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "一句话说明这个成员的职责（会出现在成员名单和团队卡片上）。",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "交给成员的完整任务，必须自足；context=fresh 时它看不到你和用户的对话，不要写「上面那个」这类指代。",
			},
			"context": map[string]any{
				"type": "string",
				"enum": []string{SubagentContextFresh, SubagentContextFork},
				"description": "fresh（默认）：成员只看 prompt；fork：附带当前已完成对话作为上下文。" +
					"不确定就用 fresh，把背景写进 prompt 里。",
			},
		},
		"required": []string{"name", "description", "prompt"},
	}
}

type spawnTeammateArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Context     string `json:"context"`
}

func (t SpawnTeammateTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，建不了成员；请自己直接完成这件事。"}, nil
	}
	// 深度检查放在参数校验之前：这一层根本不能再建成员时，先告诉模型「别在这上面花力气」。
	if t.MaxDepth > 0 && t.Depth+1 > t.MaxDepth {
		return Result{IsError: true, Content: fmt.Sprintf("成员最多嵌套 %d 层，这一层不能再建成员。", t.MaxDepth)}, nil
	}

	var a spawnTeammateArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("spawn_teammate 的参数不是合法 JSON：%v", err)}, nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return Result{IsError: true, Content: "缺少必填参数 name：给成员起一个小写短名（如 alice、front-end），团队内唯一。"}, nil
	}
	if !teamMemberNameRe.MatchString(name) {
		return Result{IsError: true, Content: fmt.Sprintf(
			"参数 name %q 不合法：要求小写字母开头、只含小写字母/数字/连字符、长度 2-32（如 alice、front-end）。", a.Name)}, nil
	}
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		return Result{IsError: true, Content: "缺少必填参数 description：用一句话说明这个成员负责什么。"}, nil
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return Result{IsError: true, Content: "缺少必填参数 prompt：请把要交给这个成员的完整任务写清楚（默认它看不到你和用户的对话）。"}, nil
	}
	ctxMode := strings.ToLower(strings.TrimSpace(a.Context))
	if ctxMode == "" {
		ctxMode = SubagentContextFresh
	}
	if ctxMode != SubagentContextFresh && ctxMode != SubagentContextFork {
		return Result{IsError: true, Content: fmt.Sprintf(
			"参数 context 只能是 fresh 或 fork（收到的是 %q）：fresh 表示成员只看 prompt，fork 表示附带当前对话上下文。", a.Context)}, nil
	}

	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}
	rootSession := strings.TrimSpace(t.RootSessionID)
	if rootSession == "" {
		rootSession = parent
	}
	root := strings.TrimSpace(t.Root)
	if root == "" && ex != nil {
		root = strings.TrimSpace(ex.Root)
	}
	history := t.History
	if ctxMode == SubagentContextFresh {
		// 契约：fresh 不带父级历史。显式清空而不是靠调用方自觉。
		history = nil
	}

	member, err := t.Service.SpawnTeammate(ctx, TeammateRequest{
		ParentSessionID: parent,
		RootSessionID:   rootSession,
		Depth:           t.Depth,
		Name:            name,
		Description:     desc,
		Prompt:          prompt,
		Context:         ctxMode,
		History:         history,
		Model:           t.Model,
		Kind:            t.Kind,
		Root:            root,
		Roots:           append([]string(nil), t.Roots...),
		Mode:            t.Mode,
	})
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf(
			"建成员失败：%s。你可以换个名字重试，或自己直接做这件事。", err.Error())}, nil
	}
	if strings.TrimSpace(member.Name) == "" {
		member.Name = name
	}
	return Result{
		Content: fmt.Sprintf("已创建成员 %s（%s，%s）。用 list_agents 看状态、send_message 追加指令。",
			teamMemberNameOr(member.Name), teamRoleOr(member.Role), teamStatusLabel(member.Status)),
		Display: map[string]any{"team_member": teamMemberDisplay(member)},
	}, nil
}

// ---------------------------------------------------------------------------
// send_message
// ---------------------------------------------------------------------------

// SendMessageTool 给成员发消息。
//
// 回执措辞刻意把「已收下 ≠ 已处理」写在正文里：模型很容易把发送成功当成对方已完成，
// 然后向用户汇报一个还不存在的结果（契约 §2 第 2 条）。
type SendMessageTool struct {
	Service         TeamService
	ParentSessionID string
}

func (SendMessageTool) Name() string { return "send_message" }

func (SendMessageTool) Description() string {
	return "给团队成员发一条消息，进它的收件箱。成功只表示「已收下」，不代表对方已经处理——" +
		"要看结果得自己用 list_agents 看状态、wait_agent 等变化。" +
		"对方正在跑：消息排队，它读到就会处理；对方已结束：用它原来的会话冷启动续跑一轮。" +
		"成员之间可以互发消息；target 用 list_agents 里的成员名。"
}

func (SendMessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "收件成员的名字（list_agents 返回的名字，如 alice）；不存在或不是本团队成员会报错。",
			},
			"message": map[string]any{
				"type": "string",
				"description": "要发给它的完整消息：写清楚你要它做什么、以及必要的背景，别写「上面那个」这类指代。" +
					"发出只代表已收下，对方处理完不会自动回你。",
			},
		},
		"required": []string{"target", "message"},
	}
}

type sendMessageArgs struct {
	Target  string `json:"target"`
	Message string `json:"message"`
}

func (t SendMessageTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，发不了成员消息。"}, nil
	}
	var a sendMessageArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("send_message 的参数不是合法 JSON：%v", err)}, nil
	}
	target := strings.TrimSpace(a.Target)
	if target == "" {
		return Result{IsError: true, Content: "缺少必填参数 target：请写要发给哪个成员（用 list_agents 里的名字）。"}, nil
	}
	message := strings.TrimSpace(a.Message)
	if message == "" {
		return Result{IsError: true, Content: "缺少必填参数 message：请把要发给对方的内容写清楚。"}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	from := teamToolActor(ex)
	if parent == "" || from == "" {
		return teamSessionMissingResult(), nil
	}

	if err := t.Service.SendMessage(ctx, parent, from, target, message); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("发消息失败：%s", err.Error())}, nil
	}
	return Result{
		Content: fmt.Sprintf("已把消息放进 %s 的收件箱（只代表已收下，不代表它已经处理）。"+
			"要确认结果：先 wait_agent 等变化，再 list_agents 看状态。", target),
		Display: map[string]any{"team_message": map[string]any{
			"target":    target,
			"from":      from,
			"delivered": true,
		}},
	}, nil
}

// ---------------------------------------------------------------------------
// list_agents
// ---------------------------------------------------------------------------

// ListAgentsTool 让模型回看团队名单。只读工具：重复调用没有任何后果。
type ListAgentsTool struct {
	Service         TeamService
	ParentSessionID string
}

func (ListAgentsTool) Name() string { return "list_agents" }

func (ListAgentsTool) Description() string {
	return "列出团队成员（含 Lead）：名字、角色、状态（运行中/已结束/失败）和职责。" +
		"想知道谁在跑、谁跑完了、该给谁发消息时用它。只读，可以随时重复调用。"
}

func (ListAgentsTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t ListAgentsTool) Execute(ctx context.Context, _ json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，看不了成员名单。"}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}
	members, err := t.Service.Roster(ctx, parent)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("查成员名单失败：%s", err.Error())}, nil
	}
	display := make([]map[string]any, 0, len(members))
	for _, m := range members {
		display = append(display, teamMemberDisplay(m))
	}
	if len(members) == 0 {
		return Result{
			Content: "当前没有团队成员。",
			Display: map[string]any{"team": display},
		}, nil
	}
	var b strings.Builder
	for _, m := range members {
		b.WriteString(teamMemberRowText(m))
		b.WriteString("\n")
	}
	return Result{
		Content: strings.TrimRight(b.String(), "\n"),
		Display: map[string]any{"team": display},
	}, nil
}

// ---------------------------------------------------------------------------
// wait_agent
// ---------------------------------------------------------------------------

// WaitAgentTool 等「下一次变化」。
//
// 它刻意只报有没有变化、不报变了什么：调用方拿到 true 之后要自己重新 list_agents，
// 这样工具结果永远是一个不会过期的判断，天然免疫轮询（契约 §2 第 4 条）。
type WaitAgentTool struct {
	Service         TeamService
	ParentSessionID string
}

func (WaitAgentTool) Name() string { return "wait_agent" }

func (WaitAgentTool) Description() string {
	return "等团队的下一次变化：成员状态变了就返回。它只报「有没有变化」，不报变了什么——" +
		"返回后要自己用 list_agents / team_task_list 重新读最新状态。" +
		"没有活跃成员时会立即返回「没有变化」。没有变化不是错误，不要用它空转轮询。"
}

func (WaitAgentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timeout_ms": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf(
					"最长等待毫秒数，范围 %d..%d，默认 %d。到时间没有任何变化就返回「没有变化」。",
					teamWaitMinMS, teamWaitMaxMS, teamWaitDefaultMS),
			},
		},
	}
}

type waitAgentArgs struct {
	TimeoutMS *int `json:"timeout_ms"`
}

func (t WaitAgentTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，等不了成员变化。"}, nil
	}
	var a waitAgentArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("wait_agent 的参数不是合法 JSON：%v", err)}, nil
	}
	timeout := teamWaitDefaultMS
	if a.TimeoutMS != nil {
		timeout = *a.TimeoutMS
		if timeout < teamWaitMinMS || timeout > teamWaitMaxMS {
			return Result{IsError: true, Content: fmt.Sprintf(
				"参数 timeout_ms 超出范围：允许 %d..%d 毫秒（收到 %d）。", teamWaitMinMS, teamWaitMaxMS, timeout)}, nil
		}
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}
	changed, err := t.Service.WaitForChange(ctx, parent, timeout)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("等待成员变化失败：%s", err.Error())}, nil
	}
	display := map[string]any{"team_wait": map[string]any{"changed": changed, "timeout_ms": timeout}}
	if !changed {
		return Result{
			Content: "等待结束：没有检测到任何变化（也可能是当前没有活跃成员，直接返回）。" +
				"没有变化不是错误——先用 list_agents 看当前状态，再决定要不要继续等。",
			Display: display,
		}, nil
	}
	return Result{
		Content: "团队有变化：请用 list_agents 和 team_task_list 重新读一遍最新状态。",
		Display: display,
	}, nil
}

// ---------------------------------------------------------------------------
// team_task_create
// ---------------------------------------------------------------------------

// TeamTaskCreateTool 在共享任务板上建一条 pending 任务。
type TeamTaskCreateTool struct {
	Service         TeamService
	ParentSessionID string
}

func (TeamTaskCreateTool) Name() string { return "team_task_create" }

func (TeamTaskCreateTool) Description() string {
	return "在共享任务板上新建一条 pending 任务（完整快照 + revision）。" +
		"任务板是团队共享的：成员都能用 team_task_list 看、team_task_update 认领/完成；" +
		"每次更新都要带 expected_revision 做 CAS，防止互相覆盖。" +
		"blocked_by 只能指向已存在且未删除的任务；依赖没完成前不能 claim。" +
		"write_scopes 只是提示性的路径前缀，不加锁、不拦写入。"
}

func (TeamTaskCreateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subject": map[string]any{
				"type":        "string",
				"description": "任务标题：一句话说清要做什么（会出现在任务列表里）。",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "任务正文：做完的标准、要交什么，写清楚，接手的成员只凭它开工。",
			},
			"blocked_by": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "依赖的任务 id 列表（如 [\"task-1\"]）。必须指向任务板上已存在且未删除的任务；" +
					"依赖全部完成前，这条任务不能被 claim。",
			},
			"write_scopes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "预计会改动的路径前缀（如 [\"web/\", \"templates/\"]）。只是给别人的提示，不加锁、不拦写入。",
			},
		},
		"required": []string{"subject", "description"},
	}
}

type teamTaskCreateArgs struct {
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	BlockedBy   []string `json:"blocked_by"`
	WriteScopes []string `json:"write_scopes"`
}

func (t TeamTaskCreateTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，用不了共享任务板。"}, nil
	}
	var a teamTaskCreateArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("team_task_create 的参数不是合法 JSON：%v", err)}, nil
	}
	subject := strings.TrimSpace(a.Subject)
	if subject == "" {
		return Result{IsError: true, Content: "缺少必填参数 subject：用一句话说清这条任务要做什么。"}, nil
	}
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		return Result{IsError: true, Content: "缺少必填参数 description：把做完的标准和要交的东西写清楚，接手的成员只凭它开工。"}, nil
	}
	blocked, err := normalizeBlockedBy(a.BlockedBy)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("参数 blocked_by 不合法：%s", err.Error())}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}

	task, err := t.Service.CreateTask(ctx, parent, TeamTaskInput{
		Subject:     subject,
		Description: desc,
		BlockedBy:   blocked,
		WriteScopes: normalizeScopes(a.WriteScopes),
	})
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("建任务失败：%s", err.Error())}, nil
	}
	return Result{
		Content: fmt.Sprintf("已创建 %s（revision=%d）。认领用 team_task_update action=claim，并带上 expected_revision=%d。",
			teamTaskSummary(task), task.Revision, task.Revision),
		Display: map[string]any{"task": teamTaskDisplay(task)},
	}, nil
}

// ---------------------------------------------------------------------------
// team_task_list
// ---------------------------------------------------------------------------

// TeamTaskListTool 列共享任务板。
type TeamTaskListTool struct {
	Service         TeamService
	ParentSessionID string
}

func (TeamTaskListTool) Name() string { return "team_task_list" }

func (TeamTaskListTool) Description() string {
	return "列共享任务板上的任务，一行一条：id、状态、标题、owner、依赖是否就绪。" +
		"默认列出全部未删除任务，可用 status / owner / ready 过滤。" +
		"认领或更新任务前，先用它（或 team_task_get）拿到最新 revision。"
}

func (TeamTaskListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{TeamTaskPending, TeamTaskInProgress, TeamTaskCompleted},
				"description": "只看某个状态的任务；省略=全部（已删除的任务永远不列）。",
			},
			"owner": map[string]any{
				"type":        "string",
				"description": "只看某个成员名下的任务（如 alice）；省略=所有人。",
			},
			"ready": map[string]any{
				"type":        "boolean",
				"description": "true=只看依赖都已满足、可以开工的；false=只看还被依赖挡住的。省略=不过滤。",
			},
		},
	}
}

type teamTaskListArgs struct {
	Status string `json:"status"`
	Owner  string `json:"owner"`
	Ready  *bool  `json:"ready"`
}

func (t TeamTaskListTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，用不了共享任务板。"}, nil
	}
	var a teamTaskListArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("team_task_list 的参数不是合法 JSON：%v", err)}, nil
	}
	status := strings.TrimSpace(a.Status)
	switch status {
	case "", TeamTaskPending, TeamTaskInProgress, TeamTaskCompleted:
	default:
		return Result{IsError: true, Content: fmt.Sprintf(
			"参数 status 只能是 pending / in_progress / completed（收到的是 %q）；省略表示全部。", a.Status)}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}

	tasks, err := t.Service.ListTasks(ctx, parent, TaskFilter{
		Status: status,
		Owner:  strings.TrimSpace(a.Owner),
		Ready:  a.Ready,
	})
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("查任务失败：%s", err.Error())}, nil
	}
	display := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		display = append(display, teamTaskDisplay(task))
	}
	if len(tasks) == 0 {
		return Result{
			Content: "当前没有任务。",
			Display: map[string]any{"tasks": display},
		}, nil
	}
	var b strings.Builder
	for _, task := range tasks {
		b.WriteString(teamTaskRowText(task))
		b.WriteString("\n")
	}
	return Result{
		Content: strings.TrimRight(b.String(), "\n"),
		Display: map[string]any{"tasks": display},
	}, nil
}

// ---------------------------------------------------------------------------
// team_task_get
// ---------------------------------------------------------------------------

// TeamTaskGetTool 读一条任务的完整快照。
type TeamTaskGetTool struct {
	Service         TeamService
	ParentSessionID string
}

func (TeamTaskGetTool) Name() string { return "team_task_get" }

func (TeamTaskGetTool) Description() string {
	return "读一条任务的完整快照：状态、owner、依赖、写范围、描述和 revision。" +
		"team_task_update 必须带 expected_revision，所以每次更新前先用它读最新 revision。"
}

func (TeamTaskGetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "任务 id（形如 task-1，从 team_task_list / team_task_create 拿）。",
			},
		},
		"required": []string{"task_id"},
	}
}

type teamTaskGetArgs struct {
	TaskID string `json:"task_id"`
}

func (t TeamTaskGetTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，用不了共享任务板。"}, nil
	}
	var a teamTaskGetArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("team_task_get 的参数不是合法 JSON：%v", err)}, nil
	}
	id := strings.TrimSpace(a.TaskID)
	if id == "" {
		return Result{IsError: true, Content: "缺少必填参数 task_id：请写要读的任务 id（形如 task-1）。"}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	if parent == "" {
		return teamSessionMissingResult(), nil
	}
	task, err := t.Service.GetTask(ctx, parent, id)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("读任务失败：%s", err.Error())}, nil
	}
	return Result{
		Content: teamTaskDetailText(task),
		Display: map[string]any{"task": teamTaskDisplay(task)},
	}, nil
}

// ---------------------------------------------------------------------------
// team_task_update
// ---------------------------------------------------------------------------

// TeamTaskUpdateTool 用 CAS 更新任务。
//
// expected_revision 是必填的：拿到旧快照再提交会被服务拒绝（TEAM_TASK_STALE_REVISION），
// 这是为了两个成员不会互相覆盖成果（契约 §2 第 5 条）。被拒之后工具会提示先重读，
// 而不是让模型原样重试。
type TeamTaskUpdateTool struct {
	Service         TeamService
	ParentSessionID string
}

func (TeamTaskUpdateTool) Name() string { return "team_task_update" }

func (TeamTaskUpdateTool) Description() string {
	return "用 CAS 更新一条任务：必须带 expected_revision，和任务当前 revision 不一致会被拒绝（TEAM_TASK_STALE_REVISION），" +
		"防止两个成员互相覆盖成果；被拒后用 team_task_get 读最新快照再提交。" +
		"action：claim=认领（要求 pending 且依赖全完成）、release=放回待办、complete=完成（只有 owner 能完成）、" +
		"reopen=把已完成打回待办、edit=改标题/描述/写范围、set_dependencies=改依赖、reassign=换 owner、delete=删除（留墓碑）。"
}

func (TeamTaskUpdateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "任务 id（形如 task-1）。",
			},
			"expected_revision": map[string]any{
				"type": "integer",
				"description": "你拿到的任务快照里的 revision。和任务当前值不一致会被拒绝（CAS 冲突），" +
					"这时先 team_task_get 读最新快照再提交——不要原样重试。",
			},
			"action": map[string]any{
				"type": "string",
				"enum": teamTaskActions,
				"description": "要做的动作：claim=认领（要求 pending 且依赖全完成）、release=放回待办、complete=完成（只有 owner 能完成）、" +
					"reopen=把已完成打回待办、edit=改标题/描述/写范围、set_dependencies=改依赖、reassign=换 owner、delete=删除（留墓碑）。",
			},
			"subject": map[string]any{
				"type":        "string",
				"description": "action=edit 时改任务标题；其它 action 忽略。",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "action=edit 时改任务正文；其它 action 忽略。",
			},
			"blocked_by": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "action=set_dependencies 时替换依赖列表；必须是已存在且未删除的任务 id。" +
					"传空数组 [] 表示清空依赖；其它 action 忽略。",
			},
			"write_scopes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "action=edit 时替换写范围（提示性路径前缀，不加锁）；其它 action 忽略。",
			},
			"owner": map[string]any{
				"type":        "string",
				"description": "action=reassign 时把任务改派给这个成员名；其它 action 忽略（claim 认领人就是调用者自己）。",
			},
		},
		"required": []string{"task_id", "expected_revision", "action"},
	}
}

type teamTaskUpdateArgs struct {
	TaskID           string   `json:"task_id"`
	ExpectedRevision *int     `json:"expected_revision"`
	Action           string   `json:"action"`
	Subject          string   `json:"subject"`
	Description      string   `json:"description"`
	BlockedBy        []string `json:"blocked_by"`
	WriteScopes      []string `json:"write_scopes"`
	Owner            string   `json:"owner"`
}

func (t TeamTaskUpdateTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Service == nil {
		return Result{IsError: true, Content: "这个环境没接团队能力，用不了共享任务板。"}, nil
	}
	var a teamTaskUpdateArgs
	if err := decodeTeamArgs(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("team_task_update 的参数不是合法 JSON：%v", err)}, nil
	}
	id := strings.TrimSpace(a.TaskID)
	if id == "" {
		return Result{IsError: true, Content: "缺少必填参数 task_id：请写要更新的任务 id（形如 task-1）。"}, nil
	}
	if a.ExpectedRevision == nil {
		return Result{IsError: true, Content: "缺少必填参数 expected_revision：先用 team_task_get 读任务最新快照，把它里面的 revision 填进来。"}, nil
	}
	rev := *a.ExpectedRevision
	if rev < 0 {
		return Result{IsError: true, Content: fmt.Sprintf("参数 expected_revision 不能为负数（收到 %d）：它是任务快照里的 revision。", rev)}, nil
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		return Result{IsError: true, Content: "缺少必填参数 action：可选 claim / release / complete / reopen / edit / set_dependencies / reassign / delete。"}, nil
	}
	if !containsString(teamTaskActions, action) {
		return Result{IsError: true, Content: fmt.Sprintf(
			"参数 action %q 不合法：只能是 %s。", a.Action, strings.Join(teamTaskActions, " / "))}, nil
	}
	owner := strings.TrimSpace(a.Owner)
	if action == "reassign" && owner == "" {
		return Result{IsError: true, Content: "action=reassign 需要同时给 owner（要改派给谁）；只想放回待办用 release。"}, nil
	}
	blocked, err := normalizeBlockedBy(a.BlockedBy)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("参数 blocked_by 不合法：%s", err.Error())}, nil
	}
	parent := teamToolParent(t.ParentSessionID, ex)
	actor := teamToolActor(ex)
	if parent == "" || actor == "" {
		return teamSessionMissingResult(), nil
	}

	task, err := t.Service.UpdateTask(ctx, parent, id, actor, rev, action, TeamTaskInput{
		Subject:     strings.TrimSpace(a.Subject),
		Description: strings.TrimSpace(a.Description),
		BlockedBy:   blocked,
		WriteScopes: normalizeScopes(a.WriteScopes),
		Owner:       owner,
	})
	if err != nil {
		if isStaleRevisionError(err) {
			return Result{IsError: true, Content: fmt.Sprintf(
				"更新被拒绝：%s 的 revision 已经不是 %d（CAS 冲突 TEAM_TASK_STALE_REVISION）。"+
					"这是为了不让两个人互相覆盖成果——先 team_task_get 读最新快照，再按新 revision 重新提交。",
				id, rev)}, nil
		}
		return Result{IsError: true, Content: fmt.Sprintf("更新任务失败：%s", err.Error())}, nil
	}
	return Result{
		Content: fmt.Sprintf("已%s：%s（revision=%d）。", teamActionLabel(action), teamTaskSummary(task), task.Revision),
		Display: map[string]any{"task": teamTaskDisplay(task)},
	}, nil
}

// containsString 判断字符串是否在列表里（action 校验用）。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
