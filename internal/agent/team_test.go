package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"blog-my/internal/llm"
)

// 编译期断言：8 个工具必须满足 Tool 接口（签名/方法集写错时在这里就炸）。
var (
	_ Tool = SpawnTeammateTool{}
	_ Tool = SendMessageTool{}
	_ Tool = ListAgentsTool{}
	_ Tool = WaitAgentTool{}
	_ Tool = TeamTaskCreateTool{}
	_ Tool = TeamTaskListTool{}
	_ Tool = TeamTaskGetTool{}
	_ Tool = TeamTaskUpdateTool{}
)

// fakeTeamService 是 TeamService 的测试替身：记录收到的调用，返回脚本化的结果。
// 不连 DB、不起 goroutine——工具层只关心「参数怎么翻译成服务调用」和「结果怎么渲染」。
type fakeTeamService struct {
	spawnReq *TeammateRequest
	spawnRet TeamMember
	spawnErr error

	rosterParent string
	rosterRet    []TeamMember
	rosterErr    error

	sendParent, sendFrom, sendTarget, sendMessage string
	sendErr                                       error

	waitParent  string
	waitTimeout int
	waitChanged bool
	waitErr     error

	createParent string
	createInput  TeamTaskInput
	createRet    TeamTask
	createErr    error

	listParent string
	listFilter TaskFilter
	listRet    []TeamTask
	listErr    error

	getParent string
	getID     string
	getRet    TeamTask
	getErr    error

	updateParent, updateID, updateActor string
	updateRev                           int
	updateAction                        string
	updatePatch                         TeamTaskInput
	updateRet                           TeamTask
	updateErr                           error
}

func (f *fakeTeamService) SpawnTeammate(_ context.Context, req TeammateRequest) (TeamMember, error) {
	f.spawnReq = &req
	return f.spawnRet, f.spawnErr
}

func (f *fakeTeamService) Roster(_ context.Context, parentSessionID string) ([]TeamMember, error) {
	f.rosterParent = parentSessionID
	return f.rosterRet, f.rosterErr
}

func (f *fakeTeamService) SendMessage(_ context.Context, parentSessionID, from, target, message string) error {
	f.sendParent, f.sendFrom, f.sendTarget, f.sendMessage = parentSessionID, from, target, message
	return f.sendErr
}

func (f *fakeTeamService) WaitForChange(_ context.Context, parentSessionID string, timeoutMS int) (bool, error) {
	f.waitParent, f.waitTimeout = parentSessionID, timeoutMS
	return f.waitChanged, f.waitErr
}

func (f *fakeTeamService) CreateTask(_ context.Context, parentSessionID string, in TeamTaskInput) (TeamTask, error) {
	f.createParent, f.createInput = parentSessionID, in
	return f.createRet, f.createErr
}

func (f *fakeTeamService) ListTasks(_ context.Context, parentSessionID string, filter TaskFilter) ([]TeamTask, error) {
	f.listParent, f.listFilter = parentSessionID, filter
	return f.listRet, f.listErr
}

func (f *fakeTeamService) GetTask(_ context.Context, parentSessionID, id string) (TeamTask, error) {
	f.getParent, f.getID = parentSessionID, id
	return f.getRet, f.getErr
}

func (f *fakeTeamService) UpdateTask(_ context.Context, parentSessionID, id, actor string, expectedRevision int, action string, patch TeamTaskInput) (TeamTask, error) {
	f.updateParent, f.updateID, f.updateActor = parentSessionID, id, actor
	f.updateRev, f.updateAction, f.updatePatch = expectedRevision, action, patch
	return f.updateRet, f.updateErr
}

// teamTools 是全部 8 个工具的清单（schema / nil Service / 会话接线共用）。
func teamTools(svc TeamService) []Tool {
	return []Tool{
		SpawnTeammateTool{Service: svc},
		SendMessageTool{Service: svc},
		ListAgentsTool{Service: svc},
		WaitAgentTool{Service: svc},
		TeamTaskCreateTool{Service: svc},
		TeamTaskListTool{Service: svc},
		TeamTaskGetTool{Service: svc},
		TeamTaskUpdateTool{Service: svc},
	}
}

func toolProp(t *testing.T, tool Tool, key string) map[string]any {
	t.Helper()
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s 的 properties 不是对象：%#v", tool.Name(), params["properties"])
	}
	prop, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("%s 的 schema 缺少参数 %q", tool.Name(), key)
	}
	return prop
}

