package agent

import (
	"context"
	"fmt"

	"blog-my/internal/llm"
)

// runToolState belongs to one run and survives model rounds and recovery windows.
// Both orchestration engines execute tools sequentially. Keep safety decisions
// here so switching engines cannot change retries or completion obligations.
type runToolState struct {
	uncertain       []uncertainEffect
	halted          bool
	step            int
	lastMutationSeq int
	lastVerifySeq   int
}

// execute applies execution safety and records successful mutations/verification.
// A budget rejection still gets a result, preserving tool-call/result pairing.
// Engine-specific budgets, events, images and user-input handling stay outside.
func (s *runToolState) execute(ctx context.Context, reg *Registry, call llm.ToolCall, ex *Exec, budgetErr error) Result {
	fp, se := toolFingerprint(call.Name, call.Arguments), toolSideEffect(call.Name)
	var res Result
	switch {
	case budgetErr != nil:
		res = Result{IsError: true, Content: budgetErr.Error()}
	case s.halted && se == SideEffectNonIdempotent:
		res = Result{IsError: true,
			Content: "前一个操作的结果未知，为避免重复副作用，这次调用没有执行。请先让用户确认上一步是否生效。",
			Display: map[string]any{"code": "NOT_EXECUTED_AFTER_UNCERTAIN", "tool": call.Name}}
	case findUncertain(s.uncertain, fp) != nil:
		res = Result{IsError: true,
			Content: fmt.Sprintf("这次调用与之前一次「结果未知」的操作完全相同（%s），已拒绝重复执行。请先确认上一步是否生效，不要重试。", call.Name),
			Display: map[string]any{"code": "UNCERTAIN_EFFECT_BLOCKED", "tool": call.Name}}
	default:
		res = executeTool(ctx, reg, call, ex)
		if res.IsError {
			cat := classifyToolError(res.Content)
			if decideRetry(cat, se) {
				// One retry only; non-idempotent calls are never replayed.
				res = executeTool(ctx, reg, call, ex)
				if res.IsError {
					cat = classifyToolError(res.Content)
				}
			}
			if res.IsError && isUnknownOutcome(cat, se) {
				s.uncertain = append(s.uncertain, uncertainEffect{
					ToolCallID: call.ID, Fingerprint: fp, ToolName: call.Name,
					Message: clipRunes(res.Content, 160),
				})
				s.halted = true
			}
		}
	}
	s.step++
	if !res.IsError {
		if mutatingTools[call.Name] {
			s.lastMutationSeq = s.step
		}
		if verifyingTools[call.Name] {
			s.lastVerifySeq = s.step
		}
	}
	return res
}
