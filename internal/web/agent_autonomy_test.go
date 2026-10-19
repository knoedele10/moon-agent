package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
	"blog-my/internal/model"
)

func TestAutonomousAskRequiresBlocker(t *testing.T) {
	tools := buildAgentTools(agentToolPolicy{Mode: ModeAuto, Kind: KindWork})
	var ask agent.Tool
	for _, tool := range tools {
		if tool.Name() == "ask_user" {
			ask = tool
		}
	}
	if _, ok := ask.(autonomousAskTool); !ok {
		t.Fatalf("auto ask tool = %T", ask)
	}
	res, err := ask.Execute(context.Background(), json.RawMessage(`{"question":"要开始吗？","options":["开始","稍后"]}`), nil)
	if err != nil || !res.IsError || res.AwaitUser != "" {
		t.Fatalf("routine question paused the run: %+v, %v", res, err)
	}
	res, err = ask.Execute(context.Background(), json.RawMessage(`{"question":"服务需要登录，请完成登录后继续。","options":["已登录","取消任务"],"blocker_type":"access_required","blocking_reason":"服务返回未登录，没有可用的认证信息","attempted_steps":"已查询服务状态，返回401；本地配置没有登录信息"}`), nil)
	if err != nil || res.IsError || res.AwaitUser == "" {
		t.Fatalf("genuine blocker cannot ask: %+v, %v", res, err)
	}
	confirm := buildAgentTools(agentToolPolicy{Mode: ModeConfirm, Kind: KindWork})
	for _, tool := range confirm {
		if tool.Name() == "ask_user" {
			if _, ok := tool.(autonomousAskTool); ok {
				t.Fatal("confirm policy changed")
			}
		}
	}
}