func toolRequired(t *testing.T, tool Tool) map[string]bool {
	t.Helper()
	params := tool.Parameters()
	raw, ok := params["required"]
	if !ok {
		return map[string]bool{}
	}
	list, ok := raw.([]string)
	if !ok {
		t.Fatalf("%s 的 required 不是字符串数组：%#v", tool.Name(), raw)
	}
	out := map[string]bool{}
	for _, r := range list {
		out[r] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

func TestTeamToolSchemas(t *testing.T) {
	cases := []struct {
		name     string
		tool     Tool
		props    []string
		required []string
	}{
		{"spawn_teammate", SpawnTeammateTool{}, []string{"name", "description", "prompt", "context"}, []string{"name", "description", "prompt"}},
		{"send_message", SendMessageTool{}, []string{"target", "message"}, []string{"target", "message"}},
		{"list_agents", ListAgentsTool{}, nil, nil},
		{"wait_agent", WaitAgentTool{}, []string{"timeout_ms"}, nil},
		{"team_task_create", TeamTaskCreateTool{}, []string{"subject", "description", "blocked_by", "write_scopes"}, []string{"subject", "description"}},
		{"team_task_list", TeamTaskListTool{}, []string{"status", "owner", "ready"}, nil},
		{"team_task_get", TeamTaskGetTool{}, []string{"task_id"}, []string{"task_id"}},
		{"team_task_update", TeamTaskUpdateTool{},
			[]string{"task_id", "expected_revision", "action", "subject", "description", "blocked_by", "write_scopes", "owner"},
			[]string{"task_id", "expected_revision", "action"}},
	}
	if len(cases) != 8 {
		t.Fatalf("契约要求恰好 8 个团队工具，实际 %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tool.Name(); got != c.name {
				t.Fatalf("Name() = %q，契约要求 %q", got, c.name)
			}
			params := c.tool.Parameters()
			if params["type"] != "object" {
				t.Fatalf("Parameters().type = %v，要求 object", params["type"])
			}
			props, ok := params["properties"].(map[string]any)
			if !ok {
				t.Fatalf("properties 不是对象：%#v", params["properties"])
			}
			if len(c.props) == 0 {
				if len(props) != 0 {
					t.Fatalf("%s 应当无参数，实际 %#v", c.name, props)
				}
			}
			for _, key := range c.props {
				prop := toolProp(t, c.tool, key)
				// 描述是模型唯一的决策依据：每个参数都必须写清语义，不能空着。
				if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
					t.Errorf("参数 %q 缺少 description——模型只能靠它决策", key)
				}
			}
			req := toolRequired(t, c.tool)
			if len(req) != len(c.required) {
				t.Fatalf("required = %v，契约要求 %v", req, c.required)
			}
			for _, r := range c.required {
				if !req[r] {
					t.Errorf("required 缺少 %q（实际 %v）", r, req)
				}
			}
		})
	}
}

func TestTeamToolSchemaEnums(t *testing.T) {
	// spawn_teammate 的 context：必须显式给出 fresh/fork，模型才不会自己发明第三档。
	ctxEnum, ok := toolProp(t, SpawnTeammateTool{}, "context")["enum"].([]string)
	if !ok || len(ctxEnum) != 2 || ctxEnum[0] != SubagentContextFresh || ctxEnum[1] != SubagentContextFork {
		t.Fatalf("spawn_teammate.context.enum = %#v，要求 [fresh fork]", toolProp(t, SpawnTeammateTool{}, "context")["enum"])
	}

	// team_task_list 的 status：空=全部，所以 enum 里不该出现空串。
	statusEnum, ok := toolProp(t, TeamTaskListTool{}, "status")["enum"].([]string)
	if !ok || len(statusEnum) != 3 {
		t.Fatalf("team_task_list.status.enum = %#v，要求 3 项", toolProp(t, TeamTaskListTool{}, "status")["enum"])
	}
	seen := map[string]bool{}
	for _, s := range statusEnum {
		seen[s] = true
	}
	for _, want := range []string{TeamTaskPending, TeamTaskInProgress, TeamTaskCompleted} {
		if !seen[want] {
			t.Errorf("status.enum 缺少 %q（实际 %v）", want, statusEnum)
		}
	}

	// team_task_update 的 action：8 个动作一个都不能少。
	actionEnum, ok := toolProp(t, TeamTaskUpdateTool{}, "action")["enum"].([]string)
	if !ok || len(actionEnum) != len(teamTaskActions) {
		t.Fatalf("team_task_update.action.enum = %#v，要求 %v", toolProp(t, TeamTaskUpdateTool{}, "action")["enum"], teamTaskActions)
	}
	actionSeen := map[string]bool{}
	for _, a := range actionEnum {
		actionSeen[a] = true
	}
	for _, want := range teamTaskActions {
		if !actionSeen[want] {
			t.Errorf("action.enum 缺少 %q", want)
		}
	}
}

