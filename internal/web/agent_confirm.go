package web

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"blog-my/internal/agent"
	agentfs "blog-my/internal/agent/fs"
)

// agent 的三档自主模式。默认 confirm：风险操作先问用户，而不是默认放开。
const (
	ModePlan    = "plan"    // 只列计划，一个工具都不执行
	ModeConfirm = "confirm" // 风险操作先问用户，确认后才执行
	ModeAuto    = "auto"    // 自主决定，不再询问
)

// confirmRequestPrefix 是「需要用户确认」文案的固定开头。
// lastAssistantWasConfirmRequest 靠它判断上一轮是不是在等用户确认——
// 风险确认与外部路径确认必须共用同一个开头，否则用户回「确认」时不会被当成确认。
const confirmRequestPrefix = "我要执行一个会改动文件的操作"

func normalizeMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case ModePlan:
		return ModePlan
	case ModeAuto:
		return ModeAuto
	case ModeConfirm:
		return ModeConfirm
	default:
		return ModeConfirm
	}
}

// riskyTool 判断一个工具是否属于“会改动东西、且难以撤销”的一类。
// 只读工具（read/list/calculator/web_search/...）不算风险，否则每个问题都要问一次，模式二会变得没法用。
func riskyTool(name string) bool {
	switch name {
	// 注意：memory_write 不在名单里——写记忆库不会动用户的文件，也不可逆风险极低，
	// 属于“所有档位都可以用”的操作（用户 2026-10-12 明确要求）。
	case "write", "edit", "move", "delete", "office_edit", "skill_run", "run_command":
		return true
	// skill_write 写的是技能库而不是用户文件，但“助手以后会怎么做”被改了，
	// 影响面不小，按风险工具处理：confirm 档先问、plan 档不给。
	case "skill_write":
		return true
	}
	// 以后新增的 shell / python / 外部服务连接器都按风险工具处理，
	// 避免“新加了一个危险工具但忘了加进名单”。
	return false
}

// outsideWriteTool 判断一个工具是不是「写类」——参数里带路径、会改磁盘。
//
// 与 riskyTool 刻意分开：skill_run / skill_write 也是风险工具，但它们动的是技能库，
// 走另一套策略，不在这里做外部路径确认。
func outsideWriteTool(name string) bool {
	switch name {
	case "write", "edit", "move", "delete", "office_edit":
		return true
	}
	return false
}

// wrapOutsideWorkspace 给写类工具套上「外部路径确认」壳（工作区是首选目录）。
//
// 只包写类；auto 档壳会直接放行（用户要求：其它目录在 auto 档直接做），
// confirm 档每次碰到工作区外都重新问——确认结果不落库、不记忆。
// plan 档不包这层：计划模式只允许写工作区内的 .md，工作区外直接拒绝（见 planWriteTool）。
func wrapOutsideWorkspace(tools []agent.Tool, root, mode string, roots ...string) []agent.Tool {
	if strings.TrimSpace(root) == "" {
		return tools
	}
	for i, t := range tools {
		if outsideWriteTool(t.Name()) {
			tools[i] = outsideWorkspaceTool{inner: t, root: root, roots: roots, mode: mode}
		}
	}
	return tools
}

// confirmTool 是风险工具的“确认包装”：在 confirm 模式下不直接执行，
// 而是复用 ask_user 的停轮机制把动作抛给用户确认。
//
// 关键点：**只有用户本人回复确认后才放行**。判断依据是
// 「上一条 assistant 消息是确认请求」且「用户这条消息是肯定答复」——
// 模型自己重试同一个调用不会被当成确认（它无法伪造用户消息）。
type confirmTool struct{ inner agent.Tool }

func (c confirmTool) Name() string               { return c.inner.Name() }
func (c confirmTool) Description() string        { return c.inner.Description() }
func (c confirmTool) Parameters() map[string]any { return c.inner.Parameters() }

func (c confirmTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	// 外部路径确认优先：内层是外部确认壳、且这次调用真的碰到工作区外时，
	// 交给它去问（它的问题里带具体路径），不再叠一层风险确认——一次调用只弹一次窗。
	if w, ok := c.inner.(interface {
		externalPaths(json.RawMessage) []string
	}); ok && len(w.externalPaths(raw)) > 0 {
		return c.inner.Execute(ctx, raw, ex)
	}
	return agent.Result{
		Content:   "已暂停，等待用户确认。",
		AwaitUser: confirmQuestion(c.Name(), raw),
	}, nil
}

// confirmQuestion 把动作讲清楚：模型要对用户说人话，而不是抛一段 JSON。
func confirmQuestion(name string, raw json.RawMessage) string {
	args := strings.TrimSpace(string(raw))
	if len(args) > 400 {
		args = args[:400] + "…"
	}
	return fmt.Sprintf("%s，需要你确认：\n\n工具：%s\n参数：%s\n\n确认执行吗？回复“确认”我就继续；回复别的我就停下。", confirmRequestPrefix, name, args)
}

