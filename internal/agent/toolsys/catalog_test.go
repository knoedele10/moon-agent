package toolsys

import (
	"testing"

	"blog-my/internal/agent"
)

// TestCatalogActivateToolsetSelection 对应契约 §7.3：
// 给一个 3 toolset 的假目录，激活 fs,memory 后只暴露这两组。
func TestCatalogActivateToolsetSelection(t *testing.T) {
	c := NewCatalog(
		Toolset{Name: "fs", Desc: "文件", Risk: RiskRead, Tools: []agent.Tool{
			&fakeTool{name: "read", desc: "读取文件", risk: RiskRead},
			&fakeTool{name: "list", desc: "列出文件", risk: RiskRead},
		}},
		Toolset{Name: "memory", Desc: "记忆", Risk: RiskSelf, Tools: []agent.Tool{
			&fakeTool{name: "memory_note", desc: "写记忆", risk: RiskSelf},
		}},
		Toolset{Name: "net", Desc: "网络", Risk: RiskNetwork, Tools: []agent.Tool{
			&fakeTool{name: "web_search", desc: "联网搜索", risk: RiskNetwork},
		}},
	)
	if total, active := c.Len(); total != 4 || active != 0 {
		t.Fatalf("初始 Len=(%d,%d)，期望 (4,0)（没有 core 集时不自动激活）", total, active)
	}
	if defs := c.Defs(); len(defs) != 0 {
		t.Fatalf("初始 Defs 应为空，实际 %v", defNames(defs))
	}

	activated := c.ActivateToolset("fs", "memory")
	if want := []string{"read", "list", "memory_note"}; !equalStrings(activated, want) {
		t.Fatalf("ActivateToolset 返回 %v，期望 %v", activated, want)
	}
	if defs := c.Defs(); !equalStrings(defNames(defs), []string{"read", "list", "memory_note"}) {
		t.Fatalf("Defs=%v，期望只含 fs+memory 三件", defNames(defs))
	}
	if _, ok := c.Lookup("web_search"); !ok {
		t.Fatal("Lookup 应能查到未激活工具（是否允许调用由调用方决定）")
	}

	// 幂等：再激活一次不产生新名字，也不改变 Defs 顺序。
	if again := c.ActivateToolset("fs", "memory"); len(again) != 0 {
		t.Fatalf("重复激活应返回空，实际 %v", again)
	}
	if total, active := c.Len(); total != 4 || active != 3 {
		t.Fatalf("重复激活后 Len=(%d,%d)，期望 (4,3)", total, active)
	}

	// 给提示词/HTTP 用的工具集信息：fs/memory 整组已激活，net 没有。
	infos := c.Toolsets()
	if len(infos) != 3 {
		t.Fatalf("Toolsets 返回 %d 组，期望 3", len(infos))
	}
	byName := make(map[string]ToolsetInfo, len(infos))
	for _, info := range infos {
		byName[info.Name] = info
	}
	if !byName["fs"].Activated || byName["fs"].Active != 2 {
		t.Fatalf("fs 组信息不对：%+v", byName["fs"])
	}
	if byName["net"].Activated || byName["net"].Active != 0 {
		t.Fatalf("net 组不应被激活：%+v", byName["net"])
	}
}

// TestCatalogCoreAlwaysActive 核心集（名为 core）在构造后就已激活。
func TestCatalogCoreAlwaysActive(t *testing.T) {
	c := NewCatalog(
		Toolset{Name: "core", Desc: "核心", Tools: []agent.Tool{
			&fakeTool{name: "todo", desc: "待办", risk: RiskRead},
		}},
		Toolset{Name: "fs", Desc: "文件", Tools: []agent.Tool{
			&fakeTool{name: "read", desc: "读文件", risk: RiskRead},
		}},
	)
	if total, active := c.Len(); total != 2 || active != 1 {
		t.Fatalf("Len=(%d,%d)，期望 (2,1)", total, active)
	}
	if defs := c.Defs(); !equalStrings(defNames(defs), []string{"todo"}) {
		t.Fatalf("Defs=%v，期望只有 core 的 todo", defNames(defs))
	}
}

// TestCatalogActivateToolsetDependencies 激活工具集时按依赖先激活被依赖组。
func TestCatalogActivateToolsetDependencies(t *testing.T) {
	c := NewCatalog(
		Toolset{Name: "memory", Desc: "记忆", Tools: []agent.Tool{
			&fakeTool{name: "memory_note", risk: RiskSelf},
		}},
		Toolset{Name: "evolve", Desc: "进化", Depends: []string{"memory"}, Tools: []agent.Tool{
			&fakeTool{name: "evolution_report", risk: RiskSelf},
		}},
	)
	activated := c.ActivateToolset("evolve")
	if want := []string{"memory_note", "evolution_report"}; !equalStrings(activated, want) {
		t.Fatalf("依赖激活顺序=%v，期望 %v（先依赖后自身）", activated, want)
	}
	// 循环依赖不能死循环。
	cyclic := NewCatalog(
		Toolset{Name: "a", Depends: []string{"b"}, Tools: []agent.Tool{&fakeTool{name: "a1", risk: RiskRead}}},
		Toolset{Name: "b", Depends: []string{"a"}, Tools: []agent.Tool{&fakeTool{name: "b1", risk: RiskRead}}},
	)
	if got := cyclic.ActivateToolset("a"); len(got) != 2 {
		t.Fatalf("循环依赖应各激活一次，实际 %v", got)
	}
}