func TestTeamToolNilService(t *testing.T) {
	// 没接团队能力的环境（比如单测里裸构造的工具）不能 panic，要给模型一句能懂的话。
	for _, tool := range teamTools(nil) {
		t.Run(tool.Name(), func(t *testing.T) {
			res, err := tool.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "7"})
			if err != nil {
				t.Fatalf("Service 为 nil 不能抛 error（会打断整轮循环），实际 %v", err)
			}
			if !res.IsError {
				t.Fatalf("Service 为 nil 必须是 IsError 的 Result，实际 %#v", res)
			}
			if !strings.Contains(res.Content, "没接团队能力") {
				t.Fatalf("报错文案要说清环境限制，实际 %q", res.Content)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// spawn_teammate
// ---------------------------------------------------------------------------

func TestSpawnTeammateToolValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"缺 name", `{"description":"负责前端","prompt":"改首页"}`, "name"},
		{"name 只有空白", `{"name":"  ","description":"负责前端","prompt":"改首页"}`, "name"},
		{"name 大写", `{"name":"Alice","description":"负责前端","prompt":"改首页"}`, "name"},
		{"name 太短", `{"name":"a","description":"负责前端","prompt":"改首页"}`, "name"},
		{"name 以数字开头", `{"name":"1abc","description":"负责前端","prompt":"改首页"}`, "name"},
		{"name 带下划线", `{"name":"front_end","description":"负责前端","prompt":"改首页"}`, "name"},
		{"缺 description", `{"name":"alice","prompt":"改首页"}`, "description"},
		{"缺 prompt", `{"name":"alice","description":"负责前端"}`, "prompt"},
		{"prompt 只有空白", `{"name":"alice","description":"负责前端","prompt":"   "}`, "prompt"},
		{"context 非法", `{"name":"alice","description":"负责前端","prompt":"改首页","context":"resume"}`, "context"},
		{"参数不是合法 JSON", `{"name":`, "JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeTeamService{}
			tool := SpawnTeammateTool{Service: fs, Depth: 0, MaxDepth: 1, Mode: "auto"}
			res, err := tool.Execute(context.Background(), json.RawMessage(c.args), &Exec{SessionID: "9", Root: "E:/ws"})
			if err != nil {
				t.Fatalf("参数校验失败不能抛 error，实际 %v", err)
			}
			if !res.IsError {
				t.Fatalf("非法参数必须返回 IsError 的 Result，实际 %#v", res)
			}
			if !strings.Contains(res.Content, c.wantSub) {
				t.Fatalf("报错文案 %q 没有点出问题参数 %q", res.Content, c.wantSub)
			}
			if fs.spawnReq != nil {
				t.Fatalf("参数不合法时不该调用 Service，实际调了：%#v", fs.spawnReq)
			}
		})
	}
}

func TestSpawnTeammateToolSuccess(t *testing.T) {
	fs := &fakeTeamService{spawnRet: TeamMember{
		Name: "alice", Description: "负责前端", Role: TeamRoleTeammate,
		Status: TeamStatusRunning, Depth: 1, ChildID: "12", Model: "qwen",
	}}
	tool := SpawnTeammateTool{
		Service: fs, Depth: 0, MaxDepth: 1, Mode: "confirm",
		RootSessionID: "3", History: []llm.Message{{Role: llm.RoleUser, Content: "不该被 fresh 带过去"}},
		Model: "qwen", Kind: "work", Root: "E:/ws",
	}
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"name":"alice","description":"负责前端","prompt":"把首页改完"}`),
		&Exec{SessionID: "9", Root: "E:/ws"})
	if err != nil {
		t.Fatalf("成功路径不该返回 error，实际 %v", err)
	}
	if res.IsError {
		t.Fatalf("成功路径不该是 IsError，实际 %#v", res)
	}
	req := fs.spawnReq
	if req == nil {
		t.Fatal("Service 没收到建成员请求")
	}
	if req.ParentSessionID != "9" || req.RootSessionID != "3" || req.Depth != 0 {
		t.Errorf("会话/深度字段不对：parent=%q root=%q depth=%d", req.ParentSessionID, req.RootSessionID, req.Depth)
	}
	if req.Name != "alice" || req.Description != "负责前端" || req.Prompt != "把首页改完" {
		t.Errorf("成员字段不对：%#v", req)
	}
	if req.Context != SubagentContextFresh {
		t.Errorf("省略 context 应默认 fresh，实际 %q", req.Context)
	}
	if req.History != nil {
		t.Errorf("fresh 不该带父级历史，实际 %#v", req.History)
	}
	if req.Model != "qwen" || req.Kind != "work" || req.Root != "E:/ws" || req.Mode != "confirm" {
		t.Errorf("继承字段不对：%#v", req)
	}
	if !strings.Contains(res.Content, "已创建成员 alice") || !strings.Contains(res.Content, "运行中") {
		t.Errorf("回执文案不对：%q", res.Content)
	}
	member, ok := res.Display["team_member"].(map[string]any)
	if !ok {
		t.Fatalf("Display 缺少 team_member 结构：%#v", res.Display)
	}
	if member["name"] != "alice" || member["status"] != TeamStatusRunning || member["role"] != TeamRoleTeammate ||
		member["child_id"] != "12" || member["depth"] != 1 {
		t.Fatalf("Display.team_member = %#v", member)
	}
}

func TestSpawnTeammateToolForkCarriesHistory(t *testing.T) {
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "把首页拆给两个人做"},
		{Role: llm.RoleAssistant, Content: "好的"},
	}
	fs := &fakeTeamService{spawnRet: TeamMember{Name: "alice", Role: TeamRoleTeammate, Status: TeamStatusRunning}}
	tool := SpawnTeammateTool{Service: fs, Depth: 0, MaxDepth: 1, History: hist}
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"name":"alice","description":"负责前端","prompt":"接着刚才的方案改","context":" FORK "}`),
		&Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("fork 路径应当成功：err=%v res=%#v", err, res)
	}
	req := fs.spawnReq
	if req.Context != SubagentContextFork {
		t.Fatalf("context 应容忍大小写与空白并归一成 fork，实际 %q", req.Context)
	}
	if len(req.History) != 2 || req.History[0].Content != "把首页拆给两个人做" {
		t.Fatalf("fork 必须带上父级历史，实际 %#v", req.History)
	}
	// 没显式给 RootSessionID 时回落成父会话 id（容量按它统计）。
	if req.RootSessionID != "9" {
		t.Fatalf("RootSessionID 应回落成父会话 id，实际 %q", req.RootSessionID)
	}
}

