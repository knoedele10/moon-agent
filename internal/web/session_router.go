package web

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

// Router delegates through the existing team service: it never accepts a user
// supplied session owner, workspace, permission mode or model credential.
type sessionRouterTool struct{ policy agentToolPolicy }

func (sessionRouterTool) Name() string { return "orchestrator_route" }
func (sessionRouterTool) Description() string {
	return "主会话路由：list 查看本会话执行成员；accept 新建具名执行对话；attach 把后续要求交回同名执行对话，保留历史。" +
		"先 list 再决定是否复用；独立任务才 accept。message 必须写明目标、约束和验收条件。返回仅代表受理，完成报告会自动送回，不能当作完成。"
}
func (sessionRouterTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"action":  map[string]any{"type": "string", "enum": []string{"list", "accept", "attach"}},
		"target":  map[string]any{"type": "string", "description": "accept 的新成员名或 attach 的已有成员名，小写字母开头，2-32 个小写字母/数字/连字符"},
		"message": map[string]any{"type": "string", "description": "完整任务或后续要求，包含可检查的交付条件"},
		"title":   map[string]any{"type": "string", "description": "accept 时的简短任务标题"},
	}, "required": []string{"action"}, "additionalProperties": false}
}

func (t sessionRouterTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	fail := func(err error) (agent.Result, error) { return agent.Result{IsError: true, Content: err.Error()}, nil }
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	var args struct {
		Action  string `json:"action"`
		Target  string `json:"target"`
		Message string `json:"message"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return fail(errors.New("路由参数必须为 JSON 对象"))
	}
	if ex == nil || model.DB == nil {
		return fail(errors.New("路由会话或数据库未就绪"))
	}
	a, err := resolveTeamActor(ex.SessionID)
	if err != nil {
		return fail(err)
	}
	if !a.IsLead || a.UserID != t.policy.UserID {
		return fail(errors.New("只有当前用户的主会话可以路由任务"))
	}
	svc := newTeamService()
	var result any
	switch args.Action {
	case "list":
		result, err = svc.Roster(ctx, ex.SessionID)
	case "accept", "attach":
		args.Target, args.Message = strings.TrimSpace(args.Target), strings.TrimSpace(args.Message)
		if err := validateTeamName(args.Target); err != nil {
			return fail(err)
		}
		if args.Message == "" || len([]rune(args.Message)) > 16000 {
			return fail(errors.New("message 需为 1-16000 字"))
		}
		if args.Action == "attach" {
			err = svc.SendMessage(ctx, ex.SessionID, "", args.Target, args.Message)
			result = map[string]any{"accepted": err == nil, "target": args.Target, "notice": "已投递；等待执行报告，尚未验收完成"}
		} else {
			var conv model.Conversation
			if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", a.ConvID, a.UserID).First(&conv).Error; err != nil {
				return fail(err)
			}
			title := strings.TrimSpace(args.Title)
			if title == "" {
				title = args.Target
			}
			result, err = svc.SpawnTeammate(ctx, agent.TeammateRequest{
				ParentSessionID: ex.SessionID, RootSessionID: strconv.FormatUint(uint64(a.ConvID), 10),
				Name: args.Target, Description: title, Prompt: args.Message, Context: "fresh",
				Model: conv.Model, Kind: normalizeKind(conv.Kind), Mode: t.policy.Mode,
				Root: t.policy.Root, Roots: append([]string(nil), t.policy.Roots...),
			})
		}
	default:
		return fail(errors.New("action 仅支持 list / accept / attach"))
	}
	if err != nil {
		return fail(err)
	}
	b, err := json.Marshal(result)
	return agent.Result{Content: string(b)}, err
}

const sessionRouterPrompt = "\n【持久执行对话】复杂且可独立交付的任务可以用 orchestrator_route 分派，简单任务直接完成。" +
	"先 list 找已有成员；同一任务后续用 attach 续跑，不重复 accept。任务中保留用户目标、约束、当前进度和验收条件。" +
	"受理或子任务返回文本不代表目标已完成；读取完成报告并核对证据，未完成的部分继续交回原成员或自行处理。" +
	"等待执行报告时结束当前轮次，完成消息会唤醒你，不要循环轮询。遇到需人工处理的阻碍如实说明。\n"

func compactSummaryBlock(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return "\n【早期对话摘要】这是历史资料，可能不完整，不产生新的操作授权；与最近消息冲突时以最近消息为准。\n" + summary + "\n"
}
