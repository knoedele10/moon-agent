package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"blog-my/internal/agent/toolsys/policy"
)

// 工具失败的安全层：错误分类 → 副作用语义 → 重试决策，以及"结果未知的副作用"拦截。
//
// 设计参考 Cyrene-Agent 的 CyreneHarness（MIT）。它的两条原则值得原样搬过来：
//
//  1. **副作用不是工具的静态属性，而是 (工具, 参数) 的属性。**
//     同一个 run_shell，"ls" 是只读、"rm -rf" 不是。
//
//  2. **失败安全默认（fail-safe default）：认不出来的一律按"有副作用"处理。**
//     它的原话是"宁愿少 retry 一次，也不要错把非幂等副作用当作可重试"。
//     不要试图做"智能命令语义分析器"——shell 里 `python script.py` 干了什么完全是黑盒。

// ErrorCategory 是工具失败的大类。
//
// 关键是**把 unknown 和 transient / fatal 分开**：
//   - transient 是"明确知道是临时的"（网络抖动、锁、503），值得重试；
//   - unknown 是"不知道是什么"（SDK 行为变化、协议解析异常、工具实现 bug），
//     重试 3 次没意义，所以**只对只读工具考虑重试 1 次**；
//   - fatal 是"Runtime 已无法保证继续安全"，不重试。
type ErrorCategory string

const (
	CatNotFound     ErrorCategory = "not_found"
	CatPermission   ErrorCategory = "permission_denied"
	CatInvalidArgs  ErrorCategory = "invalid_arguments"
	CatTimeout      ErrorCategory = "timeout"
	CatRateLimited  ErrorCategory = "rate_limited"
	CatTransient    ErrorCategory = "transient"
	CatUnknown      ErrorCategory = "unknown"
	CatSemanticFail ErrorCategory = "semantic_failure"
)

// classifyToolError 从工具返回的**文本**里判断错误类别。
//
// 为什么按文本判而不是 typed error：本项目的工具统一把失败折成
// `Result{IsError:true, Content:"..."}`（契约 §3.5），没有 error 类型可依赖。
// 所以这里做关键词匹配——**判不出来就是 unknown**，这正是我们要的保守默认。
func classifyToolError(content string) ErrorCategory {
	s := strings.ToLower(content)
	switch {
	case containsAny(s, "不存在", "not found", "no such file", "未找到", "没有名为"):
		return CatNotFound
	case containsAny(s, "permission denied", "拒绝访问", "权限不足", "eacces", "eperm", "被拒绝"):
		return CatPermission
	case containsAny(s, "invalid arguments", "参数", "必填", "不能为空", "不是合法 json", "invalid_argument"):
		return CatInvalidArgs
	case containsAny(s, "timeout", "超时", "timed out", "deadline exceeded"):
		return CatTimeout
	case containsAny(s, "429", "rate limit", "限流", "too many requests"):
		return CatRateLimited
	case containsAny(s, "connection reset", "connection refused", "econnreset", "econnrefused", "503", "502", "temporarily", "ebusy", "eagain", "网络", "连接"):
		return CatTransient
	}
	// 判不出来 → unknown。这是刻意的：不要硬塞进 transient。
	return CatUnknown
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// SideEffectKind 是 (工具, 参数) 的副作用语义。
type SideEffectKind string

const (
	SideEffectReadOnly      SideEffectKind = "read_only"
	SideEffectIdempotent    SideEffectKind = "idempotent_mutation"
	SideEffectNonIdempotent SideEffectKind = "non_idempotent_side_effect"
)

// toolSideEffect 判断这次调用的副作用语义。
//
// 分类在 toolsys/policy（agent 不能 import toolsys，否则成环）。
// **认不出来的工具一律按 non_idempotent_side_effect 处理**——这是失败安全默认：
// 将来新加一个工具（尤其是 shell / python / 外部服务连接器）忘了登记，
// 宁可它"被少重试一次"，也不能让它被自动重放。
func toolSideEffect(name string) SideEffectKind {
	switch policy.LookupMeta(name).SideEffect {
	case policy.SideEffectReadOnly:
		return SideEffectReadOnly
	case policy.SideEffectIdempotent:
		return SideEffectIdempotent
	default:
		return SideEffectNonIdempotent
	}
}

// toolMutates 报告这次调用算不算「会改变世界」、做完值得验一下。
func toolMutates(name string) bool {
	return policy.LookupMeta(name).Mutating
}

// toolVerifies 报告这次调用能不能充当「写完之后的验证」。
func toolVerifies(name string) bool {
	return policy.LookupMeta(name).Verifying
}

// decideRetry 是重试决策矩阵：类别 × 副作用 → 是否重试。
//
// 与 Cyrene 的表一致：
//
//	read_only              : transient/timeout/rate_limited → 重试；unknown → 最多 1 次；其余不重试
//	idempotent_mutation    : transient/timeout/rate_limited → 重试；其余不重试
//	non_idempotent         : **任何类别都不自动重试**（重发邮件 / 重删文件都是错的）
func decideRetry(cat ErrorCategory, se SideEffectKind) bool {
	switch se {
	case SideEffectReadOnly:
		switch cat {
		case CatTransient, CatTimeout, CatRateLimited:
			return true
		case CatUnknown:
			return true // 只读，重试一次最坏也只是白跑
		}
		return false
	case SideEffectIdempotent:
		switch cat {
		case CatTransient, CatTimeout, CatRateLimited:
			return true
		}
		return false
	default: // non_idempotent（含所有未登记的工具）
		return false
	}
}

// isUnknownOutcome 判断一次失败是否属于"**结果未知**"——
// 即副作用可能已经生效、也可能没有，Runtime 无法确认。
//
// 判据（保守）：非幂等工具 + 超时/临时性错误。这两类最典型的场景是
// "请求发出去了但没等到回执"——删文件删没删掉、代码跑没跑完，都不知道。
func isUnknownOutcome(cat ErrorCategory, se SideEffectKind) bool {
	if se != SideEffectNonIdempotent {
		return false
	}
	return cat == CatTimeout || cat == CatTransient
}

// uncertainEffect 是一条"结果未知的副作用"记录。
type uncertainEffect struct {
	ToolCallID  string
	Fingerprint string
	ToolName    string
	Message     string
}

// toolFingerprint 用「工具名 + 参数」算指纹，用来识别"是不是同一次调用"。
//
// 同一轮里模型重发一次一模一样的调用（尤其是被它自己改写过措辞的重发）时，
// 指纹相同 → 拒绝执行，避免重复副作用。
func toolFingerprint(name, args string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + strings.TrimSpace(args)))
	return hex.EncodeToString(sum[:16])
}

