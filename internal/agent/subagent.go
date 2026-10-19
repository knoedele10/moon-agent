package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/llm"
)

// 子 agent 委派（v1）的工具层。
//
// 这一层只做「受理」：把一次委派翻译成 SubagentRequest 交给 Delegator，**立即**返回回执。
// 为什么不在这里等子任务跑完：父级模型还在同一轮工具循环里，等它会白白占住父级的
// 请求与工具预算；完成通知由 web 层在子轮次结束后写回父会话（契约 §3.3）。
// 冻结契约见 docs/agent/06-subagent.md §3.1。

// 子任务状态（SubagentInfo.Status / SubagentNotice.Status 的取值）。
const (
	SubagentStatusRunning = "running"
	SubagentStatusDone    = "done"
	SubagentStatusFailed  = "failed"
	SubagentStatusAborted = "aborted"
)

// 子任务的上下文模式（SubagentRequest.Context 的取值）。
const (
	SubagentContextFresh = "fresh"
	SubagentContextFork  = "fork"
)

// SubagentRequest 是一次委派请求。
type SubagentRequest struct {
	ParentSessionID string // 父会话 id（十进制串）
	RootSessionID   string // 整棵树的根会话 id（容量按它统计）
	Depth           int    // 父级深度；子级深度 = Depth + 1
	Label           string // 3-5 个词的标签，用于卡片标题
	// Name 是团队里的成员名（小写短名，父会话内唯一，永不复用）。
	// 只有 spawn_teammate 会填；普通 subagent 委派留空。
	Name    string
	Prompt  string        // 交给子 agent 的完整任务（必须自足）
	Context string        // "fresh" | "fork"
	History []llm.Message // fork 时父级已完成轮次的前缀；fresh 为空
	Model   string        // 子级模型（继承父级）
	Kind    string        // chat/work/code/learn（继承父级）
	Root    string        // 工作区根目录（继承父级）
	Roots   []string      // 其他已启用工作区目录
	Mode    string        // 父级档位；子级据此决定风险工具是否直接拒绝
}

// SubagentHandle 是「已受理」回执。
type SubagentHandle struct {
	ChildID string // 子会话 id（十进制串）
	Label   string
	Depth   int
}

// SubagentInfo 是 list_subagents 返回的一行。
type SubagentInfo struct {
	ChildID    string
	Label      string
	Status     string // running | done | failed | aborted
	StopReason string
	Depth      int
	Reply      string // 收尾文本（已截断，最多 2000 字）
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Delegator 由 web 层注入（agent 包不能 import web）。
type Delegator interface {
	Delegate(ctx context.Context, req SubagentRequest) (SubagentHandle, error)
	ListSubagents(ctx context.Context, parentSessionID string) ([]SubagentInfo, error)
}

// SubagentNotice 是一条完成通知的内容。
type SubagentNotice struct {
	ChildID    string `json:"child_id"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	Reply      string `json:"reply,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// LLMText 把通知渲染成喂给父级模型的文本。
//
// 父模型只凭这一段决定怎么汇总，所以四件事都必须自解释：这是谁、什么状态、跑了多久、
// 结论原文。格式是冻结契约（docs/agent/06-subagent.md §3.1），不要随手改措辞。
func (n SubagentNotice) LLMText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "【子任务完成】%s（id=%s）\n", subagentLabelOr(n.Label), n.ChildID)
	fmt.Fprintf(&b, "状态：%s\n", n.statusText())
	fmt.Fprintf(&b, "耗时：%.1f 秒\n", float64(n.DurationMS)/1000)
	b.WriteString("它的结论：\n")
	if strings.TrimSpace(n.Reply) == "" {
		// 「没有结论」和「结论是空字符串」对父模型是两件事，必须明说，否则它会以为通知被截断了。
		b.WriteString("（它没有留下结论）")
	} else {
		b.WriteString(n.Reply)
	}
	return b.String()
}

