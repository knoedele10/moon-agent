package agent

import (
	"context"
	"encoding/json"
	"testing"

	"blog-my/internal/llm"
)

// 未确认工具仍沿用 OK=true 的呈现契约，但必须单独标出等待，避免算入成功经验。
func TestAwaitUserToolResultFlagAcrossEngines(t *testing.T) {
	for _, engine := range []string{"legacy", "eino"} {
		t.Run(engine, func(t *testing.T) {
			m := &fakeModel{steps: []scriptStep{{calls: []llm.ToolCall{{ID: "confirmation", Name: "skill_run", Arguments: `{}`}}}}}
			pending := &fakeTool{name: "skill_run", exec: func(context.Context, json.RawMessage, *Exec) (Result, error) {
				return Result{Content: "等待确认，尚未执行", AwaitUser: "是否执行这个技能？"}, nil
			}}
			cfg := Config{Model: "await-test", Tools: []Tool{pending}}
			if engine == "eino" {
				cfg.EinoModel = &safetyScriptModel{script: m}
			}
			events, err := runScripted(t, context.Background(), m, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := onlyResult(t, events)
			if !result.OK || !result.AwaitUser || countKind(events, EvNeedInput) != 1 || countKind(events, EvDone) != 0 {
				t.Fatalf("等待确认应独立于成功显示，且立即停轮：%+v", events)
			}
			if m.callCount() != 1 {
				t.Fatalf("等待用户后仍请求了模型 %d 次", m.callCount())
			}
		})
	}
}
