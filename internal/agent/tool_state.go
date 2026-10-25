package agent

import (
	"context"
	"fmt"
	"sync"

	"blog-my/internal/llm"
)

// runToolState belongs to one run and survives model rounds and recovery windows.
// Both orchestration engines share these safety decisions so switching engines
// cannot change retries or completion obligations.
//
// 并发约定：Eino 执行器在 AGENT_TOOL_PARALLEL=read 时会让只读工具并行跑，
// 因此状态必须自己加锁。锁只包住「判定」与「记账」，**不包住工具执行**：
// 否则并行只读又会退化成串行。
type runToolState struct {
	mu              sync.Mutex
	uncertain       []uncertainEffect
	halted          bool
	step            int
	lastMutationSeq int
	lastVerifySeq   int
}

// execute applies execution safety and records successful mutations/verification.
// A budget rejection still gets a result, preserving tool-call/result pairing.
// Engine-specific budgets, events, images and user-input handling stay outside.
func (s *runToolState) execute(ctx context.Context, source ToolSource, call llm.ToolCall, ex *Exec, budgetErr error) Result {
	se := toolSideEffect(call.Name)
	pre, proceed := s.begin(call, se, budgetErr)
	if !proceed {
		s.finish(call, se, pre, false)
		return pre
	}
	res := executeToolWithRetry(ctx, source, call, ex, se)
	s.finish(call, se, res, true)
	return res
}

// begin 在真正执行前做预算/不确定副作用检查；proceed=false 时 pre 就是最终结果。
func (s *runToolState) begin(call llm.ToolCall, se SideEffectKind, budgetErr error) (pre Result, proceed bool) {
	fp := toolFingerprint(call.Name, call.Arguments)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case budgetErr != nil:
		return Result{IsError: true, Content: budgetErr.Error()}, false
	case s.halted && se == SideEffectNonIdempotent:
		return Result{IsError: true,
			Content: "前一个操作的结果未知，为避免重复副作用，这次调用没有执行。请先让用户确认上一步是否生效。",
			Display: map[string]any{"code": "NOT_EXECUTED_AFTER_UNCERTAIN", "tool": call.Name}}, false
	case findUncertain(s.uncertain, fp) != nil:
		return Result{IsError: true,
			Content: fmt.Sprintf("这次调用与之前一次「结果未知」的操作完全相同（%s），已拒绝重复执行。请先确认上一步是否生效，不要重试。", call.Name),
			Display: map[string]any{"code": "UNCERTAIN_EFFECT_BLOCKED", "tool": call.Name}}, false
	}
	return Result{}, true
}

// finish 记录一次执行结果；executed=false 表示这次调用根本没执行（预算/保护拦截）。
func (s *runToolState) finish(call llm.ToolCall, se SideEffectKind, res Result, executed bool) {
	fp := toolFingerprint(call.Name, call.Arguments)
	s.mu.Lock()
	defer s.mu.Unlock()
	if executed && res.IsError {
		cat := classifyToolError(res.Content)
		if isUnknownOutcome(cat, se) {
			s.uncertain = append(s.uncertain, uncertainEffect{
				ToolCallID: call.ID, Fingerprint: fp, ToolName: call.Name,
				Message: clipRunes(res.Content, 160),
			})
			s.halted = true
		}
	}
	s.recordStepLocked(call.Name, res)
}

// executeToolWithRetry 是「执行 + 一次有限重试」的共享实现。
// 非幂等调用永不重放；只读/幂等调用在可重试错误上重试一次。
func executeToolWithRetry(ctx context.Context, source ToolSource, call llm.ToolCall, ex *Exec, se SideEffectKind) Result {
	res := executeTool(ctx, source, call, ex)
	if !res.IsError {
		return res
	}
	cat := classifyToolError(res.Content)
	if decideRetry(cat, se) {
		res = executeTool(ctx, source, call, ex)
	}
	return res
}

// recordStep 是**唯一**的记账入口：推进步数，并在成功时记录「改动」与「验证」。
//
// 并行批与串行都走这里，否则「写完之后同一轮并发读回两个文件」不会被记成已验证，
// 收尾时会误判「改动未验证」并要求模型再解释一轮（真实发生过，见 parallel_test.go）。
// 记账只看工具名，不看是否在批里：批里的只读工具本来就在白名单内，不会有写工具。
func (s *runToolState) recordStep(name string, res Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordStepLocked(name, res)
}

// recordStepLocked 是 recordStep 的无锁版本（调用方已持 s.mu）。
func (s *runToolState) recordStepLocked(name string, res Result) {
	s.step++
	if res.IsError {
		return
	}
	if toolMutates(name) {
		s.lastMutationSeq = s.step
	}
	if toolVerifies(name) {
		s.lastVerifySeq = s.step
	}
}
