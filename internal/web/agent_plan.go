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

// planWriteTool 是计划模式专用的 write：**只允许写 .md 文件**，用来把计划存档。
//
// 为什么这样设计：计划模式的本意是“不动手”，但用户需要把计划落成文件留档。
// 把权限收到“只能写 Markdown”这一个口子，既能存档，又不会让它借机改代码或删文件。
type planWriteTool struct {
	inner agent.Tool
	roots []string
}

func (p planWriteTool) Name() string               { return p.inner.Name() }
func (p planWriteTool) Parameters() map[string]any { return p.inner.Parameters() }

func (p planWriteTool) Description() string {
	return "把一个 Markdown 文件写入工作区，用来把计划存档（仅计划模式可用，且只允许 .md 后缀）。"
}

func (p planWriteTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if !strings.EqualFold(filepath.Ext(strings.TrimSpace(a.FilePath)), ".md") {
		return agent.Result{}, fmt.Errorf("计划模式只能写 .md 文件（收到 %q）；要改其他文件请切到“风险操作先问”或“自主执行”档", a.FilePath)
	}
	// 计划模式的写权限比其它档更窄：只允许工作区内的 .md。
	// 这里用 Resolve（工作区沙箱）判断，而不是看 Exec.AllowOutside——
	// 后者在 HTTP 链路上恒为 true，直接放行会让 plan 档也能写外部文件。
	if ex == nil || strings.TrimSpace(ex.Root) == "" {
		return agent.Result{}, fmt.Errorf("计划模式需要配置工作区才能把计划存档")
	}
	roots := append(append([]string(nil), p.roots...), ex.Roots...)
	if _, _, err := agentfs.ResolveWorkspaces(ex.Root, roots, a.FilePath); err != nil {
		return agent.Result{}, fmt.Errorf("计划模式只能把计划写在工作区内；要操作工作区外的文件，请先切换到“风险操作先问”或“自主执行”档（%s）", agentfs.OutsideNeedsAbsPath)
	}
	return p.inner.Execute(ctx, raw, ex)
}

// planPrompt 是计划模式的提示词。
//
// 「默认把计划写成 markdown 文件」是用户明确要求的默认行为：
// 有计划存档，后续切到执行档时才有东西可依。没有工作区时退化为只在对话里给计划。
func planPrompt(root string) string {
	if strings.TrimSpace(root) == "" {
		return "\n【当前模式：只列计划】你只能输出计划，不要调用任何工具，也不要说自己已经执行了什么。" +
			"计划要具体到每一步做什么、预期结果是什么。当前没有配置工作区，所以直接在对话里给出计划。\n"
	}
	return "\n【当前模式：只列计划】你只能输出计划，不要执行任何文件改动（改文件、改名、删除都不行）。" +
		"**默认操作**：把计划写成 Markdown 文件存到工作区（例如 plan-<主题或日期>.md），然后用 write 工具保存；" +
		"这是本模式唯一允许的写操作，且只允许 .md 后缀，并且只能写在工作区内；工作区外的路径一律拒绝。" +
		"保存后在回复里给出文件路径和计划要点。如果用户明确说不用存文件，就只在对话里给计划。\n"
}
