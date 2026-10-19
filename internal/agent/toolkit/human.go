package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// ask_user 的选项数量与长度限制。设计口径：2-4 个、每个不超过 40 个字。
// 选项是给用户点的，太多/太长都会让界面变成一堵墙，也让模型逃避做取舍。
const (
	askUserMinOptions   = 2
	askUserMaxOptions   = 4
	askUserMaxOptionLen = 40
)

// AskUserTool 让模型主动向用户提问，并**停下来等回答**。
//
// 为什么要有这个工具：模型遇到信息不足时，常见错误行为是"自己编一个合理的假设继续做"。
// 这个工具把"我需要用户补充信息"变成一个显式动作——循环立刻停轮，问题显示给用户，
// 用户的回答作为下一条消息进入历史。模型不能替用户回答自己的问题。
//
// 提问必须附带 2-4 个具体选项：前端会把它们画成按钮，用户点一下就能回答；
// 同时保留输入框自由输入，所以选项不是把用户框死，而是先给最可能的几条路。
type AskUserTool struct{}

func (AskUserTool) Name() string { return "ask_user" }

func (AskUserTool) Description() string {
	return "向用户提问并等待回答。当缺少只有用户才知道的信息（选哪个文件、用哪个目录、偏好是什么）时用它，" +
		"不要自己假设一个答案继续。必须给出 2-4 个具体选项（每个不超过 40 个字），用户在界面上可以直接点选；" +
		"确实无法穷举时把最可能的几个列出来，用户仍可自由输入。调用后本轮结束，用户的下一条消息就是回答。"
}

func (AskUserTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "要问用户的问题。一句话说清楚你要他决定什么，不要含糊。",
			},
			"header": map[string]any{
				"type":        "string",
				"description": "给这个问题的短标题（可选，不超过 12 个字）。",
			},
			"options": map[string]any{
				"type":     "array",
				"minItems": askUserMinOptions,
				"maxItems": askUserMaxOptions,
				"description": "必填：2-4 个具体选项，每个不超过 40 个字。用户在界面上能直接点选；" +
					"确实无法穷举时把最可能的几个列出来，用户仍可自由输入。",
				"items": map[string]any{
					"type":      "string",
					"maxLength": askUserMaxOptionLen,
				},
			},
		},
		"required": []string{"question", "options"},
	}
}

// askUserBadOptions 把「参数不合法」折成 is_error 结果而不是 Go error：
// 循环会把 is_error 回灌给模型，让它看到「必须给 2-4 个选项」后自己改对再问一次。
func askUserBadOptions(content string) (agent.Result, error) {
	return agent.Result{IsError: true, Content: content}, nil
}

func (AskUserTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var a struct {
		Question string   `json:"question"`
		Header   string   `json:"header"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return askUserBadOptions(fmt.Sprintf(
			"ask_user 必须给 2-4 个选项，每个选项是一个字符串（每个不超过 %d 个字）。参数解析失败：%v",
			askUserMaxOptionLen, err))
	}
	question := strings.TrimSpace(a.Question)
	if question == "" {
		return agent.Result{}, fmt.Errorf("question 不能为空")
	}
	opts, bad := validateAskOptions(a.Options)
	if bad != "" {
		return askUserBadOptions(bad)
	}

	var b strings.Builder
	b.WriteString(question)
	b.WriteString("\n\n可选：\n")
	for i, o := range opts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, o)
	}
	text := strings.TrimSpace(b.String())

	display := map[string]any{"question": question, "options": opts}
	if h := strings.TrimSpace(a.Header); h != "" {
		display["header"] = h
	}
	return agent.Result{
		Content:   "已向用户提问，等待回答。",
		AwaitUser: text,
		Display:   display,
	}, nil
}

// validateAskOptions 校验并归一化候选项：
// 数量必须 2-4 个，每一项去空白后不能为空、不超过 40 个字。
// 返回的 bad 非空时是一条给模型看的错误说明，调用方应把它作为 is_error 结果。
func validateAskOptions(raw []string) (opts []string, bad string) {
	if len(raw) < askUserMinOptions || len(raw) > askUserMaxOptions {
		return nil, fmt.Sprintf(
			"ask_user 必须给 %d-%d 个选项（每个不超过 %d 个字），当前给了 %d 个。",
			askUserMinOptions, askUserMaxOptions, askUserMaxOptionLen, len(raw))
	}
	opts = make([]string, 0, len(raw))
	for i, s := range raw {
		v := strings.TrimSpace(s)
		if v == "" {
			return nil, fmt.Sprintf(
				"ask_user 必须给 %d-%d 个选项（每个不超过 %d 个字），第 %d 个选项是空的。",
				askUserMinOptions, askUserMaxOptions, askUserMaxOptionLen, i+1)
		}
		if n := len([]rune(v)); n > askUserMaxOptionLen {
			return nil, fmt.Sprintf(
				"ask_user 必须给 %d-%d 个选项，每个不超过 %d 个字：第 %d 个选项有 %d 个字，请压缩成更短的具体选项。",
				askUserMinOptions, askUserMaxOptions, askUserMaxOptionLen, i+1, n)
		}
		opts = append(opts, v)
	}
	return opts, ""
}