// statusText 是通知里的状态行文案。
func (n SubagentNotice) statusText() string {
	if n.Status == SubagentStatusFailed {
		// 失败必须带上原因：父模型要靠它决定「换个做法重派」还是「自己接手」。
		reason := strings.TrimSpace(n.Error)
		if reason == "" {
			reason = strings.TrimSpace(n.StopReason)
		}
		if reason == "" {
			return "失败"
		}
		return "失败（原因：" + clipRunes(reason, 200) + "）"
	}
	return subagentStatusLabel(n.Status, n.StopReason)
}

// subagentStatusLabel 把状态（+ 收尾原因）翻成给人看的中文。
//
// 「达到输出上限」单列出来：它既不是正常完成，也不是失败——结论被截断了，
// 父模型看到这个词才知道要不要重派或追问，而不是把半截结论当完整的用。
func subagentStatusLabel(status, stopReason string) string {
	stop := strings.TrimSpace(stopReason)
	switch status {
	case SubagentStatusDone:
		if isOutputLimitStop(stop) {
			return "达到输出上限"
		}
		return "完成"
	case SubagentStatusFailed:
		return "失败"
	case SubagentStatusAborted:
		if isOutputLimitStop(stop) {
			return "达到输出上限"
		}
		return "被中止"
	case SubagentStatusRunning:
		return "运行中"
	default:
		if strings.TrimSpace(status) == "" {
			return "状态未知"
		}
		return "状态未知（" + status + "）"
	}
}

// isOutputLimitStop 判断收尾原因是不是「输出触顶」。
//
// 判据刻意放宽到多个常见拼写：agent.Run 的触顶、llm 后端的 finish_reason、
// web 层自写的 stopReason 都可能落在这一族里。宁可多认几个别名，也不能让父模型
// 把「被截断的结论」当成完整结论。
func isOutputLimitStop(reason string) bool {
	s := strings.ToLower(strings.TrimSpace(reason))
	if s == "" {
		return false
	}
	for _, needle := range []string{
		"max_tokens", "max-tokens", "max_token", "max-token",
		"output_limit", "output-limit", "max_output", "max-output",
		"token_limit", "token-limit", "length", "输出上限", "长度上限",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// subagentLabelOr 给空标签一个可读的兜底，避免通知/卡片上出现「（id=12）」这种裸标题。
func subagentLabelOr(label string) string {
	if s := strings.TrimSpace(label); s != "" {
		return s
	}
	return "未命名子任务"
}

// clipLabel 把标签压到 120 字以内：run 行的 label 列宽就是 120（契约 §3.2），
// 超长标签会让收尾落库失败；而模型给的标签本来就该是 3-5 个词。
func clipLabel(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 120 {
		return string(r)
	}
	return string(r[:120])
}

// ---------------------------------------------------------------------------
// subagent 工具
// ---------------------------------------------------------------------------

// SubagentTool 是委派工具：受理后立即返回，不等子 agent 跑完。
//
// 参数校验、深度检查、Delegator 缺失都折成 IsError 的 Result 而不是 error——
// 报错是给模型看的教学材料，抛 error 会打断循环（契约 §3.5 的失败语义）。
type SubagentTool struct {
	Delegator Delegator
	Depth     int    // 调用方（父级）的深度；子级深度 = Depth + 1
	MaxDepth  int    // 允许的最大深度（AGENT_MAX_DEPTH）
	Mode      string // 父级档位，原样传给子级

	// 下面几个字段 Exec 里没有，由 web 层在组装本轮工具时填充。
	// ParentSessionID / Root 为空时回落到 Exec 的 SessionID / Root，
	// 保证「忘填」也不会把子任务挂到空会话或空工作区上。
	ParentSessionID string
	RootSessionID   string
	History         []llm.Message
	Model           string
	Kind            string
	Root            string
	Roots           []string
}

func (SubagentTool) Name() string { return "subagent" }

func (SubagentTool) Description() string {
	return "把一个自足的子任务派给子 agent 在后台跑，立即返回回执，不阻塞当前对话。" +
		"适合能并行拆开的独立工作（分别总结几份文档、分头调研几个问题）。" +
		"prompt 必须写全：默认（context=fresh）子 agent 看不到你和用户的对话，" +
		"不要写「上面那个文件」这类指代；确实需要父级上下文时才用 context=fork。" +
		"子 agent 跑完会自动把结论通知你，不要在这里等它。"
}

func (SubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description": map[string]any{
				"type":        "string",
				"description": "3-5 个词的短标签，给这个子任务起名（会出现在卡片和完成通知里）",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "交给子 agent 的完整任务，必须自足；它默认看不到当前对话",
			},
			"context": map[string]any{
				"type":        "string",
				"enum":        []string{"fresh", "fork"},
				"description": "fresh（默认）：子 agent 只看 prompt；fork：附带父级已完成对话作为上下文",
			},
		},
		"required": []string{"description", "prompt"},
	}
}

