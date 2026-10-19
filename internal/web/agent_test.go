package web

// task-40（web 接线）的单元测试。
//
// 全部离线：模型走 llm 内置的 mock 后端（无需凭证），记忆检索指向一个不可达端口
// 以便走"检索失败降级"路径，DB 未初始化（落库函数会静默跳过）。
// 真实模型 + 真实 MySQL 的端到端验证在 docs/verify 里由 verifier 独立复核。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// agentTestRegistry 装好 mock 后端并把 agent 的模型调用缝指向它。
func agentTestRegistry(t *testing.T) *llm.Registry {
	t.Helper()
	gin.SetMode(gin.TestMode)

	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	registry = reg

	agent.SetStreamer(agent.RegistryStreamer(reg))
	t.Cleanup(func() {
		agent.SetStreamer(nil)
		llm.ClearMockScript()
		registry = nil
	})
	return reg
}

// agentTestEnv 设置 agent 相关环境变量。记忆检索指向不可达端口：
// 连接被拒会立刻失败 → kbMemoryPrompt 降级为「无记忆」，测试不会挂在等 Ollama 上。
func agentTestEnv(t *testing.T, enabled bool, root string) {
	t.Helper()
	if enabled {
		t.Setenv("AGENT_TOOLS", "1")
	} else {
		t.Setenv("AGENT_TOOLS", "")
	}
	t.Setenv("AGENT_FS_ROOT", root)
	// 单测默认走 auto：这样文件工具的行为断言（如越界写入必须失败）不被确认壳拦住。
	t.Setenv("AGENT_MODE", "auto")
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1")
}

func agentTestContext() *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	return c
}

type agentTestEvent struct {
	Name string
	Data map[string]any
}

func agentTestSender(into *[]agentTestEvent) func(string, any) {
	return func(name string, payload any) {
		b, _ := json.Marshal(payload)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		*into = append(*into, agentTestEvent{Name: name, Data: m})
	}
}

// runAgentChatFromTestCtx 是测试用的适配层：runAgentChat 现在收 turnInput（因为后台唤醒轮次没有
// gin.Context），而测试都在 gin 测试上下文上准备好请求级配置，这里把两者接起来，测试正文就不用改。
func runAgentChatFromTestCtx(
	c *gin.Context,
	conv *model.Conversation,
	userID, modelID, key, content string,
	history []llm.Message,
	sendEvent func(string, any),
) {
	in := turnInputFromGin(c)
	in.conv = conv
	in.userID = userID
	in.modelID = modelID
	in.key = key
	in.content = content
	in.history = history
	in.sendEvent = sendEvent
	runAgentChat(in)
}

// ---------------------------------------------------------------------------
// 开关与环境变量
// ---------------------------------------------------------------------------

func TestAgentToolsEnabledParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"on", true}, {"Yes", true},
		{"0", false}, {"false", false}, {"off", false}, {"", false}, {"随便", false},
	}
	for _, c := range cases {
		t.Setenv("AGENT_TOOLS", c.raw)
		if got := agentToolsEnabled(); got != c.want {
			t.Errorf("AGENT_TOOLS=%q: got %v, want %v", c.raw, got, c.want)
		}
	}
}

// TestShouldUseAgent 钉住向后兼容：两个条件都满足才走 agent。
func TestShouldUseAgent(t *testing.T) {
	cases := []struct {
		requested bool
		enabled   string
		want      bool
	}{
		{true, "1", true},
		{true, "", false},   // 客户端要，服务端没开 → 老路径
		{false, "1", false}, // 服务端开了，客户端没要 → 老路径
		{false, "", false},  // 都没 → 老路径（改造前的默认）
	}
	for _, c := range cases {
		t.Setenv("AGENT_TOOLS", c.enabled)
		if got := shouldUseAgent(c.requested); got != c.want {
			t.Errorf("requested=%v enabled=%q: got %v, want %v", c.requested, c.enabled, got, c.want)
		}
	}
}

func TestAgentIntEnv(t *testing.T) {
	t.Setenv("AGENT_MAX_ITERATIONS", "7")
	if got := agentIntEnv("AGENT_MAX_ITERATIONS"); got != 7 {
		t.Errorf("got %d, want 7", got)
	}
	for _, bad := range []string{"", "0", "-3", "abc"} {
		t.Setenv("AGENT_MAX_ITERATIONS", bad)
		if got := agentIntEnv("AGENT_MAX_ITERATIONS"); got != 0 {
			t.Errorf("%q: got %d, want 0（交给 agent 用默认值）", bad, got)
		}
	}
}

