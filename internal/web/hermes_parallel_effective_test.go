package web

import "testing"

// 配置值与生效值必须一致地回显；两个执行器都应支持只读并发。
func TestParallelEffectiveMatchesEngine(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		parallel string
		wantConf string
		wantEff  string
	}{
		{name: "默认 eino 下配 read", engine: "eino", parallel: "read", wantConf: "read", wantEff: "read"},
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
