package web

import (
	"context"
	"encoding/json"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolkit"
)

// Auto mode still has a user-input channel, but routine decisions must not
// terminate the run. A blocker needs an explicit reason and attempted remedy.
type autonomousAskTool struct{ toolkit.AskUserTool }

func (autonomousAskTool) Description() string {
	return "自主执行模式：仅在真正阻塞任务、必须由用户提供信息或操作时提问。先检查上下文、画像和工具结果，" +
		"自行决定普通方案、文件名、格式和实现细节；不要询问是否开始、是否继续或是否调用工具。" +
		"必须给出 blocker_type、blocking_reason 和 attempted_steps，解释为什么无法自行解决。" +
		"提问附带 2-4 个具体选项。调用通过后会暂停任务等待用户。"
}

func (a autonomousAskTool) Parameters() map[string]any {
	p := a.AskUserTool.Parameters()
	props := p["properties"].(map[string]any)
	props["blocker_type"] = map[string]any{"type": "string", "enum": []string{"required_information", "access_required", "scope_change"},
		"description": "仅限缺少无法推断的必要信息、需要用户登录/授权等操作、或目标冲突/必须扩大授权范围。"}
	props["blocking_reason"] = map[string]any{"type": "string", "description": "缺少什么，为什么不能从上下文或工具结果得到，为什么不回答就无法完成。"}
	props["attempted_steps"] = map[string]any{"type": "string", "description": "已经尝试的查询、检查或替代方案及其结果；确实没有可用工具时说明原因。"}
	p["required"] = []string{"question", "options", "blocker_type", "blocking_reason", "attempted_steps"}
	return p
}

func (a autonomousAskTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var args struct {
		Type     string `json:"blocker_type"`
		Reason   string `json:"blocking_reason"`
		Attempts string `json:"attempted_steps"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.Result{IsError: true, Content: "提问参数无效，请自行检查并继续任务。"}, nil
	}
	validType := args.Type == "required_information" || args.Type == "access_required" || args.Type == "scope_change"
	if !validType || strings.TrimSpace(args.Reason) == "" || strings.TrimSpace(args.Attempts) == "" {
		return agent.Result{IsError: true, Content: "当前为自主执行模式，此次提问没有提供真实阻碍，任务不会暂停。" +
			"请先检查上下文和工具结果，自行决定常规细节并继续。只有不可推断的必要信息、用户必须完成的操作或授权范围冲突才可提问；" +
			"届时提供 blocker_type、blocking_reason 和 attempted_steps，不要虚构理由。"}, nil
	}
	return a.AskUserTool.Execute(ctx, raw, ex)
}