// TestSetupInstallsStreamer 钉住一个**真漏过的生产缺口**。
//
// 背景：`agent.SetStreamer` 最初只在测试里被调用过，生产入口从没装，于是线上
// `tools=true` 直接返回 `agent: 未安装模型调用实现`。**所有单测都是绿的**——
// 因为测试自己会装。这个缺口是 cua-ui 跑真机端到端才发现的。
//
// 教训：安装动作必须在 `Setup` 里，不能靠调用方记得。这条断言把它钉死。
func TestSetupInstallsStreamer(t *testing.T) {
	agent.SetStreamer(nil)
	if agent.Streamer() != nil {
		t.Fatal("前置条件不成立：应当先卸掉 streamer")
	}
	t.Cleanup(func() { agent.SetStreamer(nil) })

	reg := llm.NewRegistry()
	reg.SetEndpoints(llm.DefaultEndpoints())
	Setup(gin.New(), reg)

	if agent.Streamer() == nil {
		t.Fatal("web.Setup 必须安装 agent 的模型调用缝，否则生产 tools=true 直接失败")
	}
}

// ---------------------------------------------------------------------------
// 工具集
// ---------------------------------------------------------------------------

// TestAgentToolsForRespectsRoot 钉住"默认安全"：没配根目录就一个文件工具都不给。
func TestAgentToolsForRespectsRoot(t *testing.T) {
	agentTestEnv(t, true, "")
	names := agentTestToolNames(t, "u1")
	for _, n := range []string{"read", "list", "write", "edit", "move", "delete", "read_image", "office_edit"} {
		if names[n] {
			t.Errorf("未配 AGENT_FS_ROOT 时不该注册文件工具 %q，实际注册了", n)
		}
	}
	// 记忆工具与根目录无关，始终注册。
	for _, n := range []string{"memory_search", "memory_write"} {
		if !names[n] {
			t.Errorf("记忆工具 %q 应当注册", n)
		}
	}

	agentTestEnv(t, true, t.TempDir())
	names = agentTestToolNames(t, "u1")
	for _, n := range []string{"read", "list", "write", "edit", "move", "delete", "read_image", "office_edit", "memory_search", "memory_write", "calculator", "datetime", "web_search", "web_reader", "ask_user", "json_format", "sql_query", "skill_run", "run_command"} {
		if !names[n] {
			t.Errorf("配了根目录后工具 %q 应当注册", n)
		}
	}
}

func TestBindTeammateReceivesWorkspaceAndModel(t *testing.T) {
	root := t.TempDir()
	conv := &model.Conversation{Id: 42}
	tools := bindSubagentContext(
		[]agent.Tool{agent.SpawnTeammateTool{Service: newTeamService(), Mode: ModeAuto}},
		conv, nil, "mock-model", KindWork, root, "42",
	)
	spawn, ok := tools[0].(agent.SpawnTeammateTool)
	if !ok {
		t.Fatalf("expected bound spawn_teammate, got %T", tools[0])
	}
	if spawn.ParentSessionID != "42" || spawn.RootSessionID != "42" || spawn.Model != "mock-model" || spawn.Kind != KindWork || spawn.Root != root {
		t.Fatalf("spawn_teammate lost its conversation context: %+v", spawn)
	}
	child := subagentToolsFor("user", agent.SubagentRequest{Root: spawn.Root, Kind: spawn.Kind, Mode: spawn.Mode}, 1)
	names := map[string]bool{}
	for _, tool := range child {
		names[tool.Name()] = true
	}
	for _, name := range []string{"read", "write", "team_task_list", "team_task_update"} {
		if !names[name] {
			t.Errorf("teammate is missing %s", name)
		}
	}
}

func agentTestToolNames(t *testing.T, userID string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	// 深度传 subagentMaxDepth()：这些用例看的是文件 / 记忆 / 技能工具，委派工具另有专门用例。
	for _, tool := range agentToolsFor(userID, agentFSRoot(), ModeAuto, true, subagentMaxDepth()) {
		names[tool.Name()] = true
	}
	return names
}