// findUncertain 在已有记录里找同指纹的项；没有返回 nil。
func findUncertain(list []uncertainEffect, fp string) *uncertainEffect {
	for i := range list {
		if list[i].Fingerprint == fp {
			return &list[i]
		}
	}
	return nil
}

// uncertainFeedback 在模型想结束、但还有未确认副作用时，生成一条**运行时反馈**。
//
// 为什么要拦：如果不拦，模型下一轮很容易"假装这件事已经完成了"，
// 或者自己再调一次同一个工具（重复删 / 重复发送）。
// 这条反馈要求它**如实告诉用户"这一步结果未知"**，而不是宣称成功。
func uncertainFeedback(list []uncertainEffect) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【运行时提醒】以下操作已经发起，但**执行结果无法确认**（超时或连接中断）。\n")
	b.WriteString("你不能把它们当作已经完成，也不要再重复执行同一个操作（可能产生第二次副作用）。\n")
	for _, e := range list {
		b.WriteString("- " + e.ToolName + "：" + e.Message + "\n")
	}
	b.WriteString("请在回复里**如实告诉用户**这几步的结果未知、需要他自己确认，然后再说明其余部分。")
	return b.String()
}

// ---------------------------------------------------------------------------
// 完成义务（防「伪完成」）
// ---------------------------------------------------------------------------
//
// 设计参考 Cyrene 的 completionObligations：模型想收尾时，runtime 检查有没有
// 未完成的义务，没完成就把它拦回去。它的原话是「不要假装完成」。
//
// 简化版：只看「写完之后有没有验证过」（mutating / verifying 来自同一张元数据表）。
// 这是最常见的伪完成场景——模型改了文件、然后直接说「改好了」。
// 只读工具里只有 read / list / read_image / sql_query 算验证——搜索、计算、时间都不算。

// needsVerification 判断「改完之后还没验」：最后一次写操作比最后一次验证更晚。
// 用序号而不是时间：同一次 Run 里序号就是模型决策的顺序，最准确。
func needsVerification(lastMutationSeq, lastVerifySeq int) bool {
	return lastMutationSeq > 0 && lastMutationSeq > lastVerifySeq
}

// verificationFeedback 是拦下来时注入的运行时提醒（role=system，不伪装 user）。
// 要求很具体：要么去验一下，要么**明说没验**——不能含糊地说「已完成」。
func verificationFeedback() string {
	return "【运行时提醒】你刚刚改动了文件，但之后没有读回来确认过。\n" +
		"请二选一：① 先调用 read（或 list / sql_query）确认改动真的生效，再给结论；" +
		"② 如果你确实不打算验证，就在回复里明说「这一步没有验证」，不要让用户以为你已经确认过。\n" +
		"不要在没有依据的情况下说「已经改好了 / 确认没问题」——那是伪完成。"
}
