package agent

import (
	"context"
	"sync"

	"blog-my/internal/llm"
)

// 并行执行：只读调用并发跑，写/执行/网络工具仍严格串行。
//
// 设计取舍（对标 hermes 的 tool_call_batches）：
//   - 不做「全量并行」：同一轮里写与读混在一起时，读的并发必须让位于写的顺序语义，
//     否则模型看到的结果与真实副作用顺序不一致，重试与回滚都会出错。
//   - 只并发**连续**的只读调用：一旦中间夹了写类调用就切开，保证「读-写-读」的
//     因果顺序不被优化掉。
//   - 并发度默认 4（与 hermes 一致），由 Config.ParallelMax 覆盖；0 表示用默认值。
//   - 事件与消息仍按**输入顺序**回放：并发只影响执行，不影响呈现与历史。
//
// 白名单来自 web 层（AGENT_TOOL_PARALLEL=read 时的 readOnlyToolNames）：
// agent 包不认识具体工具，只按名字判断，避免把「看起来像读」当成「真的只读」。

// DefaultParallelMax 是只读并发批的默认并发度。
const DefaultParallelMax = 4

// parallelEligible 报告这次调用能否与同批只读调用并发。
func parallelEligible(cfg Config, name string) bool {
	if !cfg.Parallel || len(cfg.ParallelTools) == 0 {
		return false
	}
	return cfg.ParallelTools[name]
}

// planCallGroups 把一轮调用切成执行组：连续且都允许并发的调用并成一组，
// 其余每个调用各自成组（长度 1 = 走原串行路径，语义一字不改）。
func planCallGroups(cfg Config, calls []llm.ToolCall) [][]int {
	if len(calls) == 0 {
		return nil
	}
	groups := make([][]int, 0, len(calls))
	cur := make([]int, 0, 4)
	for i, call := range calls {
		if parallelEligible(cfg, call.Name) {
			cur = append(cur, i)
			continue
		}
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = make([]int, 0, 4)
		}
		groups = append(groups, []int{i})
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// parallelMax 返回本次运行的并发上限（缺省 DefaultParallelMax）。
func parallelMax(cfg Config) int {
	if cfg.ParallelMax > 0 {
		return cfg.ParallelMax
	}
	return DefaultParallelMax
}

// runReadOnlyBatch 并发执行一批只读调用，按输入顺序返回结果。
//
// 失败语义与串行一致：每个调用都折成 Result，绝不因为一个工具 panic 或报错而中断整批
// （executeTool 内部已 recover 并把错误折成 is_error）。
// ctx 取消后不再启动新调用：已在执行的等它自己结束（工具自己响应 ctx）。
func runReadOnlyBatch(ctx context.Context, source ToolSource, ex *Exec, calls []llm.ToolCall, maxParallel int) []Result {
	out := make([]Result, len(calls))
	if len(calls) == 0 {
		return out
	}
	if maxParallel <= 0 {
		maxParallel = DefaultParallelMax
	}
	if maxParallel > len(calls) {
		maxParallel = len(calls)
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i := range calls {
		select {
		case <-ctx.Done():
			out[i] = Result{IsError: true, Content: ctx.Err().Error()}
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			// 拿到令牌后可能已经取消：这里再查一次，避免「取消之后又启动了一个工具」。
			// （select 只在拿令牌那刻判断，goroutine 真正开始跑可能已经晚了一步。）
			if err := ctx.Err(); err != nil {
				out[i] = Result{IsError: true, Content: err.Error()}
				return
			}
			out[i] = executeTool(ctx, source, calls[i], ex)
			// 只读调用的重试与串行路径保持一致（decideRetry 对只读只放行
			// transient/timeout/rate_limited/unknown）：读失败重试一次最坏只是白跑，
			// 少这一次重试会让并发档比串行档更容易把偶发错误交给模型。
			if out[i].IsError && ctx.Err() == nil {
				if cat := classifyToolError(out[i].Content); decideRetry(cat, toolSideEffect(calls[i].Name)) {
					out[i] = executeTool(ctx, source, calls[i], ex)
				}
			}
		}(i)
	}
	wg.Wait()
	return out
}
