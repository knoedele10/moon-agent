package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// update_plan 工具：把长任务的计划外化成一份可更新的状态。
//
// 为什么放在 web 层：agent 包只提供存储与渲染（internal/agent/plan.go），
// 不认识具体工具；工具注册、Kind 过滤与权限元数据都在这一层统一处理。
// 工具本身不动文件、不出网，所以任何模式下都不需要用户确认。
type planTool struct{}

func (planTool) Name() string { return "update_plan" }

func (planTool) Description() string {
	return "更新本会话的任务计划（整份替换，不是增量修改）。" +
		"任务有 3 步以上、或用户明确要计划时，先用它列出步骤；每完成一步就再调用一次更新状态。" +
		"状态只支持 pending / in_progress / completed / blocked，一次最多一个 in_progress。" +
		"计划会每轮自动展示给你；未验证的步骤不要标 completed。"
}

func (planTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"explanation": map[string]any{"type": "string", "description": "一句话说明这次更新了什么（可选）"},
			"plan": map[string]any{
				"type":        "array",
				"minItems":    1,
				"maxItems":    agent.PlanMaxSteps,
				"description": "完整的步骤清单（每次都要给全，不是只给变化的部分）",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"step":   map[string]any{"type": "string", "description": "要做什么，一句话，可验证"},
						"status": map[string]any{"type": "string", "enum": []string{agent.PlanPending, agent.PlanInProgress, agent.PlanCompleted, agent.PlanBlocked}},
					},
					"required": []string{"step", "status"},
				},
			},
		},
		"required": []string{"plan"},
	}
}

func (planTool) Execute(_ context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var body struct {
		Explanation string `json:"explanation"`
		Plan        []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return agent.Result{IsError: true, Content: "参数不是合法 JSON：" + err.Error()}, nil
	}
	steps := make([]agent.PlanStep, 0, len(body.Plan))
	for _, s := range body.Plan {
		steps = append(steps, agent.PlanStep{Step: s.Step, Status: s.Status})
	}
	session := ""
	if ex != nil {
		session = ex.SessionID
	}
	plan, err := agent.UpdatePlan(session, agent.Plan{Explanation: body.Explanation, Steps: steps})
	if err != nil {
		return agent.Result{
			IsError: true,
			Content: "计划未更新：" + err.Error() + "。请修正后重试；计划必须是一份完整的步骤清单。",
			Display: map[string]any{"code": "PLAN_INVALID"},
		}, nil
	}
	return agent.Result{
		Content: "计划已更新。\n" + agent.RenderPlan(plan) + "\n继续执行当前步骤；完成后记得再次调用 update_plan。",
		Display: map[string]any{
			"summary": fmt.Sprintf("计划 %d 步", len(plan.Steps)),
			"plan":    plan.Steps,
			"next":    nextPlanStepText(plan),
		},
	}, nil
}

// nextPlanStepText 给前端一个简短提示；没有下一步时返回空串。
func nextPlanStepText(p agent.Plan) string {
	for _, s := range p.Steps {
		if s.Status == agent.PlanInProgress {
			return s.Step
		}
	}
	for _, s := range p.Steps {
		if s.Status == agent.PlanPending {
			return s.Step
		}
	}
	return ""
}

// planToolRegistered 供测试断言「工具只在应该出现的地方出现」。
func planToolRegistered(tools []agent.Tool) bool {
	for _, t := range tools {
		if strings.TrimSpace(t.Name()) == "update_plan" {
			return true
		}
	}
	return false
}
