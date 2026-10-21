package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"blog-my/internal/agent"
	"blog-my/internal/agent/toolsys"
	"blog-my/internal/llm"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type disclosureTool struct {
	name   string
	params map[string]any
	exec   func(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error)
}

func (t *disclosureTool) Name() string               { return t.name }
func (t *disclosureTool) Description() string        { return "测试工具 " + t.name }
func (t *disclosureTool) Parameters() map[string]any { return t.params }
func (t *disclosureTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	return t.exec(ctx, raw, ex)
}

type disclosureObservation struct {
	mu     sync.Mutex
	rounds [][]string
}

type disclosureModel struct {
	observation *disclosureObservation
	tools       []*schema.ToolInfo
}

func (m *disclosureModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return &disclosureModel{observation: m.observation, tools: append([]*schema.ToolInfo(nil), tools...)}, nil
}

func (m *disclosureModel) Generate(context.Context, []*schema.Message, ...einomodel.Option) (*schema.Message, error) {
	m.observation.mu.Lock()
	defer m.observation.mu.Unlock()
	names := make([]string, 0, len(m.tools))
	for _, info := range m.tools {
		names = append(names, info.Name)
	}
	m.observation.rounds = append(m.observation.rounds, names)
	turn := len(m.observation.rounds)
	call := llm.ToolCall{ID: fmt.Sprint(turn), Arguments: `{}`}
	switch turn {
	case 1:
		if len(names) != 1 || names[0] != "tool_search" {
			return nil, fmt.Errorf("首轮应只看到 tool_search，实际 %v", names)
		}
		call.Name, call.Arguments = "tool_search", `{"query":"read"}`
	case 2:
		found := false
		for _, info := range m.tools {
			if info.Name == "read" {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("ReAct 内第二轮未重新绑定被激活的 read：%v", names)
		}
		call.Name = "read"
	default:
		return &schema.Message{Role: schema.Assistant, Content: "读取完成。"}, nil
	}
	return llm.ToSchemaMessages([]llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}})[0], nil
}

func (m *disclosureModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// 必须走真正的 Eino ReAct 与真实 Catalog/SearchTool；模型在拿到新 schema 前拒绝猜工具。
func TestEinoToolSearchActivationUpdatesNextInnerModelTurn(t *testing.T) {
	var catalog *toolsys.Catalog
	search := &disclosureTool{name: "tool_search", params: toolsys.SearchTool(nil).Parameters()}
	search.exec = func(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
		return toolsys.SearchTool(catalog).Execute(ctx, raw, ex)
	}
	reads := 0
	read := &disclosureTool{name: "read", params: map[string]any{"type": "object", "properties": map[string]any{}}}
	read.exec = func(context.Context, json.RawMessage, *agent.Exec) (agent.Result, error) {
		reads++
		return agent.Result{Content: "实际读取的内容"}, nil
	}
	catalog = toolsys.NewCatalog(
		toolsys.Toolset{Name: "core", Risk: toolsys.RiskRead, Tools: []agent.Tool{search}},
		toolsys.Toolset{Name: "files", Risk: toolsys.RiskRead, Tools: []agent.Tool{read}},
	)
	observation := &disclosureObservation{}
	var results []agent.Event
	err := agent.Run(context.Background(), agent.Config{
		Model: "disclosure-test", EinoModel: &disclosureModel{observation: observation}, ToolSource: catalog, MaxIterations: 6,
	}, []llm.Message{{Role: llm.RoleUser, Content: "先找读取工具，再读内容"}}, func(ev agent.Event) {
		if ev.Kind == agent.EvToolResult {
			results = append(results, ev)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || len(observation.rounds) != 3 || len(results) != 2 || !results[1].OK {
		t.Fatalf("应在同一 ReAct pass 中完成搜索、执行、答复；reads=%d rounds=%v results=%+v", reads, observation.rounds, results)
	}
}