func TestSpawnTeammateToolDepthLimit(t *testing.T) {
	fs := &fakeTeamService{}
	tool := SpawnTeammateTool{Service: fs, Depth: 1, MaxDepth: 1}
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"name":"alice","description":"负责前端","prompt":"改首页"}`),
		&Exec{SessionID: "9"})
	if err != nil {
		t.Fatalf("深度超限不能抛 error，实际 %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "不能再建成员") {
		t.Fatalf("深度超限必须给一句明确的拒绝，实际 %#v", res)
	}
	if fs.spawnReq != nil {
		t.Fatalf("深度超限时不该调用 Service")
	}
}

func TestSpawnTeammateToolServiceError(t *testing.T) {
	fs := &fakeTeamService{spawnErr: errors.New("名字 alice 已被占用（包括已结束的成员）")}
	tool := SpawnTeammateTool{Service: fs, Depth: 0, MaxDepth: 1}
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"name":"alice","description":"负责前端","prompt":"改首页"}`),
		&Exec{SessionID: "9"})
	if err != nil {
		t.Fatalf("Service 报错必须折成 Result，不能抛 error，实际 %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "已被占用") {
		t.Fatalf("报错文案要带上 Service 的原因，实际 %#v", res)
	}
}

// ---------------------------------------------------------------------------
// send_message
// ---------------------------------------------------------------------------

func TestSendMessageToolValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"缺 target", `{"message":"改一下首页"}`, "target"},
		{"target 只有空白", `{"target":"  ","message":"改一下首页"}`, "target"},
		{"缺 message", `{"target":"alice"}`, "message"},
		{"message 只有空白", `{"target":"alice","message":"   "}`, "message"},
		{"参数不是合法 JSON", `{"target":`, "JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeTeamService{}
			res, err := SendMessageTool{Service: fs}.Execute(context.Background(), json.RawMessage(c.args), &Exec{SessionID: "9"})
			if err != nil {
				t.Fatalf("参数校验失败不能抛 error，实际 %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, c.wantSub) {
				t.Fatalf("非法参数应点出 %q，实际 %#v", c.wantSub, res)
			}
			if fs.sendTarget != "" {
				t.Fatalf("参数不合法时不该调用 Service")
			}
		})
	}
}

