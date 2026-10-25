package agent

import (
	"context"
	"strings"

	"blog-my/internal/agent/hooks"
	"blog-my/internal/agent/rules"
	"blog-my/internal/llm"
)

func noteJournal(ex *Exec, kind string, payload any) {
	if ex == nil || ex.Journal == nil {
		return
	}
	ex.Journal(kind, payload)
}

func beforeTool(ctx context.Context, ex *Exec, call llm.ToolCall) (Result, bool) {
	noteJournal(ex, "tool_started", map[string]any{
		"id": call.ID, "tool": call.Name, "effect": string(toolSideEffect(call.Name)),
	})
	root := ""
	if ex != nil {
		root = ex.Root
	}
	if decision := hooks.PreToolUse(ctx, root, call.Name, call.Arguments); !decision.Allow {
		res := Result{IsError: true, Content: decision.Reason, Display: map[string]any{"code": "HOOK_DENIED", "tool": call.Name}}
		noteJournal(ex, "tool_finished", map[string]any{
			"id": call.ID, "tool": call.Name, "ok": false, "effect": string(toolSideEffect(call.Name)),
		})
		return res, false
	}
	return Result{}, true
}

func afterTool(ctx context.Context, ex *Exec, call llm.ToolCall, res Result) Result {
	root, turn := "", ""
	if ex != nil {
		root, turn = ex.Root, ex.TurnID
	}
	if note := rules.MaybeVerify(ctx, root, turn, call.Name, res.IsError); note != "" {
		res.Content = strings.TrimRight(res.Content, "\n") + "\n\n" + note
	}
	hooks.PostToolUse(ctx, root, call.Name, res.IsError, res.Content)
	finished := map[string]any{
		"id": call.ID, "tool": call.Name, "ok": !res.IsError, "effect": string(toolSideEffect(call.Name)),
	}
	if path := artifactPath(res); path != "" {
		finished["path"] = path
	}
	noteJournal(ex, "tool_finished", finished)
	return res
}

func artifactPath(res Result) string {
	if res.Display == nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "to"} {
		if s, ok := res.Display[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func finishRun(ctx context.Context, ex *Exec) {
	root := ""
	if ex != nil {
		root = ex.Root
	}
	hooks.Stop(ctx, root)
}