// TestAgentToolDefsAreValidJSONSchema 钉住：交给模型的参数定义必须是合法 JSON Schema 对象，
// 否则上游会拒掉整个请求（llm 侧会返回 ErrToolDefInvalid）。
func TestAgentToolDefsAreValidJSONSchema(t *testing.T) {
	agentTestEnv(t, true, t.TempDir())
	reg := agent.NewRegistry(agentToolsFor("u1", agentFSRoot(), ModeAuto, true, subagentMaxDepth())...)
	defs := reg.Defs()
	if len(defs) == 0 {
		t.Fatal("至少应暴露一个工具")
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
		if strings.TrimSpace(d.Name) == "" {
			t.Errorf("工具缺 name")
		}
		if strings.TrimSpace(d.Description) == "" {
			t.Errorf("工具 %s 缺 description（模型要靠它判断何时调用）", d.Name)
		}
		if d.Parameters["type"] != "object" {
			t.Errorf("工具 %s 的 parameters.type 应为 object，got %v", d.Name, d.Parameters["type"])
		}
		if _, ok := d.Parameters["properties"]; !ok {
			t.Errorf("工具 %s 的 parameters 缺 properties", d.Name)
		}
	}
	for _, name := range []string{"read", "skill_run", "run_command", "send_message", "team_task_list"} {
		if !names[name] {
			t.Errorf("工具目录缺少 %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 系统提示词
// ---------------------------------------------------------------------------

func TestAgentSystemPromptRootAndMemory(t *testing.T) {
	root := `E:\some\root`
	p := agentSystemPrompt(root, "<memory>用户喜欢简洁</memory>")
	if !strings.Contains(p, root) {
		t.Errorf("提示词应写明根目录，got:\n%s", p)
	}
	if !strings.Contains(p, "read_image") {
		t.Errorf("配了根目录时应介绍文件工具")
	}
	if !strings.Contains(p, "相对路径始终按工作区解析") || !strings.Contains(p, "完整绝对路径") {
		t.Errorf("提示词应明确相对路径归属及工作区外绝对路径要求，got:\n%s", p)
	}
	if !strings.Contains(p, "<memory>") {
		t.Errorf("有记忆时应注入记忆块")
	}
	if !strings.Contains(p, "memory_search") {
		t.Errorf("应介绍记忆工具")
	}

	// 没配根目录：必须明确告诉模型没有文件工具，而不是含糊不提——
	// 否则模型会去调一个不存在的工具，白白烧一轮。
	p2 := agentSystemPrompt("", "")
	if !strings.Contains(p2, "没有开启文件工具") {
		t.Errorf("未配根目录时应明确说明没有文件工具，got:\n%s", p2)
	}
	if strings.Contains(p2, "read_image") {
		t.Errorf("未配根目录时不该介绍文件工具")
	}
	if strings.Contains(p2, "<memory>") {
		t.Errorf("无记忆时不该留下空记忆块")
	}
}

// ---------------------------------------------------------------------------
// SSE 负载
// ---------------------------------------------------------------------------

// TestToolCallArguments 钉住：参数是合法 JSON 时原样嵌成对象（前端好渲染），
// 非法 JSON 时退回字符串——若硬塞 RawMessage，**整个 SSE 负载**都会变成非法 JSON。
func TestToolCallArguments(t *testing.T) {
	if got, ok := toolCallArguments(`{"a":1}`).(json.RawMessage); !ok {
		t.Errorf("合法 JSON 应转成 json.RawMessage，got %T", got)
	} else if string(got) != `{"a":1}` {
		t.Errorf("内容应原样保留，got %s", got)
	}
	if got := toolCallArguments("{不是 JSON"); got != "{不是 JSON" {
		t.Errorf("非法 JSON 应退回字符串，got %T %v", got, got)
	}
	if got := toolCallArguments("   "); got != "" {
		t.Errorf("空白应退回空串，got %v", got)
	}
}

// ---------------------------------------------------------------------------
// 端到端（mock 模型 + 真实文件系统）
// ---------------------------------------------------------------------------

// TestRunAgentChatWritesFileAndEmitsEvents 是 task-40 的核心验收：
// 脚本化 mock 模型发出一次 write 调用 → 断言**文件真的被写了**、SSE 事件序列正确。
func TestRunAgentChatWritesFileAndEmitsEvents(t *testing.T) {
	agentTestRegistry(t)
	tmp := t.TempDir()
	agentTestEnv(t, true, tmp)

	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "write",
			Arguments: `{"file_path":"a.txt","content":"hello from agent"}`,
		}}},
		{Text: "已经写好了。"},
	})

	var events []agentTestEvent
	c := agentTestContext()
	conv := &model.Conversation{Id: 1, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "把 hello from agent 写进 a.txt"}}

	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", history[0].Content, history, agentTestSender(&events))

	// 1) 文件系统真的变了——这是"工具真能跑"的唯一硬证据。
	got, err := os.ReadFile(filepath.Join(tmp, "a.txt"))
	if err != nil {
		t.Fatalf("a.txt 应当被 agent 写出来，读取失败: %v", err)
	}
	if string(got) != "hello from agent" {
		t.Fatalf("a.txt 内容不对: %q", got)
	}

	// 2) 事件序列
	var seq []string
	for _, e := range events {
		seq = append(seq, e.Name)
	}
	if len(seq) == 0 || seq[len(seq)-1] != "done" {
		t.Fatalf("最后一个事件应是 done，实际序列: %v", seq)
	}

	var sawCall, sawResult bool
	for _, e := range events {
		switch e.Name {
		case "tool_call":
			sawCall = true
			if e.Data["name"] != "write" {
				t.Errorf("tool_call.name 应为 write，got %v", e.Data["name"])
			}
			if _, ok := e.Data["arguments"].(map[string]any); !ok {
				t.Errorf("arguments 应是对象，got %T %v", e.Data["arguments"], e.Data["arguments"])
			}
			if e.Data["id"] != "call_1" {
				t.Errorf("tool_call.id 应为 call_1，got %v", e.Data["id"])
			}
		case "tool_result":
			sawResult = true
			if e.Data["ok"] != true {
				t.Errorf("tool_result.ok 应为 true，got %v", e.Data["ok"])
			}
			if e.Data["id"] != "call_1" {
				t.Errorf("tool_result.id 应与调用对应，got %v", e.Data["id"])
			}
		}
	}
	if !sawCall {
		t.Errorf("缺少 tool_call 事件，序列: %v", seq)
	}
	if !sawResult {
		t.Errorf("缺少 tool_result 事件，序列: %v", seq)
	}
}