func TestSendMessageToolSuccessAndSemantics(t *testing.T) {
	fs := &fakeTeamService{}
	tool := SendMessageTool{Service: fs, ParentSessionID: "3"}
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"target":" alice ","message":" 把首页改完 "}`),
		&Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("成功路径应当成功：err=%v res=%#v", err, res)
	}
	// parent 用 web 层显式填的团队会话；from 永远是调用方自己的会话。
	if fs.sendParent != "3" || fs.sendFrom != "9" || fs.sendTarget != "alice" || fs.sendMessage != "把首页改完" {
		t.Fatalf("服务调用参数不对：parent=%q from=%q target=%q message=%q",
			fs.sendParent, fs.sendFrom, fs.sendTarget, fs.sendMessage)
	}
	// 回执必须把「已收下 ≠ 已处理」写出来，否则模型会把发送成功当成对方已完成。
	if !strings.Contains(res.Content, "已收下") || !strings.Contains(res.Content, "不代表") {
		t.Fatalf("回执必须说明已收下不代表已处理，实际 %q", res.Content)
	}
	if !strings.Contains(res.Content, "wait_agent") || !strings.Contains(res.Content, "list_agents") {
		t.Fatalf("回执要告诉模型怎么确认结果，实际 %q", res.Content)
	}
	msg, ok := res.Display["team_message"].(map[string]any)
	if !ok || msg["target"] != "alice" || msg["from"] != "9" || msg["delivered"] != true {
		t.Fatalf("Display.team_message 形状不对：%#v", res.Display)
	}
}

func TestSendMessageToolServiceError(t *testing.T) {
	fs := &fakeTeamService{sendErr: errors.New("目标成员 bob 不存在")}
	res, err := SendMessageTool{Service: fs}.Execute(context.Background(),
		json.RawMessage(`{"target":"bob","message":"在吗"}`), &Exec{SessionID: "9"})
	if err != nil {
		t.Fatalf("Service 报错必须折成 Result，不能抛 error，实际 %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "目标成员 bob 不存在") {
		t.Fatalf("报错文案要带上 Service 的原因，实际 %#v", res)
	}
}

// ---------------------------------------------------------------------------
// list_agents
// ---------------------------------------------------------------------------

func TestListAgentsToolRows(t *testing.T) {
	fs := &fakeTeamService{rosterRet: []TeamMember{
		{Name: "lead", Role: TeamRoleLead, Status: TeamStatusRunning, Description: "团队的根会话"},
		{Name: "alice", Role: TeamRoleTeammate, Status: TeamStatusRunning, Description: "负责前端", Depth: 1, ChildID: "12"},
		{Name: "bob", Role: TeamRoleTeammate, Status: TeamStatusInactive, Description: "跑完了", Depth: 1, ChildID: "13"},
		{Name: "carl", Role: TeamRoleTeammate, Status: TeamStatusFailed, Description: "挂了", Depth: 1, ChildID: "14"},
	}}
	res, err := ListAgentsTool{Service: fs}.Execute(context.Background(), nil, &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("列表路径应当成功：err=%v res=%#v", err, res)
	}
	if fs.rosterParent != "9" {
		t.Fatalf("Roster 应查询 Exec.SessionID=9，实际 %q", fs.rosterParent)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 4 {
		t.Fatalf("应当一行一条共 4 行，实际 %d 行：%q", len(lines), res.Content)
	}
	if !strings.Contains(lines[0], "lead") || !strings.Contains(lines[0], "运行中") || !strings.Contains(lines[0], "lead") {
		t.Errorf("lead 行内容不对：%q", lines[0])
	}
	if !strings.Contains(lines[1], "alice") || !strings.Contains(lines[1], "运行中") || !strings.Contains(lines[1], "teammate") {
		t.Errorf("成员行内容不对：%q", lines[1])
	}
	if !strings.Contains(lines[2], "已结束") {
		t.Errorf("inactive 应显示「已结束」：%q", lines[2])
	}
	if !strings.Contains(lines[3], "失败") {
		t.Errorf("failed 应显示「失败」：%q", lines[3])
	}

	rows, ok := res.Display["team"].([]map[string]any)
	if !ok || len(rows) != 4 {
		t.Fatalf("Display.team 形状不对：%#v", res.Display)
	}
	if rows[0]["role"] != TeamRoleLead || rows[0]["child_id"] != "" {
		t.Errorf("lead 行 Display 不对：%#v", rows[0])
	}
	if rows[1]["child_id"] != "12" || rows[1]["depth"] != 1 || rows[1]["description"] != "负责前端" {
		t.Errorf("成员行 Display 不对：%#v", rows[1])
	}
}

func TestListAgentsToolEmptyAndErrors(t *testing.T) {
	// 空名单：Display 必须是空数组而不是 null（前端直接遍历）。
	res, err := ListAgentsTool{Service: &fakeTeamService{}}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("空名单不是错误：err=%v res=%#v", err, res)
	}
	if res.Content != "当前没有团队成员。" {
		t.Fatalf("空名单文案 = %q", res.Content)
	}
	rows, ok := res.Display["team"].([]map[string]any)
	if !ok || len(rows) != 0 {
		t.Fatalf("空名单 Display 必须是空数组：%#v", res.Display)
	}

	// Service 报错折成 Result。
	fs := &fakeTeamService{rosterErr: errors.New("数据库暂时不可用")}
	res, err = ListAgentsTool{Service: fs}.Execute(context.Background(), nil, &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "数据库暂时不可用") {
		t.Fatalf("Service 报错应折成 IsError：err=%v res=%#v", err, res)
	}
}

func TestTeamToolsMissingSession(t *testing.T) {
	// 没有会话 id 就没法定位团队：要给一句能懂的报错，而不是把空串塞给服务。
	// 每个工具都给一份合法参数：这样才能走到「缺会话」这一步（参数校验在会话检查之前）。
	argsByTool := map[string]string{
		"spawn_teammate":   `{"name":"alice","description":"负责前端","prompt":"改首页"}`,
		"send_message":     `{"target":"alice","message":"在吗"}`,
		"list_agents":      `{}`,
		"wait_agent":       `{}`,
		"team_task_create": `{"subject":"补接口","description":"补完"}`,
		"team_task_list":   `{}`,
		"team_task_get":    `{"task_id":"task-1"}`,
		"team_task_update": `{"task_id":"task-1","expected_revision":1,"action":"claim"}`,
	}
	for _, tool := range teamTools(&fakeTeamService{}) {
		t.Run(tool.Name(), func(t *testing.T) {
			args, ok := argsByTool[tool.Name()]
			if !ok {
				t.Fatalf("测试缺少 %s 的合法参数", tool.Name())
			}
			res, err := tool.Execute(context.Background(), json.RawMessage(args), nil)
			if err != nil || !res.IsError || !strings.Contains(res.Content, "会话") {
				t.Fatalf("缺会话时应返回 IsError：err=%v res=%#v", err, res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// wait_agent
// ---------------------------------------------------------------------------

func TestWaitAgentToolTimeoutSemantics(t *testing.T) {
	// 省略 timeout_ms → 默认 30 秒；changed=false 不是错误，文案要明确说「没有变化」。
	fs := &fakeTeamService{waitChanged: false}
	res, err := WaitAgentTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil {
		t.Fatalf("超时不是 error，实际 %v", err)
	}
	if res.IsError {
		t.Fatalf("没有变化不是失败，不该是 IsError：%#v", res)
	}
	if fs.waitTimeout != teamWaitDefaultMS {
		t.Fatalf("默认 timeout 应为 %d，实际 %d", teamWaitDefaultMS, fs.waitTimeout)
	}
	if !strings.Contains(res.Content, "没有检测到任何变化") {
		t.Fatalf("超时文案要明确说没有变化，实际 %q", res.Content)
	}
	if !strings.Contains(res.Content, "不是错误") {
		t.Fatalf("超时文案要告诉模型这不是错误，实际 %q", res.Content)
	}
	wait, ok := res.Display["team_wait"].(map[string]any)
	if !ok || wait["changed"] != false || wait["timeout_ms"] != teamWaitDefaultMS {
		t.Fatalf("Display.team_wait 形状不对：%#v", res.Display)
	}
}

func TestWaitAgentToolChanged(t *testing.T) {
	fs := &fakeTeamService{waitChanged: true}
	res, err := WaitAgentTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{"timeout_ms":60000}`), &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("有变化应当成功：err=%v res=%#v", err, res)
	}
	if fs.waitTimeout != 60000 {
		t.Fatalf("显式 timeout 应透传，实际 %d", fs.waitTimeout)
	}
	if !strings.Contains(res.Content, "有变化") || !strings.Contains(res.Content, "重新读") {
		t.Fatalf("有变化时要提醒重新读状态，实际 %q", res.Content)
	}
	wait, _ := res.Display["team_wait"].(map[string]any)
	if wait["changed"] != true {
		t.Fatalf("Display.team_wait.changed 应为 true：%#v", res.Display)
	}
}

