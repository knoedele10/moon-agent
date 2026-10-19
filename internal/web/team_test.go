package web

// 团队服务的单测（契约 docs/agent/07-team.md §1.2 / §二）。
//
// 全部离线：不连 MySQL，只测纯函数——名字校验、任务板状态机、CAS、依赖校验、
// 文案与状态映射。落库与续跑路径由端到端验证覆盖（web 层这里没有可注入的假 DB）。

import (
	"strings"
	"testing"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// 编译期钉住：web 层的 teamService 必须满足 agent 包冻结的接口。
var _ agent.TeamService = (*teamService)(nil)

// ---------------------------------------------------------------------------
// 名字校验
// ---------------------------------------------------------------------------

func TestValidateTeamName(t *testing.T) {
	for _, ok := range []string{"alice", "a1", "code-reviewer", "writer-2", "x9"} {
		if err := validateTeamName(ok); err != nil {
			t.Errorf("validateTeamName(%q) 报错，应该是合法名字：%v", ok, err)
		}
	}
	cases := map[string]string{
		"":                      "空",
		"a":                     "只有一个字符",
		"Alice":                 "大写",
		"1alice":                "数字开头",
		"-alice":                "连字符开头",
		"al ice":                "空格",
		"al_ice":                "下划线",
		"alice!":                "特殊字符",
		strings.Repeat("a", 33): "超长（32 是上限）",
		"lead":                  "保留名",
	}
	for raw, why := range cases {
		if err := validateTeamName(raw); err == nil {
			t.Errorf("validateTeamName(%q) 应该报错（%s）", raw, why)
		}
	}
}

// ---------------------------------------------------------------------------
// 文案与状态映射
// ---------------------------------------------------------------------------

func TestTeamMessageContentCarriesSender(t *testing.T) {
	got := teamMessageContent("alice", " 顺便看下登录页 ")
	for _, want := range []string{"【来自 alice】", "顺便看下登录页"} {
		if !strings.Contains(got, want) {
			t.Errorf("消息正文缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, " 顺便") {
		t.Errorf("消息正文没有去掉首尾空白：%q", got)
	}
}

func TestTeamMemberStatusMapping(t *testing.T) {
	cases := map[string]string{
		model.SubagentRunning: "running",
		model.SubagentDone:    "inactive",
		model.SubagentFailed:  "failed",
		model.SubagentAborted: "failed",
		"":                    "inactive",
	}
	for raw, want := range cases {
		if got := teamMemberStatus(raw); got != want {
			t.Errorf("teamMemberStatus(%q) = %q，想要 %q", raw, got, want)
		}
	}
}

func TestTeamTreeMembersShowsNestedSubagentsAndLatestRun(t *testing.T) {
	runs := []model.AgentSubagentRun{
		{Id: 1, ChildConversationID: 11, ParentConversationID: 1, Name: "architect", Label: "架构", Depth: 1, Status: model.SubagentRunning},
		{Id: 2, ChildConversationID: 12, ParentConversationID: 11, Label: "整理目录", Depth: 2, Status: model.SubagentRunning, Activity: "正在读文件"},
		{Id: 3, ChildConversationID: 11, ParentConversationID: 1, Name: "architect", Label: "架构", Depth: 1, Status: model.SubagentDone},
	}
	members := teamTreeMembers(model.Conversation{Id: 1, Model: "mock"}, runs, false)
	if len(members) != 3 {
		t.Fatalf("want lead + child + grandchild, got %#v", members)
	}
	if members[1]["name"] != "architect" || members[1]["status"] != "inactive" || members[1]["parent_id"] != uint(1) {
		t.Fatalf("latest direct member missing: %#v", members[1])
	}
	if members[2]["role"] != "subagent" || members[2]["depth"] != 2 || members[2]["parent_id"] != uint(11) || members[2]["activity"] != "正在读文件" {
		t.Fatalf("nested subagent missing: %#v", members[2])
	}
}

func TestParseTeamTaskID(t *testing.T) {
	for raw, want := range map[string]int{"task-3": 3, "3": 3, " task-12 ": 12} {
		got, err := parseTeamTaskID(raw)
		if err != nil || got != want {
			t.Errorf("parseTeamTaskID(%q) = (%d, %v)，想要 (%d, nil)", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "task-0", "task-", "abc", "-2"} {
		if _, err := parseTeamTaskID(raw); err == nil {
			t.Errorf("parseTeamTaskID(%q) 应该报错", raw)
		}
	}
}

func TestTeamTimeoutDuration(t *testing.T) {
	cases := map[int]time.Duration{
		0:        30 * time.Second,
		-5:       30 * time.Second,
		10000:    10 * time.Second,
		99999999: time.Hour, // 上限 1 小时
	}
	for raw, want := range cases {
		if got := teamTimeoutDuration(raw); got != want {
			t.Errorf("teamTimeoutDuration(%d) = %v，想要 %v", raw, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 任务板：依赖与 write_scopes
// ---------------------------------------------------------------------------

func TestValidateTeamBlockedBy(t *testing.T) {
	lookup := func(no int) (string, bool) {
		switch no {
		case 1:
			return model.TeamTaskPending, true
		case 2:
			return model.TeamTaskCompleted, true
		case 3:
			return model.TeamTaskDeleted, true
		}
		return "", false
	}
	if err := validateTeamBlockedBy([]string{"task-1", "task-2"}, lookup); err != nil {
		t.Fatalf("存在的依赖不该报错：%v", err)
	}
	if err := validateTeamBlockedBy([]string{"task-9"}, lookup); err == nil {
		t.Error("依赖指向不存在的任务必须报错")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("报错文案要能教会模型怎么改：%v", err)
	}
	if err := validateTeamBlockedBy([]string{"task-3"}, lookup); err == nil {
		t.Error("依赖指向已删除的任务必须报错（被删的任务永远不算完成）")
	}
	if err := validateTeamBlockedBy([]string{"abc"}, lookup); err == nil {
		t.Error("依赖 id 格式非法必须报错")
	}
}

func TestTeamTaskDepsReady(t *testing.T) {
	lookup := func(no int) (string, bool) {
		switch no {
		case 1:
			return model.TeamTaskCompleted, true
		case 2:
			return model.TeamTaskInProgress, true
		case 3:
			return model.TeamTaskDeleted, true
		}
		return "", false
	}
	if !teamTaskDepsReady("[]", lookup) {
		t.Error("没有依赖 = 就绪")
	}
	if !teamTaskDepsReady(`["task-1"]`, lookup) {
		t.Error("依赖全部完成 = 就绪")
	}
	if teamTaskDepsReady(`["task-1","task-2"]`, lookup) {
		t.Error("有依赖没完成 = 未就绪")
	}
	if teamTaskDepsReady(`["task-3"]`, lookup) {
		t.Error("依赖是 tombstone = 未就绪（不能偷偷放行）")
	}
	if teamTaskDepsReady(`["task-9"]`, lookup) {
		t.Error("依赖不存在 = 未就绪")
	}
	if teamTaskDepsReady(`not-json`, lookup) {
		t.Error("依赖字段坏了 = 未就绪（宁可卡住也不误判完成）")
	}
}

// write_scopes 只存不拦：任何路径前缀都原样存下来，不做校验、不加锁。
func TestNormalizeTeamWriteScopesStoresAnything(t *testing.T) {
	in := []string{"../escape", "/etc/passwd", `C:\Windows`, "a/b", "a/b", "", "  保持原样  "}
	got := normalizeTeamWriteScopes(in)
	want := []string{"../escape", "/etc/passwd", `C:\Windows`, "a/b", "  保持原样  "}
	if len(got) != len(want) {
		t.Fatalf("normalizeTeamWriteScopes 结果 %#v，想要 %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 scope = %q，想要 %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 任务板：状态机
// ---------------------------------------------------------------------------

func teamTaskFixture() model.AgentTeamTask {
	return model.AgentTeamTask{
		Id:                 7,
		UserID:             "u1",
		TeamConversationID: 3,
		TaskNo:             2,
		Subject:            "写团队单测",
		Description:        "把状态机钉住",
		Status:             model.TeamTaskPending,
		Owner:              "",
		BlockedBy:          "[]",
		WriteScopes:        "[]",
		Revision:           4,
	}
}

func planOK(t *testing.T, task model.AgentTeamTask, actor teamActor, revision int, action string, patch agent.TeamTaskInput, facts teamBoardFacts) map[string]any {
	t.Helper()
	updates, err := planTeamTaskUpdate(task, actor, revision, action, patch, facts)
	if err != nil {
		t.Fatalf("action=%s 应该成功，却报错：%v", action, err)
	}
	return updates
}

func planErr(t *testing.T, task model.AgentTeamTask, actor teamActor, revision int, action string, patch agent.TeamTaskInput, facts teamBoardFacts) error {
	t.Helper()
	if _, err := planTeamTaskUpdate(task, actor, revision, action, patch, facts); err == nil {
		t.Fatalf("action=%s 应该被拒，却成功了", action)
	} else {
		return err
	}
	return nil
}

func TestPlanClaimRequiresPendingAndReadyDeps(t *testing.T) {
	task := teamTaskFixture()
	actor := teamActor{Name: "alice"}

	updates := planOK(t, task, actor, task.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if updates["status"] != model.TeamTaskInProgress || updates["owner"] != "alice" {
		t.Fatalf("claim 应该把任务置为 in_progress 并认领给 alice：%#v", updates)
	}

	err := planErr(t, task, actor, task.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: false})
	if !strings.Contains(err.Error(), "依赖") {
		t.Errorf("依赖未完成时 claim 的报错要提到依赖：%v", err)
	}

	owned := task
	owned.Owner = "bob"
	err = planErr(t, owned, actor, task.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "已经被") {
		t.Errorf("别人认领的任务不能再 claim：%v", err)
	}

	running := task
	running.Status = model.TeamTaskInProgress
	err = planErr(t, running, actor, task.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("非 pending 任务不能 claim：%v", err)
	}
}

func TestPlanCompleteRequiresOwner(t *testing.T) {
	task := teamTaskFixture()
	task.Status = model.TeamTaskInProgress
	task.Owner = "alice"

	updates := planOK(t, task, teamActor{Name: "alice"}, task.Revision, "complete", agent.TeamTaskInput{}, teamBoardFacts{})
	if updates["status"] != model.TeamTaskCompleted {
		t.Fatalf("owner 完成应该落 completed：%#v", updates)
	}

	err := planErr(t, task, teamActor{Name: "bob"}, task.Revision, "complete", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("非 owner 完成要报「只有 owner」：%v", err)
	}

	// lead 也不能替别人完成：完成代表成果验收，必须由 owner 自己交。
	err = planErr(t, task, teamActor{Name: "lead", IsLead: true}, task.Revision, "complete", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("lead 替别人完成也要被拒：%v", err)
	}

	pending := teamTaskFixture()
	pending.Owner = "alice"
	err = planErr(t, pending, teamActor{Name: "alice"}, pending.Revision, "complete", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "in_progress") {
		t.Errorf("没 claim 就 complete 要报先 claim：%v", err)
	}
}

func TestPlanReleaseAndReopen(t *testing.T) {
	task := teamTaskFixture()
	task.Status = model.TeamTaskInProgress
	task.Owner = "alice"

	updates := planOK(t, task, teamActor{Name: "alice"}, task.Revision, "release", agent.TeamTaskInput{}, teamBoardFacts{})
	if updates["status"] != model.TeamTaskPending || updates["owner"] != "" {
		t.Fatalf("release 应该回到 pending 且清空 owner：%#v", updates)
	}
	planErr(t, task, teamActor{Name: "bob"}, task.Revision, "release", agent.TeamTaskInput{}, teamBoardFacts{})

	done := task
	done.Status = model.TeamTaskCompleted
	updates = planOK(t, done, teamActor{Name: "alice"}, done.Revision, "reopen", agent.TeamTaskInput{}, teamBoardFacts{})
	if updates["status"] != model.TeamTaskPending {
		t.Fatalf("reopen 应该回到 pending：%#v", updates)
	}
	planErr(t, task, teamActor{Name: "alice"}, task.Revision, "reopen", agent.TeamTaskInput{}, teamBoardFacts{})
}

func TestPlanReassignPermissions(t *testing.T) {
	task := teamTaskFixture()
	task.Status = model.TeamTaskInProgress
	task.Owner = "alice"
	memberExists := func(name string) bool { return name == "bob" || name == "alice" }

	updates := planOK(t, task, teamActor{Name: "lead", IsLead: true}, task.Revision, "reassign",
		agent.TeamTaskInput{Owner: "bob"}, teamBoardFacts{MemberExists: memberExists})
	if updates["owner"] != "bob" {
		t.Fatalf("lead 应该能把任务换给 bob：%#v", updates)
	}
	err := planErr(t, task, teamActor{Name: "lead", IsLead: true}, task.Revision, "reassign",
		agent.TeamTaskInput{Owner: "carol"}, teamBoardFacts{MemberExists: memberExists})
	if !strings.Contains(err.Error(), "不是本团队成员") {
		t.Errorf("owner 必须是团队成员：%v", err)
	}
	// 成员可以把自己手上的任务交出去，但不能抢别人的。
	planOK(t, task, teamActor{Name: "alice"}, task.Revision, "reassign",
		agent.TeamTaskInput{Owner: "bob"}, teamBoardFacts{MemberExists: memberExists})
	err = planErr(t, task, teamActor{Name: "bob"}, task.Revision, "reassign",
		agent.TeamTaskInput{Owner: "bob"}, teamBoardFacts{MemberExists: memberExists})
	if !strings.Contains(err.Error(), "lead") {
		t.Errorf("成员抢别人的任务要报「只有 lead 能改归属」：%v", err)
	}
}

func TestPlanDeleteLeavesTombstoneAndFreezesTask(t *testing.T) {
	task := teamTaskFixture()
	task.Status = model.TeamTaskInProgress
	task.Owner = "alice"

	updates := planOK(t, task, teamActor{Name: "alice"}, task.Revision, "delete", agent.TeamTaskInput{}, teamBoardFacts{})
	if updates["status"] != model.TeamTaskDeleted {
		t.Fatalf("delete 必须是 tombstone（status=deleted，行不删）：%#v", updates)
	}

	deleted := task
	deleted.Status = model.TeamTaskDeleted
	err := planErr(t, deleted, teamActor{Name: "lead", IsLead: true}, deleted.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "删除") {
		t.Errorf("tombstone 上不能再做任何变更：%v", err)
	}
}

func TestPlanEditAndSetDependencies(t *testing.T) {
	task := teamTaskFixture()
	task.Owner = "alice"

	updates := planOK(t, task, teamActor{Name: "alice"}, task.Revision, "edit",
		agent.TeamTaskInput{Subject: "新标题", WriteScopes: []string{"src/team"}}, teamBoardFacts{})
	if updates["subject"] != "新标题" {
		t.Fatalf("edit 应该更新标题：%#v", updates)
	}
	if updates["write_scopes"] != `["src/team"]` {
		t.Fatalf("edit 应该更新 write_scopes（只存不拦）：%#v", updates)
	}
	err := planErr(t, task, teamActor{Name: "bob"}, task.Revision, "edit",
		agent.TeamTaskInput{Subject: "抢改"}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "owner") {
		t.Errorf("非 owner 改别人认领的任务要被拒：%v", err)
	}
	err = planErr(t, task, teamActor{Name: "alice"}, task.Revision, "edit", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "没带") {
		t.Errorf("空 edit 要明确报没给字段：%v", err)
	}

	// set_dependencies 必须显式带 blocked_by（哪怕空数组），避免误清空。
	err = planErr(t, task, teamActor{Name: "alice"}, task.Revision, "set_dependencies", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "blocked_by") {
		t.Errorf("缺 blocked_by 要明确报参数名：%v", err)
	}
	if _, err := planTeamTaskUpdate(task, teamActor{Name: "alice"}, task.Revision, "set_dependencies",
		agent.TeamTaskInput{BlockedBy: []string{}}, teamBoardFacts{}); err != nil {
		t.Errorf("显式传空数组应该允许清空依赖：%v", err)
	}
}

func TestPlanRejectsStaleRevision(t *testing.T) {
	task := teamTaskFixture() // Revision = 4
	err := planErr(t, task, teamActor{Name: "alice"}, 3, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "TEAM_TASK_STALE_REVISION") {
		t.Fatalf("CAS 失败必须带冻结错误码 TEAM_TASK_STALE_REVISION：%v", err)
	}
	if !strings.Contains(err.Error(), "team_task_get") {
		t.Errorf("CAS 报错要告诉模型下一步怎么做：%v", err)
	}
	err = planErr(t, task, teamActor{Name: "alice"}, 0, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "expected_revision") {
		t.Errorf("缺 revision 要明确报参数名：%v", err)
	}
}

func TestPlanRejectsUnknownActionAndAnonymousActor(t *testing.T) {
	task := teamTaskFixture()
	err := planErr(t, task, teamActor{Name: "alice"}, task.Revision, "explode", agent.TeamTaskInput{}, teamBoardFacts{})
	if !strings.Contains(err.Error(), "未知的 action") {
		t.Errorf("未知 action 要列出可用动作：%v", err)
	}
	err = planErr(t, task, teamActor{Name: ""}, task.Revision, "claim", agent.TeamTaskInput{}, teamBoardFacts{DepsReady: true})
	if !strings.Contains(err.Error(), "团队成员") {
		t.Errorf("匿名会话不能动任务板：%v", err)
	}
}