// subagentArgs 是 subagent 工具的参数。
type subagentArgs struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Context     string `json:"context"`
}

func (t SubagentTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Delegator == nil {
		return Result{IsError: true, Content: "这个环境没接子 agent 能力，派不了子任务；请自己直接完成这件事。"}, nil
	}
	// 深度检查放在参数校验之前：这一层根本不能派活时，先告诉模型「别在这上面花力气」，
	// 它才会把整件事写进自己的回复里。
	if t.Depth+1 > t.MaxDepth {
		return Result{IsError: true, Content: fmt.Sprintf("子 agent 最多嵌套 %d 层，这一层不能再派活。", t.MaxDepth)}, nil
	}

	var a subagentArgs
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}") // 有些后端对无参调用会发空字符串，兜一下
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("subagent 的参数不是合法 JSON：%v", err)}, nil
	}
	label := strings.TrimSpace(a.Description)
	if label == "" {
		return Result{IsError: true, Content: "缺少必填参数 description：请给子任务起一个 3-5 个词的标签，方便在卡片和通知里认出它。"}, nil
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return Result{IsError: true, Content: "缺少必填参数 prompt：请把要交给子 agent 的完整任务写清楚（它默认看不到你和用户的对话）。"}, nil
	}
	ctxMode := strings.ToLower(strings.TrimSpace(a.Context))
	if ctxMode == "" {
		ctxMode = SubagentContextFresh
	}
	if ctxMode != SubagentContextFresh && ctxMode != SubagentContextFork {
		return Result{IsError: true, Content: fmt.Sprintf("参数 context 只能是 fresh 或 fork（收到的是 %q）：fresh 表示子 agent 只看 prompt，fork 表示附带父级对话上下文。", a.Context)}, nil
	}

	parent := strings.TrimSpace(t.ParentSessionID)
	if parent == "" && ex != nil {
		parent = strings.TrimSpace(ex.SessionID)
	}
	root := strings.TrimSpace(t.Root)
	roots := append([]string(nil), t.Roots...)
	if root == "" && ex != nil {
		root = strings.TrimSpace(ex.Root)
		if len(roots) == 0 {
			roots = append(roots, ex.Roots...)
		}
	}
	rootSession := strings.TrimSpace(t.RootSessionID)
	if rootSession == "" {
		rootSession = parent
	}
	history := t.History
	if ctxMode == SubagentContextFresh {
		// 契约：fresh 不带父级历史。显式清空而不是靠调用方自觉，
		// 否则一次误传就会把整段无关对话灌进子任务。
		history = nil
	}

	handle, err := t.Delegator.Delegate(ctx, SubagentRequest{
		ParentSessionID: parent,
		RootSessionID:   rootSession,
		Depth:           t.Depth,
		Label:           clipLabel(label),
		Prompt:          prompt,
		Context:         ctxMode,
		History:         history,
		Model:           t.Model,
		Kind:            t.Kind,
		Root:            root,
		Roots:           roots,
		Mode:            t.Mode,
	})
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("派子任务失败：%s。你可以稍后重试，或自己直接做这件事。", err.Error())}, nil
	}

	outLabel := strings.TrimSpace(handle.Label)
	if outLabel == "" {
		outLabel = clipLabel(label)
	}
	return Result{
		Content: fmt.Sprintf("已派出子任务 %s（id=%s），它跑完会自动通知你，先继续别的事。", outLabel, handle.ChildID),
		Display: map[string]any{"subagent": map[string]any{
			"child_id": handle.ChildID,
			"label":    outLabel,
			"status":   SubagentStatusRunning,
		}},
	}, nil
}