func TestWaitAgentToolValidationAndErrors(t *testing.T) {
	for _, args := range []string{`{"timeout_ms":5000}`, `{"timeout_ms":4000000}`, `{"timeout_ms":-1}`} {
		fs := &fakeTeamService{}
		res, err := WaitAgentTool{Service: fs}.Execute(context.Background(), json.RawMessage(args), &Exec{SessionID: "9"})
		if err != nil || !res.IsError || !strings.Contains(res.Content, "timeout_ms") {
			t.Fatalf("越界 timeout 应报错（%s）：err=%v res=%#v", args, err, res)
		}
		if fs.waitTimeout != 0 {
			t.Fatalf("越界时不该调用 Service（%s）", args)
		}
	}
	fs := &fakeTeamService{waitErr: errors.New("数据库暂时不可用")}
	res, err := WaitAgentTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "数据库暂时不可用") {
		t.Fatalf("Service 报错应折成 IsError：err=%v res=%#v", err, res)
	}
}

// ---------------------------------------------------------------------------
// team_task_create / list / get
// ---------------------------------------------------------------------------

func TestTeamTaskCreateToolValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"缺 subject", `{"description":"补完接口"}`, "subject"},
		{"subject 只有空白", `{"subject":"  ","description":"补完接口"}`, "subject"},
		{"缺 description", `{"subject":"补接口"}`, "description"},
		{"blocked_by 含空项", `{"subject":"补接口","description":"补完","blocked_by":["task-1"," "]}`, "blocked_by"},
		{"参数不是合法 JSON", `{"subject":`, "JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeTeamService{}
			res, err := TeamTaskCreateTool{Service: fs}.Execute(context.Background(), json.RawMessage(c.args), &Exec{SessionID: "9"})
			if err != nil {
				t.Fatalf("参数校验失败不能抛 error，实际 %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, c.wantSub) {
				t.Fatalf("非法参数应点出 %q，实际 %#v", c.wantSub, res)
			}
			if fs.createParent != "" {
				t.Fatalf("参数不合法时不该调用 Service")
			}
		})
	}
}

func TestTeamTaskCreateToolSuccess(t *testing.T) {
	fs := &fakeTeamService{createRet: TeamTask{
		ID: "task-3", Revision: 1, Subject: "补接口", Description: "补完接口",
		Status: TeamTaskPending, Ready: true,
	}}
	res, err := TeamTaskCreateTool{Service: fs}.Execute(context.Background(), json.RawMessage(
		`{"subject":" 补接口 ","description":" 补完接口 ","blocked_by":["task-1"," task-2 "],"write_scopes":["web/"," ","templates/"]}`),
		&Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("成功路径应当成功：err=%v res=%#v", err, res)
	}
	if fs.createParent != "9" {
		t.Fatalf("CreateTask 应作用于 Exec.SessionID，实际 %q", fs.createParent)
	}
	in := fs.createInput
	if in.Subject != "补接口" || in.Description != "补完接口" {
		t.Errorf("标题/描述应去掉首尾空白：%#v", in)
	}
	if len(in.BlockedBy) != 2 || in.BlockedBy[0] != "task-1" || in.BlockedBy[1] != "task-2" {
		t.Errorf("blocked_by 应逐项去空白：%#v", in.BlockedBy)
	}
	if len(in.WriteScopes) != 2 || in.WriteScopes[0] != "web/" || in.WriteScopes[1] != "templates/" {
		t.Errorf("write_scopes 应丢掉空项：%#v", in.WriteScopes)
	}
	if !strings.Contains(res.Content, "task-3") || !strings.Contains(res.Content, "revision=1") {
		t.Errorf("回执要带上 id 和 revision（下一步 CAS 要用），实际 %q", res.Content)
	}
	task, ok := res.Display["task"].(map[string]any)
	if !ok || task["id"] != "task-3" || task["status"] != TeamTaskPending || task["ready"] != true {
		t.Fatalf("Display.task 形状不对：%#v", res.Display)
	}
}

