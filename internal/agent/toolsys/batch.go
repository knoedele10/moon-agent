package toolsys

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"blog-my/internal/agent"
	"blog-my/internal/llm"
)

// defaultMaxParallel 是 RunBatch 的默认并发上限（契约 §3.3）。
const defaultMaxParallel = 4

// Plan 把一次模型返回的调用序列切成批次：连续只用只读工具的调用并成一批。
//
// 红线：写/执行/网络/改变自身等非只读工具**永不**与其它调用同批（各自单独一批）；
// riskOf 查不到名字的调用也按「非只读」处理，宁可串行也不要赌副作用可并行。
// 返回的批次是 calls 的下标切片，调用方按批次顺序执行即可保持输入顺序。
func Plan(calls []llm.ToolCall, riskOf func(name string) (Risk, bool)) [][]int {
	if len(calls) == 0 {
		return nil
	}
	var batches [][]int
	for index, call := range calls {
		readOnly := false
		if riskOf != nil {
			if risk, ok := riskOf(call.Name); ok && risk == RiskRead {
				readOnly = true
			}
		}
		if !readOnly {
			batches = append(batches, []int{index})
			continue
		}
		if len(batches) > 0 && isReadBatch(batches[len(batches)-1], calls, riskOf) {
			batches[len(batches)-1] = append(batches[len(batches)-1], index)
			continue
		}
		batches = append(batches, []int{index})
	}
	return batches
}

// isReadBatch 判断最后一个批次是不是「纯只读批」，用于决定能否继续并入。
//
// 批次只可能由 Plan 生成，所以这里只需要看首元素；额外再校验一次全部元素，
// 防止调用方手工构造批次后误用。未知风险一律视为非只读。
func isReadBatch(batch []int, calls []llm.ToolCall, riskOf func(string) (Risk, bool)) bool {
	if len(batch) == 0 || riskOf == nil {
		return false
	}
	for _, index := range batch {
		if index < 0 || index >= len(calls) {
			return false
		}
		risk, ok := riskOf(calls[index].Name)
		if !ok || risk != RiskRead {
			return false
		}
	}
	return true
}

// RunBatch 并发执行同一批调用，按输入顺序返回结果。
//
// 并发上限默认 4；ctx 取消后不再启动新调用，未执行的调用返回 TOOL_CANCELLED，
// 已经在跑的调用会把取消透传给工具（由工具自己尽快退出）。
// 返回值与 calls 一一对应——上层只要按序发事件，事件顺序就仍是输入顺序。
func RunBatch(ctx context.Context, ex *agent.Exec, reg *agent.Registry, calls []llm.ToolCall, maxParallel int) []agent.Result {
	if len(calls) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallel
	}
	if maxParallel > len(calls) {
		maxParallel = len(calls)
	}
	results := make([]agent.Result, len(calls))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for index := range calls {
		if ctx.Err() != nil {
			results[index] = canceledResult(calls[index])
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			results[index] = canceledResult(calls[index])
			continue
		}
		if ctx.Err() != nil {
			<-sem
			results[index] = canceledResult(calls[index])
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[index] = executeCall(ctx, ex, reg, calls[index])
		}(index)
	}
	wg.Wait()
	return results
}

// canceledResult 是取消后未执行调用的结果（仍要回灌，保持 tool_call 对应关系）。
func canceledResult(call llm.ToolCall) agent.Result {
	name := call.Name
	return agent.Result{
		IsError: true,
		Content: fmt.Sprintf("工具 %q 未执行：本次运行已取消。请不要假设它已经生效。", name),
		Display: map[string]any{"code": "TOOL_CANCELLED", "tool": name},
	}
}

// executeCall 查表并执行一次调用，把一切失败折成 is_error 结果（与主循环语义一致）。
func executeCall(ctx context.Context, ex *agent.Exec, reg *agent.Registry, call llm.ToolCall) (res agent.Result) {
	defer func() {
		if recovered := recover(); recovered != nil {
			res = agent.Result{
				IsError: true,
				Content: fmt.Sprintf("工具 %q 内部错误：%v", call.Name, recovered),
				Display: map[string]any{"code": "TOOL_PANIC", "tool": call.Name},
			}
		}
	}()

	tool, ok := reg.Lookup(call.Name)
	if !ok {
		return agent.Result{
			IsError: true,
			Content: fmt.Sprintf("未知工具 %q：当前可用工具是 %s。请改用可用工具，或直接给出最终答复。",
				call.Name, registryNames(reg)),
			Display: map[string]any{"code": "TOOL_UNKNOWN", "tool": call.Name},
		}
	}
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		// 有些后端对无参调用会发空字符串而不是 "{}"，兜一下。
		raw = "{}"
	}
	if !json.Valid([]byte(raw)) {
		return agent.Result{
			IsError: true,
			Content: fmt.Sprintf("工具 %q 的参数不是合法 JSON：%s", call.Name, clipText(raw, 200)),
			Display: map[string]any{"code": "TOOL_BAD_ARGS", "tool": call.Name},
		}
	}
	res, err := tool.Execute(ctx, json.RawMessage(raw), ex)
	if err != nil {
		res.IsError = true
		res.Content = err.Error()
	}
	if res.IsError && strings.TrimSpace(res.Content) == "" {
		res.Content = fmt.Sprintf("工具 %q 失败，但没有给出说明", call.Name)
	}
	return res
}

// registryNames 返回排序后的已登记工具名，用于「未知工具」报错里给出可用清单。
func registryNames(reg *agent.Registry) string {
	if reg == nil || reg.Len() == 0 {
		return "（当前没有可用工具）"
	}
	names := reg.Names()
	sort.Strings(names)
	return strings.Join(names, ", ")
}
