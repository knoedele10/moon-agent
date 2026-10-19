package web

// 任务 A「模式 × 位置 × 路径形态」权限矩阵的 **HTTP 层端到端**测试。
//
// 这一组测试不走 runAgentChat 内部函数，而是真的：
//   PUT /api/agent/settings 保存工作区与档位 → POST /api/chat（SSE）→ 工具真的执行 → 检查磁盘。
// 模型走 mock 后端（llm.SetMockScript），不需要凭证、不联网；数据库用 .env 里的真 MySQL。
//
// 运行方式（默认跳过，避免日常 go test 依赖 MySQL）：
//
//	$env:AGENT_IT='1'
//	go test ./internal/web/ -run TestPermHTTPMatrix -count=1 -v
//
// 与 web/agent_confirm_test.go / web/agent_pending_test.go 的分工：
// 那边钉的是壳的判定逻辑（纯函数与单工具），这边钉的是「从 HTTP 请求进来、
// 经过档位解析与确认重放、到文件真的落盘/拒绝」的完整链路。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// permHTTPEngine 返回一个注册了 /api/chat 与 /api/agent/settings 的测试路由。
// 复用 agentHTTPTestSetup：它负责 mock 后端注册、真实 MySQL 初始化与用户级清理。
func permHTTPEngine(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	r, userID := agentHTTPTestSetup(t)
	r.PUT("/api/agent/settings", putAgentSettings)
	t.Cleanup(func() {
		if model.DB == nil {
			return
		}
		model.DB.Where("user_id = ?", userID).Delete(&model.AgentSetting{})
		model.DB.Where("user_id = ?", userID).Delete(&model.AgentPendingTool{})
	})
	return r, userID
}

// permHTTPSetWorkspace 走真实的 PUT /api/agent/settings 保存工作区与档位。
func permHTTPSetWorkspace(t *testing.T, r *gin.Engine, root, mode string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"workspace": root, "mode": mode})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/agent/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/agent/settings 期望 200，got %d: %s", w.Code, w.Body.String())
	}
}

// permHTTPChat 发一次 /api/chat 并解析 SSE 帧；convID=0 表示新会话。
func permHTTPChat(t *testing.T, r *gin.Engine, convID uint, content string) []agentTestEvent {
	t.Helper()
	body := map[string]any{"model": "mock-echo", "content": content, "tools": true}
	if convID != 0 {
		body["conversation_id"] = convID
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return sseFrames(t, agentHTTPPost(t, r, string(b)))
}

// permOnlyConversationID 取该测试用户唯一的会话 id（新会话请求跑完后调用）。
func permOnlyConversationID(t *testing.T, userID string) uint {
	t.Helper()
	ids := agentHTTPConversationIDs(t, userID)
	if len(ids) != 1 {
		t.Fatalf("期望该测试用户只有 1 个会话，实际 %d 个: %v", len(ids), ids)
	}
	return ids[0]
}

// permToolResults 返回所有 tool_result 帧的 payload。
func permToolResults(frames []agentTestEvent) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f.Name == "tool_result" {
			out = append(out, f.Data)
		}
	}
	return out
}

// permNeedInputs 返回所有 need_input 帧的 payload。
func permNeedInputs(frames []agentTestEvent) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f.Name == "need_input" {
			out = append(out, f.Data)
		}
	}
	return out
}

// permWriteCall 造一条 write 工具调用脚本（arguments 是严格 JSON）。
func permWriteCall(t *testing.T, id, path, content string) llm.MockTurn {
	t.Helper()
	args, err := json.Marshal(map[string]string{"file_path": path, "content": content})
	if err != nil {
		t.Fatalf("marshal write args: %v", err)
	}
	return llm.MockTurn{ToolCalls: []llm.ToolCall{{ID: id, Name: "write", Arguments: string(args)}}}
}

func permReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("期望文件已写出 %s，读取失败: %v", path, err)
	}
	return string(b)
}

func permAssertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("期望文件不存在，但它被写出来了: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("检查文件是否存在时出错 %s: %v", path, err)
	}
}

