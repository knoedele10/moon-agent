package web

import (
	"context"
	"encoding/json"
	"testing"

	"blog-my/internal/agent"
)

func TestRuntimeExtrasRespectExecutionPolicy(t *testing.T) {
	tools := []agent.Tool{
		hermesWiringTool{"skill_improve"}, hermesWiringTool{"memory_note"}, hermesWiringTool{"mcp__demo__write"},
	}
	plan := runtimeExtrasForPolicy(tools, agentToolPolicy{Kind: KindWork, Mode: ModePlan})
	if len(plan) != 1 || plan[0].Name() != "memory_note" {
		t.Fatalf("计划档不应获得技能修改或 MCP：%v", sortedToolNames(plan))
	}
	confirmed := runtimeExtrasForPolicy(tools[:1], agentToolPolicy{Kind: KindWork, Mode: ModeConfirm})
	res, err := confirmed[0].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || res.AwaitUser == "" || res.Content == "skill_improve ok" {
		t.Fatalf("确认档扩展工具绕过确认：%+v, %v", res, err)
	}
	child := runtimeExtrasForPolicy(tools[:1], agentToolPolicy{Kind: KindWork, Mode: ModeConfirm, Child: true})
	res, err = child[0].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError || res.Display["code"] != "SUBAGENT_DENIED" {
		t.Fatalf("子级不应自行执行待确认工具：%+v, %v", res, err)
	}
	auto := runtimeExtrasForPolicy(tools[:1], agentToolPolicy{Kind: KindCode, Mode: ModeAuto})
	res, err = auto[0].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || res.IsError || res.AwaitUser != "" || res.Content != "skill_improve ok" {
		t.Fatalf("自主代码档应能改进技能：%+v, %v", res, err)
	}
}

func TestRuntimeExtrasRespectConversationKind(t *testing.T) {
	tools := []agent.Tool{
		hermesWiringTool{"session_search"}, hermesWiringTool{"memory_note"},
		hermesWiringTool{"skill_list"}, hermesWiringTool{"skill_improve"}, hermesWiringTool{"mcp__demo__write"},
	}
	chat := runtimeExtrasForPolicy(tools, agentToolPolicy{Kind: KindChat, Mode: ModeAuto})
	if len(chat) != 2 {
		t.Fatalf("日常聊天只扩展记忆能力：%v", sortedToolNames(chat))
	}
	learn := runtimeExtrasForPolicy(tools, agentToolPolicy{Kind: KindLearn, Mode: ModeAuto})
	if len(learn) != 3 {
		t.Fatalf("学习用途不应得到技能写入或 MCP：%v", sortedToolNames(learn))
	}
}

func TestRuntimeLearningRespectsActualCapabilities(t *testing.T) {
	t.Setenv("AGENT_TOOLSETS", "core,fs")
	if runtimeMayLearn(agentToolPolicy{Kind: KindWork, Mode: ModeAuto}) {
		t.Fatal("未开放技能工具集的轮次不能后台修改技能")
	}
	t.Setenv("AGENT_TOOLSETS", "skill,evolve")
	if !runtimeMayLearn(agentToolPolicy{Kind: KindCode, Mode: ModeAuto}) {
		t.Fatal("开放技能工具的自主代码会话应允许固化")
	}
	for _, p := range []agentToolPolicy{
		{Kind: KindWork, Mode: ModePlan}, {Kind: KindWork, Mode: ModeConfirm},
		{Kind: KindLearn, Mode: ModeAuto}, {Kind: KindChat, Mode: ModeAuto},
		{Kind: KindWork, Mode: ModeAuto, Child: true},
	} {
		if runtimeMayLearn(p) {
			t.Fatalf("受限轮次可后台改技能：%+v", p)
		}
	}
}

func TestSessionSearchRejectsUnboundedAndCancelledRequests(t *testing.T) {
	tool := sessionSearchTool{userID: "test-user"}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("不应无条件扫描全部历史：%+v, %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = tool.Execute(ctx, json.RawMessage(`{"query":"历史"}`), nil); err == nil {
		t.Fatal("取消的检索仍然访问数据库")
	}
}
