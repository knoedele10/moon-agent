package evolve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// memStore 是内存版技能库：只实现 SkillStore 三个方法，用来验证固化器的
// 「建议 vs 落库」「幂等」「限量」三条语义，不碰数据库与文件。
type memStore struct {
	mu      sync.Mutex
	items   map[string]SkillItem // name → item（忽略 userID：单测只有一个用户）
	upserts int
}

func newMemStore(items ...SkillItem) *memStore {
	m := &memStore{items: make(map[string]SkillItem, len(items))}
	for _, it := range items {
		m.items[it.Name] = it
	}
	return m
}

func (m *memStore) List(string) ([]SkillItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SkillItem, 0, len(m.items))
	for _, it := range m.items {
		out = append(out, it)
	}
	return out, nil
}

func (m *memStore) Get(_ string, name string) (SkillItem, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[name]
	return it, ok, nil
}

func (m *memStore) Upsert(_ string, it SkillItem) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, existed := m.items[it.Name]
	m.items[it.Name] = it
	m.upserts++
	return !existed, nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upserts
}

func (m *memStore) enabled(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[name].Enabled
}

// 两条同名不同名但正文相同的技能：Jaccard 相似度 1.0，必然判重复。
func duplicateSkills() []SkillItem {
	body := "步骤：先读配置，再改字段，最后跑测试确认。"
	return []SkillItem{
		{Name: "alpha", Description: "把配置字段改掉并确认测试通过的流程", Body: body, Enabled: true},
		{Name: "beta", Description: "把配置字段改掉并确认测试通过的流程", Body: body, Enabled: true},
	}
}

// 只建议不落库：suggest 档跑完报告里有重复，但技能库一个字节都不变。
func TestCuratorSuggestDoesNotMutate(t *testing.T) {
	st := newMemStore(duplicateSkills()...)
	dir := t.TempDir()
	c := &Curator{Store: st, Recorder: NewRecorder(dir), Apply: false}
	rep, err := c.Run(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if len(rep.Duplicates) == 0 {
		t.Fatal("应报告重复技能")
	}
	if len(rep.Applied) != 0 {
		t.Fatalf("suggest 档不该改库：%v", rep.Applied)
	}
	if got := st.count(); got != 0 {
		t.Fatalf("suggest 档调用了 %d 次 Upsert", got)
	}
	for _, it := range duplicateSkills() {
		if !st.enabled(it.Name) {
			t.Fatalf("%q 被意外归档", it.Name)
		}
	}
	// 报告要落盘，供界面和 /api/agent/evolution 读取。
	last, found, err := c.LastReport(dir)
	if err != nil || !found {
		t.Fatalf("报告没有落盘：found=%v err=%v", found, err)
	}
	if len(last.Duplicates) != len(rep.Duplicates) {
		t.Fatalf("落盘报告与返回值不一致：%v vs %v", last.Duplicates, rep.Duplicates)
	}
}

// auto 档真的归档重复项，且重复运行幂等（第二次什么都不改）。
func TestCuratorApplyArchivesOnceAndIsIdempotent(t *testing.T) {
	st := newMemStore(duplicateSkills()...)
	dir := t.TempDir()
	c := &Curator{Store: st, Recorder: NewRecorder(dir), Apply: true}
	rep, err := c.Run(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if len(rep.Applied) != 1 || !strings.HasPrefix(rep.Applied[0], "archive:") {
		t.Fatalf("应归档 1 条：%v", rep.Applied)
	}
	archived := strings.TrimPrefix(rep.Applied[0], "archive:")
	if st.enabled(archived) {
		t.Fatalf("%q 应被归档（Enabled=false）", archived)
	}
	before := st.count()

	rep2, err := c.Run(context.Background(), "u1")
	if err != nil {
		t.Fatalf("第二次 Run 失败：%v", err)
	}
	if len(rep2.Applied) != 0 {
		t.Fatalf("重复运行不该再改库：%v", rep2.Applied)
	}
	if after := st.count(); after != before {
		t.Fatalf("幂等失败：第二次仍调用了 %d 次 Upsert", after-before)
	}
}

// 单次固化最多改 MaxMutates 条：没改完的下次再来，绝不一次刷爆技能库。
func TestCuratorApplyRespectsMaxMutates(t *testing.T) {
	items := make([]SkillItem, 0, 8)
	for _, name := range []string{"a1", "a2", "b1", "b2", "c1", "c2", "d1", "d2"} {
		items = append(items, SkillItem{
			Name: name, Description: "同一套流程的不同副本用来验证固化限量", Body: "同正文", Enabled: true,
		})
	}
	st := newMemStore(items...)
	c := &Curator{Store: st, Recorder: NewRecorder(t.TempDir()), Apply: true, MaxMutates: 2}
	rep, err := c.Run(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if len(rep.Applied) > 2 {
		t.Fatalf("MaxMutates=2 却改了 %d 条：%v", len(rep.Applied), rep.Applied)
	}
	if len(rep.Applied) == 0 {
		t.Fatal("有重复就应至少归档一条")
	}
}

// 候选自身体检不过（含危险命令）就不落库：进化不能把危险技能写进用户技能库。
func TestCuratorApplySkipsDangerousCandidate(t *testing.T) {
	st := newMemStore()
	c := &Curator{Store: st, Apply: true}
	dangerous := Candidate{
		Kind:        "skill",
		Name:        "cleanup",
		Description: "一次清理磁盘根目录的流程，危险动作不要照抄",
		Body:        "运行 rm -rf / 清理磁盘",
	}
	applied, err := c.apply("u1", nil, nil, []Candidate{dangerous})
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("危险候选不该落库：%v", applied)
	}
	if got := st.count(); got != 0 {
		t.Fatalf("危险候选调用了 %d 次 Upsert", got)
	}
}

// 归档后即使候选正文变了也不复活：归档是人的决定，自动化不能推翻。
func TestCuratorApplyDoesNotReviveArchivedSkill(t *testing.T) {
	st := newMemStore(SkillItem{Name: "old", Description: "早已归档的技能不应该被自动复活", Body: "旧正文", Enabled: false})
	c := &Curator{Store: st, Apply: true}
	cand := Candidate{Kind: "skill", Name: "old", Description: "早已归档的技能不应该被自动复活", Body: "新正文"}
	applied, err := c.apply("u1", []SkillItem{{Name: "old", Enabled: false}}, nil, []Candidate{cand})
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("归档技能被复活：%v", applied)
	}
	if st.enabled("old") {
		t.Fatal("归档技能被改回启用")
	}
}

// 报告文件必须是原子写：读到的永远是完整 JSON（不能半截）。
func TestCuratorReportIsCompleteJSON(t *testing.T) {
	dir := t.TempDir()
	c := &Curator{Store: newMemStore(duplicateSkills()...), Recorder: NewRecorder(dir), Apply: false}
	if _, err := c.Run(context.Background(), "u1"); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, reportFileName))
	if err != nil {
		t.Fatalf("读报告失败：%v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(raw)), "}") {
		t.Fatalf("报告不是完整 JSON：%q", raw)
	}
}