func TestTeamTaskCreateToolServiceError(t *testing.T) {
	fs := &fakeTeamService{createErr: errors.New("blocked_by 指向不存在的任务 task-9")}
	res, err := TeamTaskCreateTool{Service: fs}.Execute(context.Background(),
		json.RawMessage(`{"subject":"补接口","description":"补完","blocked_by":["task-9"]}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "task-9") {
		t.Fatalf("Service 报错应折成 IsError 并带原因：err=%v res=%#v", err, res)
	}
}

func TestTeamTaskListToolRowsAndFilter(t *testing.T) {
	fs := &fakeTeamService{listRet: []TeamTask{
		{ID: "task-1", Revision: 2, Subject: "写文档", Status: TeamTaskInProgress, Owner: "alice", Ready: true},
		{ID: "task-2", Revision: 1, Subject: "补接口", Status: TeamTaskPending, BlockedBy: []string{"task-1"}, Ready: false},
	}}
	ready := false
	res, err := TeamTaskListTool{Service: fs}.Execute(context.Background(),
		json.RawMessage(`{"status":"in_progress","owner":"alice","ready":false}`), &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("列表路径应当成功：err=%v res=%#v", err, res)
	}
	filter := fs.listFilter
	if filter.Status != TeamTaskInProgress || filter.Owner != "alice" {
		t.Errorf("过滤条件没透传：%#v", filter)
	}
	if filter.Ready == nil || *filter.Ready != ready {
		t.Errorf("ready=false 必须传成指针（要和「没传」区分开）：%#v", filter.Ready)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 2 {
		t.Fatalf("应当一行一条共 2 行，实际 %q", res.Content)
	}
	if !strings.Contains(lines[0], "task-1") || !strings.Contains(lines[0], "进行中") || !strings.Contains(lines[0], "alice") {
		t.Errorf("任务行内容不对：%q", lines[0])
	}
	if !strings.Contains(lines[1], "task-2") || !strings.Contains(lines[1], "待办") || !strings.Contains(lines[1], "依赖未完成") {
		t.Errorf("被依赖挡住的任务要标出来：%q", lines[1])
	}
	rows, ok := res.Display["tasks"].([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("Display.tasks 形状不对：%#v", res.Display)
	}
	if rows[0]["owner"] != "alice" || rows[0]["revision"] != 2 {
		t.Errorf("任务 Display 不对：%#v", rows[0])
	}
	if blocked, ok := rows[1]["blocked_by"].([]string); !ok || len(blocked) != 1 || blocked[0] != "task-1" {
		t.Errorf("blocked_by 应原样带出：%#v", rows[1]["blocked_by"])
	}
}

func TestTeamTaskListToolEmptyValidationAndErrors(t *testing.T) {
	// 空列表：Display 必须是空数组而不是 null。
	res, err := TeamTaskListTool{Service: &fakeTeamService{}}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("空列表不是错误：err=%v res=%#v", err, res)
	}
	if res.Content != "当前没有任务。" {
		t.Fatalf("空列表文案 = %q", res.Content)
	}
	rows, ok := res.Display["tasks"].([]map[string]any)
	if !ok || len(rows) != 0 {
		t.Fatalf("空列表 Display 必须是空数组：%#v", res.Display)
	}

	// status 只认契约里的三值。
	fs := &fakeTeamService{}
	res, err = TeamTaskListTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{"status":"done"}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "status") {
		t.Fatalf("非法 status 应报错：err=%v res=%#v", err, res)
	}
	if fs.listParent != "" {
		t.Fatalf("非法 status 不该调用 Service")
	}

	// Service 报错折成 Result。
	fs = &fakeTeamService{listErr: errors.New("数据库暂时不可用")}
	res, err = TeamTaskListTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "数据库暂时不可用") {
		t.Fatalf("Service 报错应折成 IsError：err=%v res=%#v", err, res)
	}
}

func TestTeamTaskGetTool(t *testing.T) {
	fs := &fakeTeamService{getRet: TeamTask{
		ID: "task-2", Revision: 4, Subject: "补接口", Description: "补 GET /api/conversations/:id/team",
		Status: TeamTaskInProgress, Owner: "alice", BlockedBy: []string{"task-1"}, WriteScopes: []string{"web/"},
		Ready: false,
	}}
	res, err := TeamTaskGetTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{"task_id":" task-2 "}`), &Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("读取路径应当成功：err=%v res=%#v", err, res)
	}
	if fs.getID != "task-2" {
		t.Fatalf("task_id 应去空白后透传，实际 %q", fs.getID)
	}
	for _, want := range []string{"task-2", "revision: 4", "owner: alice", "task-1", "还有未完成", "补 GET /api/conversations/:id/team", "web/"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("任务详情缺少 %q：\n%s", want, res.Content)
		}
	}
	task, ok := res.Display["task"].(map[string]any)
	if !ok || task["revision"] != 4 || task["owner"] != "alice" || task["ready"] != false {
		t.Fatalf("Display.task 形状不对：%#v", res.Display)
	}

	// 缺 task_id / 服务报错。
	res, err = TeamTaskGetTool{Service: &fakeTeamService{}}.Execute(context.Background(), json.RawMessage(`{}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "task_id") {
		t.Fatalf("缺 task_id 应报错：err=%v res=%#v", err, res)
	}
	fs = &fakeTeamService{getErr: errors.New("任务 task-9 不存在")}
	res, err = TeamTaskGetTool{Service: fs}.Execute(context.Background(), json.RawMessage(`{"task_id":"task-9"}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "task-9 不存在") {
		t.Fatalf("Service 报错应折成 IsError：err=%v res=%#v", err, res)
	}
}

// ---------------------------------------------------------------------------
// team_task_update
// ---------------------------------------------------------------------------

func TestTeamTaskUpdateToolValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantSub string
	}{
		{"缺 task_id", `{"expected_revision":1,"action":"claim"}`, "task_id"},
		{"缺 expected_revision", `{"task_id":"task-1","action":"claim"}`, "expected_revision"},
		{"expected_revision 为负", `{"task_id":"task-1","expected_revision":-1,"action":"claim"}`, "expected_revision"},
		{"缺 action", `{"task_id":"task-1","expected_revision":1}`, "action"},
		{"action 非法", `{"task_id":"task-1","expected_revision":1,"action":"finish"}`, "action"},
		{"reassign 缺 owner", `{"task_id":"task-1","expected_revision":1,"action":"reassign"}`, "owner"},
		{"blocked_by 含空项", `{"task_id":"task-1","expected_revision":1,"action":"set_dependencies","blocked_by":[""]}`, "blocked_by"},
		{"参数不是合法 JSON", `{"task_id":`, "JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeTeamService{}
			res, err := TeamTaskUpdateTool{Service: fs}.Execute(context.Background(), json.RawMessage(c.args), &Exec{SessionID: "9"})
			if err != nil {
				t.Fatalf("参数校验失败不能抛 error，实际 %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, c.wantSub) {
				t.Fatalf("非法参数应点出 %q，实际 %#v", c.wantSub, res)
			}
			if fs.updateID != "" {
				t.Fatalf("参数不合法时不该调用 Service")
			}
		})
	}
}

func TestTeamTaskUpdateToolSuccess(t *testing.T) {
	fs := &fakeTeamService{updateRet: TeamTask{
		ID: "task-1", Revision: 4, Subject: "写文档", Status: TeamTaskInProgress, Owner: "alice", Ready: true,
	}}
	tool := TeamTaskUpdateTool{Service: fs, ParentSessionID: "3"}
	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"task_id":"task-1","expected_revision":3,"action":"claim","write_scopes":["web/"]}`),
		&Exec{SessionID: "9"})
	if err != nil || res.IsError {
		t.Fatalf("成功路径应当成功：err=%v res=%#v", err, res)
	}
	if fs.updateParent != "3" || fs.updateID != "task-1" || fs.updateActor != "9" {
		t.Fatalf("服务调用参数不对：parent=%q id=%q actor=%q", fs.updateParent, fs.updateID, fs.updateActor)
	}
	if fs.updateRev != 3 || fs.updateAction != "claim" {
		t.Fatalf("CAS/action 没透传：rev=%d action=%q", fs.updateRev, fs.updateAction)
	}
	if len(fs.updatePatch.WriteScopes) != 1 || fs.updatePatch.WriteScopes[0] != "web/" {
		t.Fatalf("patch 没透传：%#v", fs.updatePatch)
	}
	if !strings.Contains(res.Content, "已认领") || !strings.Contains(res.Content, "task-1") || !strings.Contains(res.Content, "revision=4") {
		t.Fatalf("回执文案不对：%q", res.Content)
	}
	task, ok := res.Display["task"].(map[string]any)
	if !ok || task["id"] != "task-1" || task["revision"] != 4 || task["owner"] != "alice" {
		t.Fatalf("Display.task 形状不对：%#v", res.Display)
	}
}

func TestTeamTaskUpdateToolCASConflict(t *testing.T) {
	// CAS 冲突必须给出可执行的下一步（重读 revision），而不是让模型原样重试。
	fs := &fakeTeamService{updateErr: errors.New("TEAM_TASK_STALE_REVISION: 期望 revision 3，当前 5")}
	res, err := TeamTaskUpdateTool{Service: fs}.Execute(context.Background(), json.RawMessage(
		`{"task_id":"task-1","expected_revision":3,"action":"complete"}`), &Exec{SessionID: "9"})
	if err != nil {
		t.Fatalf("Service 报错必须折成 Result，不能抛 error，实际 %v", err)
	}
	if !res.IsError {
		t.Fatalf("CAS 冲突必须是 IsError：%#v", res)
	}
	for _, want := range []string{"CAS 冲突", "TEAM_TASK_STALE_REVISION", "team_task_get", "task-1"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("CAS 冲突文案缺少 %q：%q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "更新任务失败") {
		t.Errorf("CAS 冲突不该走通用文案：%q", res.Content)
	}
}

func TestTeamTaskUpdateToolServiceError(t *testing.T) {
	fs := &fakeTeamService{updateErr: errors.New("任务 task-9 不存在")}
	res, err := TeamTaskUpdateTool{Service: fs}.Execute(context.Background(), json.RawMessage(
		`{"task_id":"task-9","expected_revision":1,"action":"claim"}`), &Exec{SessionID: "9"})
	if err != nil || !res.IsError || !strings.Contains(res.Content, "task-9 不存在") {
		t.Fatalf("普通错误应走通用文案并带上原因：err=%v res=%#v", err, res)
	}
}

func TestIsStaleRevisionError(t *testing.T) {
	// 识别得出来才算 CAS 冲突；识别不出来（普通错误）不能伪装成 CAS。
	stale := []string{
		"TEAM_TASK_STALE_REVISION",
		"stale revision: expected 3, got 5",
		"任务 revision 不匹配：期望 3，当前 5",
		"revision 已经变了",
	}
	for _, s := range stale {
		if !isStaleRevisionError(errors.New(s)) {
			t.Errorf("%q 应识别为 CAS 冲突", s)
		}
	}
	fresh := []string{
		"任务 task-9 不存在",
		"expected_revision 必须是正整数",
		"数据库暂时不可用",
	}
	for _, s := range fresh {
		if isStaleRevisionError(errors.New(s)) {
			t.Errorf("%q 不该识别为 CAS 冲突", s)
		}
	}
	if isStaleRevisionError(nil) {
		t.Error("nil 不该识别为 CAS 冲突")
	}
}
