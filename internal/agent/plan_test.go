package agent

import (
	"strings"
	"testing"

	"blog-my/internal/llm"
)

func TestUpdatePlanNormalizesAndRenders(t *testing.T) {
	ResetPlans()
	defer ResetPlans()

	p, err := UpdatePlan("s1", Plan{
		Explanation: "先定位再改",
		Steps: []PlanStep{
			{Step: "读现有实现", Status: "done"},
			{Step: "实现检索工具", Status: "doing"},
			{Step: "补测试", Status: "待办"},
			{Step: "跑验证", Status: "blocked"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Status != PlanCompleted || p.Steps[1].Status != PlanInProgress ||
		p.Steps[2].Status != PlanPending || p.Steps[3].Status != PlanBlocked {
		t.Fatalf("状态归一化不对：%+v", p.Steps)
	}
	block := PlanBlock("s1")
	for _, want := range []string{"进度 1/4", "[x] 读现有实现", "[>] 实现检索工具", "[ ] 补测试", "[!] 跑验证", "下一步：实现检索工具"} {
		if !strings.Contains(block, want) {
			t.Errorf("计划块缺少 %q：\n%s", want, block)
		}
	}
}

func TestUpdatePlanValidation(t *testing.T) {
	ResetPlans()
	defer ResetPlans()

	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{"空计划", Plan{}, "不能为空"},
		{"空步骤", Plan{Steps: []PlanStep{{Step: "  ", Status: "pending"}}}, "内容为空"},
		{"非法状态", Plan{Steps: []PlanStep{{Step: "a", Status: "whatever"}}}, "不合法"},
		{"两个进行中", Plan{Steps: []PlanStep{{Step: "a", Status: "in_progress"}, {Step: "b", Status: "in_progress"}}}, "一次只保留一个"},
		{"重复步骤", Plan{Steps: []PlanStep{{Step: "a", Status: "pending"}, {Step: "a", Status: "completed"}}}, "重复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdatePlan("s1", tc.plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误包含 %q，实际 %v", tc.want, err)
			}
		})
	}
	if _, ok := CurrentPlan("s1"); ok {
		t.Fatal("校验失败的计划不得写入")
	}
}

func TestUpdatePlanReplacesWholePlan(t *testing.T) {
	ResetPlans()
	defer ResetPlans()

	if _, err := UpdatePlan("s1", Plan{Steps: []PlanStep{{Step: "a", Status: "pending"}, {Step: "b", Status: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan("s1", Plan{Steps: []PlanStep{{Step: "a", Status: "completed"}}}); err != nil {
		t.Fatal(err)
	}
	p, ok := CurrentPlan("s1")
	if !ok || len(p.Steps) != 1 || p.Steps[0].Status != PlanCompleted {
		t.Fatalf("应当是整份替换，实际 %+v", p)
	}
}

func TestPlanIsolatedPerSession(t *testing.T) {
	ResetPlans()
	defer ResetPlans()
	if _, err := UpdatePlan("a", Plan{Steps: []PlanStep{{Step: "a", Status: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := CurrentPlan("b"); ok {
		t.Fatal("计划必须按会话隔离")
	}
	if PlanBlock("b") != "" {
		t.Fatal("没有计划的会话不应注入空块")
	}
}

// withPlanContext 每轮都重新渲染，且永远只有一份计划块。
func TestWithPlanContextReplacesTransientBlock(t *testing.T) {
	ResetPlans()
	defer ResetPlans()
	if _, err := UpdatePlan("s1", Plan{Steps: []PlanStep{{Step: "改代码", Status: "in_progress"}}}); err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleSystem, Content: planBlockPrefix + "\n旧计划"},
		{Role: llm.RoleUser, Content: "继续"},
	}
	got := withPlanContext(msgs, "s1")
	if len(got) != 3 {
		t.Fatalf("应剔除旧计划块并追加新块，实际 %d 条：%+v", len(got), got)
	}
	count := 0
	for _, m := range got {
		if planInjected(m.Content) {
			count++
			if !strings.Contains(m.Content, "改代码") {
				t.Fatalf("注入的应是新计划：%q", m.Content)
			}
		}
	}
	if count != 1 {
		t.Fatalf("计划块应恰好一份，实际 %d", count)
	}
	// 没有计划时：只剔除旧块，不追加。
	if got := withPlanContext(msgs, "nobody"); len(got) != 2 {
		t.Fatalf("没有计划时应只剔除旧块，实际 %d 条", len(got))
	}
}
