package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"blog-my/internal/llm"
	"blog-my/internal/model"
)

// 真实 TCP/HTTP + MySQL + 两种执行器；模型用确定性脚本，避免把供应商波动当成学习效果。
func TestHermesLearningLifecycleHTTP(t *testing.T) {
	if os.Getenv("AGENT_IT") != "1" {
		t.Skip("需要真实 MySQL：设置 AGENT_IT=1，并建议指定独立 DB_NAME")
	}
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			hermesClearEnv(t)
			t.Setenv("AGENT_ENGINE", engine)
			t.Setenv("AGENT_EVOLVE", "record")
			t.Setenv("AGENT_TOOL_DISCLOSURE", "search")
			t.Setenv("AGENT_SKILL_UNDERSTAND", "0")
			t.Setenv("AGENT_SKILL_RERANK", "0")
			t.Setenv("AGENT_AUTO_MEMORY", "0")
			agentTestEnv(t, true, t.TempDir())
			router, userID := agentHTTPTestSetup(t)
			router.GET("/api/agent/evolution", getEvolutionStatus)
			router.GET("/api/agent/tool-catalog", getToolCatalog)
			server := httptest.NewServer(router)
			defer server.Close()
			t.Cleanup(func() { model.DB.Where("user_id = ?", userID).Delete(&model.AgentSkill{}) })
			llm.SetMockScript([]llm.MockTurn{
				{ToolCalls: []llm.ToolCall{{ID: "calc1", Name: "calculator", Arguments: `{"expression":"2+3"}`}}},
				{ToolCalls: []llm.ToolCall{{ID: "time1", Name: "datetime", Arguments: `{}`}}},
				{ToolCalls: []llm.ToolCall{{ID: "calc2", Name: "calculator", Arguments: `{"expression":"4+5"}`}}},
				{ToolCalls: []llm.ToolCall{{ID: "time2", Name: "datetime", Arguments: `{}`}}},
				{Text: "计算和时间核对已完成。"},
			})
			post := func(content string) string {
				t.Helper()
				response, err := http.Post(server.URL+"/api/chat", "application/json", strings.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("HTTP %d: %s, %v", response.StatusCode, body, err)
				}
				return string(body)
			}
			raw := post(`{"model":"mock-echo","content":"计算两组数值并核对当前时间","tools":true}`)
			calls := 0
			for _, frame := range sseFrames(t, raw) {
				if frame.Name == "error" {
					t.Fatalf("工具会话失败：%v", frame.Data)
				}
				if frame.Name == "tool_result" {
					calls++
					if frame.Data["ok"] != true {
						t.Fatalf("工具没有真正成功：%v", frame.Data)
					}
				}
			}
			if calls != 4 {
				t.Fatalf("应真正完成4个工具，实际 %d：%s", calls, raw)
			}
			evo := newHermesEvolve(userID)
			report, err := evo.Curate(context.Background(), true)
			if err != nil || len(report.Applied) == 0 {
				t.Fatalf("成功轨迹未固化：%+v, %v", report, err)
			}
			items, err := evo.store.List(userID)
			if err != nil || len(items) != 1 {
				t.Fatalf("技能未真实落库：%+v, %v", items, err)
			}
			learned := items[0]
			if !learned.Enabled || !strings.Contains(learned.Body, "expression") {
				t.Fatalf("固化技能缺少实际参数：%+v", learned)
			}
			llm.SetMockScript([]llm.MockTurn{{Text: "已按积累的技能完成这次任务。"}})
			raw = post(`{"model":"mock-echo","content":"` + learned.Name + `","tools":true}`)
			if !strings.Contains(raw, `"name":"`+learned.Name+`"`) {
				t.Fatalf("后续会话未召回新技能：%s", raw)
			}
			stat, found, err := evo.ledger.Stat(learned.Name)
			if err != nil || !found || stat.Uses != 1 || stat.OK != 1 {
				t.Fatalf("技能复用没有进入反馈账本：%+v, %v", stat, err)
			}
		})
	}
}