// Exercise the full settings -> HTTP/SSE -> Eino -> filesystem path with a
// real database and a scripted model. No provider calls or user files are used.
func TestAutonomousHTTPExecution(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("requires local MySQL: AGENT_IT=1")
	}
	t.Setenv("AGENT_ENGINE", "eino")
	t.Setenv("AGENT_TOOLS", "1")
	t.Setenv("AGENT_AUTO_MEMORY", "0")
	t.Setenv("AGENT_MAX_ITERATIONS", "2")
	t.Setenv("AGENT_MAX_TOOL_CALLS", "1")
	t.Setenv("AGENT_LIMIT_RECOVERY_ROUNDS", "2")
	t.Run("decide_then_write_verify_and_finish", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeConfirm)
		assistant := model.AgentAssistant{UserID: userID, Name: "autonomy-test", Mode: ModeConfirm, Enabled: true}
		if err := model.DB.Create(&assistant).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { model.DB.Delete(&assistant) })
		llm.SetMockScript([]llm.MockTurn{
			{ToolCalls: []llm.ToolCall{{ID: "routine-ask", Name: "ask_user", Arguments: `{"question":"要开始写文件吗？","options":["开始","稍后"]}`}}},
			permWriteCall(t, "write-result", "result.txt", "自主执行验证：中文内容"),
			{ToolCalls: []llm.ToolCall{{ID: "verify-result", Name: "read", Arguments: `{"file_path":"result.txt"}`}}},
			{Text: "已创建文件并读回确认。"},
		})
		body, err := json.Marshal(map[string]any{"model": "mock-echo", "content": "创建并核对一个文本文件，常规细节自行决定", "tools": true,
			"assistant_id": assistant.Id, "execution_mode": ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		frames := sseFrames(t, agentHTTPPost(t, r, string(body)))
		if len(permNeedInputs(frames)) != 0 {
			t.Fatalf("routine choice paused the HTTP run: %+v", frames)
		}
		if got := permReadFile(t, filepath.Join(root, "result.txt")); got != "自主执行验证：中文内容" {
			t.Fatalf("unexpected output: %q", got)
		}
		results := permToolResults(frames)
		if len(results) != 3 || results[0]["ok"] != false || results[1]["ok"] != true || results[2]["ok"] != true {
			t.Fatalf("tool flow: %+v", results)
		}
		for _, frame := range frames {
			if frame.Name == "error" {
				t.Fatalf("HTTP run failed: %+v", frame)
			}
		}
	})
	t.Run("final_limit_reports_instead_of_node_error", func(t *testing.T) {
		t.Setenv("AGENT_MAX_ITERATIONS", "1")
		t.Setenv("AGENT_MAX_TOOL_CALLS", "8")
		t.Setenv("AGENT_LIMIT_RECOVERY_ROUNDS", "1")
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeAuto)
		llm.SetMockScript([]llm.MockTurn{
			{ToolCalls: []llm.ToolCall{{ID: "list-1", Name: "list", Arguments: `{}`}}},
			{ToolCalls: []llm.ToolCall{{ID: "list-2", Name: "list", Arguments: `{}`}}},
		})
		frames := permHTTPChat(t, r, 0, "检查这个目录")
		var report, incomplete bool
		for _, frame := range frames {
			if frame.Name == "error" {
				t.Fatalf("internal limit error leaked to UI: %+v", frame)
			}
			if frame.Name == "delta" {
				text, _ := frame.Data["content"].(string)
				report = report || strings.Contains(text, "自动续行后的执行上限")
			}
			if frame.Name == "done" {
				incomplete = frame.Data["incomplete"] == true && frame.Data["reason"] == "execution_limit"
			}
		}
		if !report || !incomplete || len(permToolResults(frames)) != 2 {
			t.Fatalf("missing final progress state: %+v", frames)
		}
	})
	t.Run("switch_back_to_confirm_overrides_auto_preset", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeAuto)
		assistant := model.AgentAssistant{UserID: userID, Name: "auto-preset", Mode: ModeAuto, Enabled: true}
		if err := model.DB.Create(&assistant).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { model.DB.Delete(&assistant) })
		llm.SetMockScript([]llm.MockTurn{permWriteCall(t, "confirm-write", "pending.txt", "not authorized yet"), {Text: "不该执行"}})
		body, err := json.Marshal(map[string]any{"model": "mock-echo", "content": "创建文件", "tools": true, "assistant_id": assistant.Id, "execution_mode": ModeConfirm})
		if err != nil {
			t.Fatal(err)
		}
		frames := sseFrames(t, agentHTTPPost(t, r, string(body)))
		questions := permNeedInputs(frames)
		if len(questions) != 1 || questions[0]["confirm"] != true {
			t.Fatalf("mode switch bypassed confirmation: %+v", frames)
		}
		permAssertAbsent(t, filepath.Join(root, "pending.txt"))
	})
	t.Run("necessary_question_still_waits_for_real_user", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeAuto)
		llm.SetMockScript([]llm.MockTurn{
			{ToolCalls: []llm.ToolCall{{ID: "real-blocker", Name: "ask_user", Arguments: `{"question":"需要你先登录服务","options":["已登录","取消"],"blocker_type":"access_required","blocking_reason":"接口返回401，需要本人登录","attempted_steps":"已查看服务状态和本地设置，没有登录凭证"}`}}},
			permWriteCall(t, "should-not-run", "pending.txt", "模型不能替用户回答"),
		})
		frames := permHTTPChat(t, r, 0, "处理需要登录的任务")
		questions := permNeedInputs(frames)
		if len(questions) != 1 || questions[0]["confirm"] != false || len(permToolResults(frames)) != 1 {
			t.Fatalf("necessary question did not pause: %+v", frames)
		}
		permAssertAbsent(t, filepath.Join(root, "pending.txt"))
	})
}

func TestExecutionModeRequestValidation(t *testing.T) {
	c := agentTestContext()
	if !setExecutionMode(c, " AUTO ") {
		t.Fatal("valid mode rejected")
	}
	if got := turnInputFromGin(c).executionMode; got != ModeAuto {
		t.Fatalf("request override lost: %q", got)
	}
	bad := agentTestContext()
	if setExecutionMode(bad, "invalid") || bad.Writer.Status() != 400 {
		t.Fatal("invalid mode accepted")
	}
}

func TestAutonomousRecoveryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"", 2}, {"0", 0}, {"3", 3}, {"99", 8}, {"bad", 2}} {
		t.Setenv("AGENT_LIMIT_RECOVERY_ROUNDS", tc.value)
		if got := agentLimitRecoveryRounds(ModeAuto); got != tc.want {
			t.Fatalf("%q: got %d", tc.value, got)
		}
		if agentLimitRecoveryRounds(ModeConfirm) != 0 || agentLimitRecoveryRounds(ModePlan) != 0 {
			t.Fatal("non-auto mode must retain strict limits")
		}
	}
	if !strings.Contains(modePrompt(ModeAuto, ""), "自行决定") {
		t.Fatal("missing autonomous decision guidance")
	}
}