// ---------------------------------------------------------------------------
// list_subagents 工具
// ---------------------------------------------------------------------------

// ListSubagentsTool 让父级模型回看自己派出去的子任务。
//
// 它是只读工具（登记在 safety.go 的 readOnlyTools 里）：重复调用没有任何后果，
// 失败也允许重试一次——这正是父级轮询子任务状态需要的语义。
type ListSubagentsTool struct {
	Delegator Delegator
}

func (ListSubagentsTool) Name() string { return "list_subagents" }

func (ListSubagentsTool) Description() string {
	return "列出你派出去的所有子任务：状态（运行中/完成/失败/被中止）、耗时和结论摘要。" +
		"想知道某个子任务跑到哪了、或收到完成通知后想回看细节时用它。"
}

func (ListSubagentsTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t ListSubagentsTool) Execute(ctx context.Context, raw json.RawMessage, ex *Exec) (Result, error) {
	if t.Delegator == nil {
		return Result{IsError: true, Content: "这个环境没接子 agent 能力，查不了子任务。"}, nil
	}
	parent := ""
	if ex != nil {
		parent = strings.TrimSpace(ex.SessionID)
	}
	infos, err := t.Delegator.ListSubagents(ctx, parent)
	if err != nil {
		return Result{IsError: true, Content: fmt.Sprintf("查子任务失败：%s", err.Error())}, nil
	}
	if len(infos) == 0 {
		return Result{
			Content: "当前没有子任务。",
			Display: map[string]any{"subagents": []map[string]any{}},
		}, nil
	}

	var b strings.Builder
	display := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		b.WriteString(subagentRowText(info))
		b.WriteString("\n")
		display = append(display, subagentInfoDisplay(info))
	}
	return Result{
		Content: strings.TrimRight(b.String(), "\n"),
		Display: map[string]any{"subagents": display},
	}, nil
}

// subagentRowText 把一条子任务渲染成一行（一行一条，模型扫起来最省 token）。
func subagentRowText(info SubagentInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- #%s [%s] %s", info.ChildID, subagentStatusLabel(info.Status, info.StopReason), subagentLabelOr(info.Label))
	if info.FinishedAt != nil {
		fmt.Fprintf(&b, "（%.1f 秒）", info.FinishedAt.Sub(info.StartedAt).Seconds())
	}
	switch {
	case strings.TrimSpace(info.Reply) != "":
		fmt.Fprintf(&b, "：%s", clipRunes(firstLine(info.Reply), 120))
	case strings.TrimSpace(info.Error) != "":
		fmt.Fprintf(&b, "：%s", clipRunes(firstLine(info.Error), 120))
	}
	return b.String()
}

// subagentInfoDisplay 是 list_subagents 的 Display 行，形状与 3.6 的 HTTP 接口一致，
// 前端拿同一套字段渲染卡片，不用为工具结果再写一套解析。
func subagentInfoDisplay(info SubagentInfo) map[string]any {
	m := map[string]any{
		"child_id":    info.ChildID,
		"label":       info.Label,
		"status":      info.Status,
		"stop_reason": info.StopReason,
		"reply":       info.Reply,
		"error":       info.Error,
		"depth":       info.Depth,
		"started_at":  info.StartedAt,
		"finished_at": nil,
	}
	if info.FinishedAt != nil {
		m["finished_at"] = *info.FinishedAt
	}
	return m
}
