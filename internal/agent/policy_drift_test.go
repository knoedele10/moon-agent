package agent

import (
	"testing"

	"blog-my/internal/agent/toolsys/policy"
)

// 安全层必须是元数据表的薄封装。agent 不能 import toolsys（会成环），
// 所以这里对照 policy；toolsys.LookupMeta 与 policy.LookupMeta 的一致性在 toolsys 测试里。
func TestSafetyAgreesWithToolMeta(t *testing.T) {
	if string(SideEffectReadOnly) != string(policy.SideEffectReadOnly) ||
		string(SideEffectIdempotent) != string(policy.SideEffectIdempotent) ||
		string(SideEffectNonIdempotent) != string(policy.SideEffectNonIdempotent) {
		t.Fatal("副作用枚举字符串与 policy 不一致")
	}

	for _, name := range policy.KnownToolNames() {
		meta := policy.LookupMeta(name)
		if string(toolSideEffect(name)) != string(meta.SideEffect) {
			t.Errorf("%s 副作用：safety=%s meta=%s", name, toolSideEffect(name), meta.SideEffect)
		}
		if toolMutates(name) != meta.Mutating {
			t.Errorf("%s mutating：safety=%v meta=%v", name, toolMutates(name), meta.Mutating)
		}
		if toolVerifies(name) != meta.Verifying {
			t.Errorf("%s verifying：safety=%v meta=%v", name, toolVerifies(name), meta.Verifying)
		}
	}
}

func TestSafetyUnknownIsConservative(t *testing.T) {
	cases := []string{"not_a_registered_tool", "shell", "python_repl", "send_email", "随便什么新工具", "Read", " read "}
	for _, name := range cases {
		meta := policy.LookupMeta(name)
		if meta.Known || meta.ParallelOK || !meta.Confirm || meta.SideEffect != policy.SideEffectNonIdempotent {
			t.Fatalf("%q 的元数据不是保守默认：%+v", name, meta)
		}
		if toolSideEffect(name) != SideEffectNonIdempotent {
			t.Errorf("%q 必须按非幂等处理，实际 %s", name, toolSideEffect(name))
		}
		if toolMutates(name) || toolVerifies(name) {
			t.Errorf("%q 未知工具不能算改动或验证", name)
		}
	}
	if toolSideEffect("mcp__demo__echo") != SideEffectNonIdempotent || toolMutates("mcp__demo__echo") {
		t.Fatal("mcp__ 应是非幂等且不触发完成义务")
	}
}