// TestCatalogActivateAll 全量激活用于「AGENT_TOOLSETS 为空 = 现状全量」的兼容路径。
func TestCatalogActivateAll(t *testing.T) {
	c := NewCatalog(
		Toolset{Name: "a", Desc: "A", Tools: []agent.Tool{&fakeTool{name: "a1"}, &fakeTool{name: "a2"}}},
		Toolset{Name: "b", Desc: "B", Tools: []agent.Tool{&fakeTool{name: "b1"}}},
	)
	if got := c.ActivateAll(); !equalStrings(got, []string{"a1", "a2", "b1"}) {
		t.Fatalf("ActivateAll=%v", got)
	}
	if defs := c.Defs(); len(defs) != 3 {
		t.Fatalf("全量激活后 Defs 应有 3 个，实际 %v", defNames(defs))
	}
	if again := c.ActivateAll(); len(again) != 0 {
		t.Fatalf("ActivateAll 应幂等，实际 %v", again)
	}
}

// TestCatalogRiskOverrideAndTags 单工具可以覆盖组风险，并贡献检索别名。
func TestCatalogRiskOverrideAndTags(t *testing.T) {
	risky := &fakeTool{name: "delete", risk: RiskWrite}
	tagged := &fakeTool{name: "read", risk: "", tags: []string{"查看", "打开"}}
	// 组默认故意写成 write：未标注的 read 应采用元数据里的 read，而不是整组默认。
	c := NewCatalog(Toolset{Name: "fs", Desc: "文件", Risk: RiskWrite, Tools: []agent.Tool{risky, tagged}})
	if risk, ok := c.RiskOf("delete"); !ok || risk != RiskWrite {
		t.Fatalf("delete 风险=%v, ok=%v，期望 write", risk, ok)
	}
	if risk, ok := c.RiskOf("read"); !ok || risk != RiskRead {
		t.Fatalf("read 未标注时应采用元数据 read，而不是组默认 write，实际 %v", risk)
	}
	if matches := c.Search("查看", 5); len(matches) != 1 || matches[0].Name != "read" {
		t.Fatalf("别名检索应命中 read，实际 %v", matchNames(matches))
	}
	if _, ok := c.RiskOf("missing"); ok {
		t.Fatal("未登记的工具 RiskOf 应返回 false")
	}
}

// TestCatalogRiskAnnotatedBeatsMeta 显式标注压过元数据；表外名字用保守风险，而不是组默认 read。
func TestCatalogRiskAnnotatedBeatsMeta(t *testing.T) {
	c := NewCatalog(Toolset{Name: "misc", Desc: "杂项", Risk: RiskRead, Tools: []agent.Tool{
		&fakeTool{name: "write"},                  // 元数据是 write
		&fakeTool{name: "delete", risk: RiskRead}, // 标注故意盖过元数据的 write
		&fakeTool{name: "custom_unlisted"},        // 表外
	}})
	if risk, ok := c.RiskOf("write"); !ok || risk != RiskWrite {
		t.Fatalf("未标注的 write 应取元数据 write，实际 %v ok=%v", risk, ok)
	}
	if risk, ok := c.RiskOf("delete"); !ok || risk != RiskRead {
		t.Fatalf("显式标注应盖过元数据，实际 %v ok=%v", risk, ok)
	}
	if risk, ok := c.RiskOf("custom_unlisted"); !ok || risk != RiskWrite {
		t.Fatalf("表外工具应取保守风险 write，实际 %v ok=%v", risk, ok)
	}
}

