package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

func TestPlanToolUpdatesSessionPlan(t *testing.T) {
	agent.ResetPlans()
	defer agent.ResetPlans()
	ex := &agent.Exec{SessionID: "conv-test"}

	raw, _ := json.Marshal(map[string]any{
		"explanation": "拆成三步",
		"plan": []map[string]any{
			{"step": "定位问题", "status": "completed"},
			{"step": "写修复", "status": "in_progress"},
			{"step": "跑测试", "status": "pending"},
		},
	})
	res, err := planTool{}.Execute(context.Background(), raw, ex)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("合法计划不应报错：%s", res.Content)
	}
	for _, want := range []string{"计划已更新", "[x] 定位问题", "[>] 写修复", "继续执行"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("回执缺少 %q：\n%s", want, res.Content)
		}
	}
	p, ok := agent.CurrentPlan("conv-test")
	if !ok || len(p.Steps) != 3 {
		t.Fatalf("计划未写入：%+v ok=%v", p, ok)
	}
	if next := res.Display["next"]; next != "写修复" {
		t.Errorf("next 应为进行中的步骤，实际 %v", next)
	}
	// 计划块每轮注入时必须能读到同一份状态。
	if block := agent.PlanBlock("conv-test"); !strings.Contains(block, "写修复") {
		t.Errorf("PlanBlock 未反映最新计划：%s", block)
	}
}

func TestPlanToolRejectsInvalidPlanWithRepairHint(t *testing.T) {
	agent.ResetPlans()
	defer agent.ResetPlans()
	ex := &agent.Exec{SessionID: "conv-bad"}
	raw, _ := json.Marshal(map[string]any{
		"plan": []map[string]any{{"step": "a", "status": "not-a-status"}},
	})
	res, err := planTool{}.Execute(context.Background(), raw, ex)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "修正后重试") {
		t.Fatalf("非法计划应返回可修复错误：%+v", res)
	}
	if _, ok := agent.CurrentPlan("conv-bad"); ok {
		t.Fatal("非法计划不得写入")
	}
}

func TestUpdatePlanToolRegisteredOnlyWhereUseful(t *testing.T) {
	work := buildAgentTools(agentToolPolicy{Mode: ModeAuto, Kind: KindWork})
	if !planToolRegistered(work) {
		t.Fatal("工作模式应当有 update_plan")
	}
	code := buildAgentTools(agentToolPolicy{Mode: ModeAuto, Kind: KindCode})
	if !planToolRegistered(code) {
		t.Fatal("代码模式应当有 update_plan")
	}
	chat := filterToolsByKind(work, KindChat)
	if planToolRegistered(chat) {
		t.Fatal("日常聊天不应出现任务计划工具")
	}
	planMode := buildAgentTools(agentToolPolicy{Mode: ModePlan, Kind: KindWork})
	if !planToolRegistered(planMode) {
		t.Fatal("计划模式必须能更新计划")
	}
}
