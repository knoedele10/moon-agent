package web

import "testing"

// 审计官 2026-10-21 定稿的唯一 P1：eino 执行器（默认）下
// AGENT_TOOL_PARALLEL=read 静默失效，而状态接口把配置值当生效值回显。
//
// 本用例把「配置值」与「生效值」的区分锁死：两处都要对，且必须能看出不一致。
func TestParallelEffectiveMatchesEngine(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		parallel string
		wantConf string
		wantEff  string
	}{
		{name: "默认 eino 下配 read", engine: "eino", parallel: "read", wantConf: "read", wantEff: "off"},
		{name: "legacy 下配 read", engine: "legacy", parallel: "read", wantConf: "read", wantEff: "read"},
		{name: "eino 下不配", engine: "eino", parallel: "", wantConf: "off", wantEff: "off"},
		{name: "legacy 下不配", engine: "legacy", parallel: "", wantConf: "off", wantEff: "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermesClearEnv(t)
			t.Setenv("AGENT_ENGINE", tc.engine)
			if tc.parallel != "" {
				t.Setenv("AGENT_TOOL_PARALLEL", tc.parallel)
			}
			if got := parallelMode(); got != tc.wantConf {
				t.Fatalf("parallelMode() = %q，期望 %q", got, tc.wantConf)
			}
			if got := parallelEffective(); got != tc.wantEff {
				t.Fatalf("parallelEffective() = %q，期望 %q", got, tc.wantEff)
			}
		})
	}
}