// TestCatalogSearchRanking 契约 §3 的检索档位：名字精确 > 前缀 > 别名 > 名字子串 > 描述子串。
func TestCatalogSearchRanking(t *testing.T) {
	c := NewCatalog(Toolset{Name: "t", Desc: "测试", Tools: []agent.Tool{
		&fakeTool{name: "read", desc: "精确"},
		&fakeTool{name: "read_file", desc: "前缀"},
		&fakeTool{name: "list", desc: "别名", tags: []string{"查看"}},
		&fakeTool{name: "file_reader", desc: "名字子串"},
		&fakeTool{name: "writer", desc: "写入文件内容"},
		&fakeTool{name: "calc", desc: "计算器"},
	}})

	cases := []struct {
		name  string
		query string
		limit int
		want  []string // 期望的前若干名（按顺序）
	}{
		{name: "名字精确优先", query: "read", limit: 2, want: []string{"read", "read_file"}},
		{name: "别名命中", query: "查看", limit: 1, want: []string{"list"}},
		{name: "描述子串", query: "写入", limit: 1, want: []string{"writer"}},
		{name: "limit 生效", query: "read", limit: 1, want: []string{"read"}},
		{name: "无关词无结果", query: "zzz", limit: 5, want: []string{}},
		{name: "空查询无结果", query: "   ", limit: 5, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchNames(c.Search(tc.query, tc.limit))
			if tc.limit > 0 && len(got) > tc.limit {
				t.Fatalf("返回 %d 条超过 limit %d", len(got), tc.limit)
			}
			if len(got) < len(tc.want) {
				t.Fatalf("query=%q 返回 %v，期望至少前 %v", tc.query, got, tc.want)
			}
			if !equalStrings(got[:len(tc.want)], tc.want) {
				t.Fatalf("query=%q 返回 %v，期望前 %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestCatalogSearchStableTie 同分必须按声明顺序稳定。
func TestCatalogSearchStableTie(t *testing.T) {
	c := NewCatalog(Toolset{Name: "t", Desc: "测试", Tools: []agent.Tool{
		&fakeTool{name: "alpha", desc: "同一个描述"},
		&fakeTool{name: "beta", desc: "同一个描述"},
		&fakeTool{name: "gamma", desc: "同一个描述"},
	}})
	first := matchNames(c.Search("同一个描述", 3))
	for i := 0; i < 5; i++ {
		if got := matchNames(c.Search("同一个描述", 3)); !equalStrings(got, first) {
			t.Fatalf("第 %d 次检索顺序变了：%v vs %v", i, got, first)
		}
	}
	if want := []string{"alpha", "beta", "gamma"}; !equalStrings(first, want) {
		t.Fatalf("同分顺序=%v，期望声明顺序 %v", first, want)
	}
}

// TestCatalogDuplicateToolKeepsFirst 同名工具先声明者优先，后者静默忽略。
func TestCatalogDuplicateToolKeepsFirst(t *testing.T) {
	first := &fakeTool{name: "read", desc: "第一份"}
	second := &fakeTool{name: "read", desc: "第二份"}
	c := NewCatalog(
		Toolset{Name: "a", Tools: []agent.Tool{first}},
		Toolset{Name: "b", Tools: []agent.Tool{second}},
	)
	if total, _ := c.Len(); total != 1 {
		t.Fatalf("同名工具应只登记一次，实际总数 %d", total)
	}
	tool, ok := c.Lookup("read")
	if !ok || tool.Description() != "第一份" {
		t.Fatalf("应按先声明者保留，实际 %q", tool.Description())
	}
	if second.callCount() != 0 {
		t.Fatal("重复工具不应被调用")
	}
}

// TestCatalogEmptyAndNil 空目录/空工具集不应 panic。
func TestCatalogEmptyAndNil(t *testing.T) {
	c := NewCatalog()
	if total, active := c.Len(); total != 0 || active != 0 {
		t.Fatalf("空目录 Len=(%d,%d)", total, active)
	}
	if defs := c.Defs(); len(defs) != 0 {
		t.Fatalf("空目录 Defs=%v", defs)
	}
	if matches := c.Search("anything", 5); len(matches) != 0 {
		t.Fatalf("空目录 Search=%v", matchNames(matches))
	}
	if _, ok := c.Lookup("x"); ok {
		t.Fatal("空目录 Lookup 应 false")
	}
	if infos := c.Toolsets(); len(infos) != 0 {
		t.Fatalf("空目录 Toolsets=%v", infos)
	}
	// nil 工具/空名字工具被跳过。
	c2 := NewCatalog(Toolset{Name: "x", Tools: []agent.Tool{nil, &fakeTool{name: "  "}, &fakeTool{name: "ok"}}})
	if total, _ := c2.Len(); total != 1 {
		t.Fatalf("nil/空名工具应被跳过，实际 %d", total)
	}
}

// TestCatalogDefsStableAcrossReads Defs 多次调用顺序必须稳定（提示词缓存）。
func TestCatalogDefsStableAcrossReads(t *testing.T) {
	c := NewCatalog(
		Toolset{Name: "a", Tools: []agent.Tool{&fakeTool{name: "a1"}, &fakeTool{name: "a2"}}},
		Toolset{Name: "b", Tools: []agent.Tool{&fakeTool{name: "b1"}}},
	)
	c.ActivateToolset("b", "a")
	want := defNames(c.Defs())
	for i := 0; i < 10; i++ {
		if got := defNames(c.Defs()); !equalStrings(got, want) {
			t.Fatalf("第 %d 次 Defs 顺序变了：%v vs %v", i, got, want)
		}
	}
	if !equalStrings(want, []string{"b1", "a1", "a2"}) {
		t.Fatalf("Defs 顺序=%v，期望先激活顺序 b1 再声明顺序 a1,a2", want)
	}
}