// TestPermHTTPMatrix 逐格跑权限矩阵。每个子测试独立用户、独立工作区，
// 因此互不污染；子测试之间不并行（mock 脚本是进程级状态）。
func TestPermHTTPMatrix(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设 AGENT_IT=1 才跑（见本文件顶部说明）")
	}

	t.Run("plan_工作区内_md_允许", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModePlan)
		target := filepath.Join(root, "plan-1.md")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "plan-1.md", "# 计划"),
			{Text: "已把计划存档。"},
		})

		frames := permHTTPChat(t, r, 0, "把计划存成 plan-1.md")
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("期望 1 条成功的 tool_result，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "# 计划" {
			t.Fatalf("plan-1.md 内容不对: %q", got)
		}
	})

	t.Run("plan_工作区内_非md_拒绝", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModePlan)
		target := filepath.Join(root, "notes.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "notes.txt", "不该写进去"),
			{Text: "计划模式不能写非 md。"},
		})

		frames := permHTTPChat(t, r, 0, "把笔记写成 txt")
		if need := permNeedInputs(frames); len(need) != 0 {
			t.Fatalf("非 md 应直接拒绝，不该弹确认: %v", need)
		}
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != false {
			t.Fatalf("期望 1 条失败的 tool_result，实际: %v", results)
		}
		summary, _ := results[0]["summary"].(string)
		if !strings.Contains(summary, "只能写 .md") {
			t.Fatalf("拒绝文案应说明只允许 .md，实际: %q", summary)
		}
		permAssertAbsent(t, target)
	})

	t.Run("plan_工作区外_拒绝", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		out := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModePlan)
		target := filepath.Join(out, "plan-outside.md")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", target, "# 不该落盘"),
			{Text: "计划模式不该写工作区外。"},
		})

		frames := permHTTPChat(t, r, 0, "把计划写到工作区外的绝对路径")
		if need := permNeedInputs(frames); len(need) != 0 {
			t.Fatalf("规范期望 plan 档工作区外直接拒绝，但实测弹了确认窗（当前实现是确认而非拒绝）: %v", need)
		}
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != false {
			t.Fatalf("期望 1 条失败的 tool_result，实际: %v", results)
		}
		summary, _ := results[0]["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			t.Fatal("拒绝文案不能为空")
		}
		permAssertAbsent(t, target)
	})

	t.Run("confirm_工作区内_确认后成功", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeConfirm)
		target := filepath.Join(root, "inside.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "inside.txt", "confirmed inside"),
			{Text: "已写入。"},
		})

		first := permHTTPChat(t, r, 0, "把 inside.txt 写出来")
		need := permNeedInputs(first)
		if len(need) != 1 {
			t.Fatalf("confirm 档第一次写必须弹一次确认，实际 need_input=%v", need)
		}
		if need[0]["confirm"] != true {
			t.Fatalf("need_input.confirm 应为 true，实际: %v", need[0])
		}
		permAssertAbsent(t, target) // 确认之前绝不能落盘

		convID := permOnlyConversationID(t, userID)
		second := permHTTPChat(t, r, convID, "确认")
		if again := permNeedInputs(second); len(again) != 0 {
			t.Fatalf("确认后不该再弹窗，实际: %v", again)
		}
		results := permToolResults(second)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("确认后应执行成功，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "confirmed inside" {
			t.Fatalf("inside.txt 内容不对: %q", got)
		}
	})

	t.Run("confirm_工作区外_绝对路径_确认后成功", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		out := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeConfirm)
		target := filepath.Join(out, "outside.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", target, "confirmed outside"),
			{Text: "已写到工作区外。"},
		})

		first := permHTTPChat(t, r, 0, "把文件写到工作区外的绝对路径")
		need := permNeedInputs(first)
		if len(need) != 1 {
			t.Fatalf("confirm 档外部写必须弹一次确认，实际 need_input=%v", need)
		}
		question, _ := need[0]["question"].(string)
		wantPath := filepath.ToSlash(filepath.Clean(target))
		if !strings.Contains(question, wantPath) {
			t.Fatalf("外部确认文案必须带完整路径 %q，实际: %q", wantPath, question)
		}
		if !strings.Contains(question, "工作区之外") {
			t.Fatalf("外部确认文案应说明是工作区之外，实际: %q", question)
		}
		display, _ := need[0]["display"].(map[string]any)
		if display["confirm"] != true {
			t.Fatalf("外部确认 display.confirm 应为 true，实际: %v", display)
		}
		permAssertAbsent(t, target)

		convID := permOnlyConversationID(t, userID)
		second := permHTTPChat(t, r, convID, "确认")
		if again := permNeedInputs(second); len(again) != 0 {
			t.Fatalf("确认后不该再弹第二层窗，实际: %v", again)
		}
		results := permToolResults(second)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("确认后外部写应执行成功，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "confirmed outside" {
			t.Fatalf("outside.txt 内容不对: %q", got)
		}
	})

	t.Run("confirm_工作区内_相对子目录路径_确认后成功", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeConfirm)
		// 用户报错原文里的 blog/DESIGN.md 就是这种相对路径：它按工作区解析，
		// 落在工作区内，不该被当成「工作区外的相对路径」拒绝。
		target := filepath.Join(root, "blog", "DESIGN.md")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "blog/DESIGN.md", "# 设计"),
			{Text: "已写入 blog/DESIGN.md。"},
		})

		first := permHTTPChat(t, r, 0, "把设计写到 blog/DESIGN.md")
		if need := permNeedInputs(first); len(need) != 1 {
			t.Fatalf("confirm 档工作区内写应弹一次确认，实际: %v", need)
		}
		permAssertAbsent(t, target)

		second := permHTTPChat(t, r, permOnlyConversationID(t, userID), "确认")
		results := permToolResults(second)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("相对子目录路径确认后应写成功，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "# 设计" {
			t.Fatalf("blog/DESIGN.md 内容不对: %q", got)
		}
	})

	t.Run("confirm_工作区外_相对路径_确认后拒绝文案可读", func(t *testing.T) {
		r, userID := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeConfirm)
		target := filepath.Join(root, "..", "escape.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "..\\escape.txt", "不该落盘"),
			{Text: "相对路径出不去。"},
		})

		// confirm 档对每次写都先弹一次风险确认；真正决定能不能落盘的是确认后的重放。
		first := permHTTPChat(t, r, 0, "用相对路径写到工作区外")
		if need := permNeedInputs(first); len(need) != 1 {
			t.Fatalf("confirm 档应先弹一次风险确认，实际: %v", need)
		}
		permAssertAbsent(t, target)

		second := permHTTPChat(t, r, permOnlyConversationID(t, userID), "确认")
		if again := permNeedInputs(second); len(again) != 0 {
			t.Fatalf("确认后不该再弹窗，实际: %v", again)
		}
		results := permToolResults(second)
		if len(results) != 1 || results[0]["ok"] != false {
			t.Fatalf("工作区外的相对路径确认后仍应被 fs 层拒绝，实际: %v", results)
		}
		summary, _ := results[0]["summary"].(string)
		if !strings.Contains(summary, "工作区外只接受绝对路径") {
			t.Fatalf("拒绝文案应明确要求绝对路径，实际: %q", summary)
		}
		if !strings.Contains(summary, "完整路径") {
			t.Fatalf("拒绝文案应提示给出完整路径，实际: %q", summary)
		}
		permAssertAbsent(t, target)
	})

	t.Run("auto_工作区内_直接成功", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeAuto)
		target := filepath.Join(root, "auto.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "auto.txt", "auto inside"),
			{Text: "已写入。"},
		})

		frames := permHTTPChat(t, r, 0, "直接写 auto.txt")
		if need := permNeedInputs(frames); len(need) != 0 {
			t.Fatalf("auto 档工作区内不该弹窗，实际: %v", need)
		}
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("auto 档工作区内应直接成功，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "auto inside" {
			t.Fatalf("auto.txt 内容不对: %q", got)
		}
	})

	t.Run("auto_工作区外_绝对路径_直接成功", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		root := t.TempDir()
		out := t.TempDir()
		permHTTPSetWorkspace(t, r, root, ModeAuto)
		target := filepath.Join(out, "auto-outside.txt")
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", target, "auto outside"),
			{Text: "已写到外面。"},
		})

		frames := permHTTPChat(t, r, 0, "直接写到工作区外")
		if need := permNeedInputs(frames); len(need) != 0 {
			t.Fatalf("auto 档工作区外不该弹窗，实际: %v", need)
		}
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != true {
			t.Fatalf("auto 档工作区外应直接成功，实际: %v", results)
		}
		if got := permReadFile(t, target); got != "auto outside" {
			t.Fatalf("auto-outside.txt 内容不对: %q", got)
		}
	})

	t.Run("无工作区_文件工具不注册", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		t.Setenv("AGENT_FS_ROOT", "")
		t.Setenv("AGENT_MODE", ModeAuto)
		llm.SetMockScript([]llm.MockTurn{
			permWriteCall(t, "c1", "nowhere.txt", "没有工作区"),
			{Text: "没有文件工具可用。"},
		})

		frames := permHTTPChat(t, r, 0, "随便写一个文件")
		results := permToolResults(frames)
		if len(results) != 1 || results[0]["ok"] != false {
			t.Fatalf("没有工作区时 write 应不可用（失败回灌），实际: %v", results)
		}
		summary, _ := results[0]["summary"].(string)
		if !strings.Contains(summary, "未知工具") {
			t.Fatalf("拒绝文案应说明工具不存在，实际: %q", summary)
		}
	})

	t.Run("工作区不存在_设置接口给出可区分文案", func(t *testing.T) {
		r, _ := permHTTPEngine(t)
		// Z: 盘在测试机上不存在，用来模拟「工作区路径本身不可写/建不出来」。
		body, _ := json.Marshal(map[string]string{
			"workspace": `Z:\perm-http-does-not-exist-` + fmt.Sprint(os.Getpid()),
			"mode":      ModeAuto,
		})
		req := httptest.NewRequest(http.MethodPut, "/api/agent/settings", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("不可创建的工作区应 400，实际 %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("错误响应不是 JSON: %v (%s)", err, w.Body.String())
		}
		if !strings.Contains(resp["error"], "无法创建该目录") {
			t.Fatalf("文案应说明目录不可创建（而不是权限没开），实际: %q", resp["error"])
		}
	})
}