// outsideWorkspaceTool 是「工作区是首选目录」的确认壳：包住写类工具。
//
// 只有当参数里的路径**落在工作区之外**、且当前档位不是 auto 时才拦下来问用户；
// auto 档直接放行。外部路径一律以绝对路径出现在确认文案与 Display 里，
// 用户点确认前能看清要动哪个文件；用户确认后由 web/agent_pending.go 重放这一次调用。
type outsideWorkspaceTool struct {
	inner agent.Tool
	root  string
	roots []string
	mode  string
}

func (o outsideWorkspaceTool) Name() string               { return o.inner.Name() }
func (o outsideWorkspaceTool) Description() string        { return o.inner.Description() }
func (o outsideWorkspaceTool) Parameters() map[string]any { return o.inner.Parameters() }

// outsidePathKeys 是写类工具参数里可能出现路径的字段名。
var outsidePathKeys = []string{"file_path", "from_path", "to_path"}

// externalPaths 返回这次调用里落在工作区之外的绝对路径（Clean 后，/ 分隔）。
//
// 只认绝对路径：工作区外的相对路径（../ 穿越、根内链接逃逸）在 fs 层一律拒绝，
// 不在这里问用户——问了也会执行失败。系统目录同理，交给 fs 层报错。
func (o outsideWorkspaceTool) externalPaths(raw json.RawMessage) []string {
	root := strings.TrimSpace(o.root)
	if root == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil
	}
	var out []string
	for _, key := range outsidePathKeys {
		p, _ := args[key].(string)
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		abs := filepath.Clean(p)
		if _, _, err := agentfs.ResolveWorkspaces(root, o.roots, abs); err == nil {
			continue // 工作区内：走原来的流程
		}
		if agentfs.IsSystemPath(abs) {
			continue // 永远拒绝的位置，交给内层工具报错
		}
		out = append(out, filepath.ToSlash(abs))
	}
	return out
}

func (o outsideWorkspaceTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	paths := o.externalPaths(raw)
	if len(paths) == 0 || normalizeMode(o.mode) == ModeAuto {
		return o.inner.Execute(ctx, raw, ex)
	}
	return agent.Result{
		Content:   "已暂停，等待用户确认工作区外的路径。",
		AwaitUser: outsideQuestion(o.Name(), paths),
		Display:   map[string]any{"confirm": true, "paths": paths},
	}, nil
}

// outsideQuestion 把「要碰工作区外的文件」讲清楚：列出确切路径，并说明只放行这一次。
// 开头必须与 confirmQuestion 共用 confirmRequestPrefix，确认流程才认得出这是确认请求。
func outsideQuestion(name string, paths []string) string {
	return fmt.Sprintf("%s，需要你确认：这次操作会碰到工作区之外的路径。\n\n工具：%s\n工作区外路径：\n- %s\n\n确认执行吗？回复“确认”我就继续；回复别的我就停下。",
		confirmRequestPrefix, name, strings.Join(paths, "\n- "))
}

// isAffirmative 判断用户这条消息是不是肯定答复。
func isAffirmative(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	// 只认短消息：长句子里出现“好”不算确认（用户可能只是在讨论别的）。
	if len([]rune(m)) > 12 {
		return false
	}
	for _, w := range []string{"确认", "确定", "同意", "可以", "继续", "好的", "好", "是", "执行", "yes", "y", "ok", "confirm", "go"} {
		if m == w || strings.HasPrefix(m, w) {
			return true
		}
	}
	return false
}

// lastAssistantWasConfirmRequest 判断历史里最后一条 assistant 消息是不是确认请求。
func lastAssistantWasConfirmRequest(history []llmMessageLike) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		return strings.HasPrefix(strings.TrimSpace(history[i].Content), confirmRequestPrefix)
	}
	return false
}

// llmMessageLike 让上面的判断函数不依赖具体消息类型（便于单测）。
type llmMessageLike struct {
	Role    string
	Content string
}

// modePrompt 把当前档位写进系统提示词，让模型知道自己的权限边界。
func modePrompt(mode, root string) string {
	switch normalizeMode(mode) {
	case ModePlan:
		return planPrompt(root)
	case ModeAuto:
		return "\n【当前模式：自主执行】用户已经授权你完成当前任务。先执行检查，再自行选择方案并调用工具，持续推进到完成和核对结果；" +
			"普通实现细节、文件命名、输出格式和先后顺序依据用户要求、画像、项目惯例自行决定，必要时简短说明你的假设。" +
			"不要把计划或‘我会开始’当作交付，不要每一步询问是否开始、继续或调用工具。" +
			"工具失败时先读错误、修正参数或换一种方法，避免重复同一个失败调用。阶段次数用完会自动续行并提醒重新规划。" +
			"只有缺少无法从上下文或工具获取的必要信息、必须由用户完成登录/授权等动作、" +
			"或目标冲突/操作明显超出本次授权范围时才用 ask_user 暂停，并提供具体阻碍和已经尝试的方法。" +
			"涉及删除、改名、覆盖时先确认目标和现有内容，再操作并核对；结果未知的操作先查状态，避免重复产生副作用。\n"
	default:
		return "\n【当前模式：风险操作先确认】只读工具可以直接用；write/edit/move/delete 这类会改动文件的操作" +
			"会先弹给用户确认，收到确认后再继续，不要绕过。要碰工作区外的路径时先说明原因，" +
			"写类操作会带上具体路径再弹一次确认，每次都重新问。\n"
	}
}