// TestRunAgentChatSandboxDeniesEscape 是**负向对照**：让模型尝试写到根目录之外，
// 必须被拒绝、文件不得出现在根外，且循环要把失败**回灌**给模型而不是崩掉。
func TestRunAgentChatSandboxDeniesEscape(t *testing.T) {
	agentTestRegistry(t)
	root := t.TempDir()
	agentTestEnv(t, true, root)

	outside := filepath.Join(filepath.Dir(root), "escaped-agent-test.txt")
	_ = os.Remove(outside)
	t.Cleanup(func() { _ = os.Remove(outside) })

	llm.SetMockScript([]llm.MockTurn{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_x",
			Name:      "write",
			Arguments: `{"file_path":"../escaped-agent-test.txt","content":"不该被写出去"}`,
		}}},
		{Text: "被拒绝了。"},
	})

	var events []agentTestEvent
	c := agentTestContext()
	conv := &model.Conversation{Id: 2, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "写个越界文件"}}

	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", history[0].Content, history, agentTestSender(&events))

	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("沙箱红线被突破：根目录外的文件被写出来了 %s", outside)
	}

	var sawFail bool
	for _, e := range events {
		if e.Name == "tool_result" && e.Data["ok"] == false {
			sawFail = true
		}
		if e.Name == "error" {
			t.Fatalf("工具失败不该让整轮报错（契约 §3.5 要回灌给模型），got %v", e.Data)
		}
	}
	if !sawFail {
		t.Errorf("越界写入应产生 ok=false 的 tool_result")
	}
}

// TestRunAgentChatPlainTextNoTools 钉住：模型不调工具时就是一条普通回复，
// 不产生任何 tool_call/tool_result。
func TestRunAgentChatPlainTextNoTools(t *testing.T) {
	agentTestRegistry(t)
	agentTestEnv(t, true, t.TempDir())

	var events []agentTestEvent
	c := agentTestContext()
	conv := &model.Conversation{Id: 3, UserID: "u1"}
	history := []llm.Message{{Role: llm.RoleUser, Content: "你好"}}

	runAgentChatFromTestCtx(c, conv, "u1", "mock-echo", "", "你好", history, agentTestSender(&events))

	for _, e := range events {
		if e.Name == "tool_call" || e.Name == "tool_result" {
			t.Errorf("没调工具却出现了 %s 事件", e.Name)
		}
	}
	if len(events) == 0 || events[len(events)-1].Name != "done" {
		t.Errorf("应以 done 收尾，got %v", events)
	}
}
