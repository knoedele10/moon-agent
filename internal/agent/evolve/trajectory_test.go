package evolve

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore 是内存版 SkillStore，测试里代替数据库。
type fakeStore struct {
	mu      sync.Mutex
	items   map[string]SkillItem
	upserts int
}

func newFakeStore(items ...SkillItem) *fakeStore {
	s := &fakeStore{items: make(map[string]SkillItem, len(items))}
	for _, it := range items {
		s.items[it.Name] = it
	}
	return s
}

func (s *fakeStore) List(string) ([]SkillItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkillItem, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeStore) Get(_ string, name string) (SkillItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[name]
	return it, ok, nil
}

func (s *fakeStore) Upsert(_ string, it SkillItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.items[it.Name]
	s.items[it.Name] = it
	s.upserts++
	return !exists, nil
}

func (s *fakeStore) upsertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upserts
}

// validSkill 构造一条结构合法、内容安全的技能。
func validSkill(name string) SkillItem {
	return SkillItem{
		Name:        name,
		Description: "把 Windows 安装包从源码构建出来，覆盖签名与校验步骤。",
		Body:        "## 何时使用\n\n需要打 Windows 安装包时。\n\n## 怎么做\n\n1. 运行 build 脚本。\n2. 校验产物哈希。\n",
		Source:      "test",
		Enabled:     true,
	}
}

func fixedRecorder(t *testing.T, dir string) (*Recorder, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	rec := NewRecorder(dir)
	rec.Now = func() time.Time { return now }
	return rec, now
}

// writeRun 写一条固定形状的运行轨迹（默认 r→w→r→w 全成功）。
func writeRun(t *testing.T, rec *Recorder, id string, at time.Time, calls []ToolCallRecord, outcome, note string) {
	t.Helper()
	run := rec.Start(RunMeta{RunID: id, UserID: "u1", SessionID: "s1", StartedAt: at})
	for _, c := range calls {
		run.Tool(c)
	}
	if err := run.Finish(outcome, note); err != nil {
		t.Fatalf("写入轨迹失败: %v", err)
	}
}

// TestCuratorApplyAndIdempotent 覆盖契约 §7.7：
// Apply=false 不动库；Apply=true 最多改 MaxMutates 条；重复运行幂等。
func TestCuratorApplyAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	rec, now := fixedRecorder(t, dir)

	repeat := []ToolCallRecord{
		{Name: "fs_read", Args: `{"path":"config.json"}`, OK: true}, {Name: "fs_write", Args: `{"path":"result.json","content":"ok"}`, OK: true},
		{Name: "fs_read", Args: `{"path":"config.json"}`, OK: true}, {Name: "fs_write", Args: `{"path":"result.json","content":"ok"}`, OK: true},
	}
	for i := 0; i < 3; i++ {
		writeRun(t, rec, fmt.Sprintf("run-%d", i), now, repeat, "ok", "")
	}
	ledger := &Ledger{Path: filepath.Join(dir, "ledger.jsonl")}
	for i := 0; i < 5; i++ {
		if err := ledger.Record(SkillUse{Name: "skill-a", OK: true, At: now}); err != nil {
			t.Fatalf("写账本失败: %v", err)
		}
	}
	if err := ledger.Record(SkillUse{Name: "skill-b", OK: false, At: now}); err != nil {
		t.Fatalf("写账本失败: %v", err)
	}

	// skill-a 与 skill-b 正文完全相同 → 应被识别为重复，保留使用更多的 a。
	a, b := validSkill("skill-a"), validSkill("skill-b")
	b.Name = "skill-b"
	store := newFakeStore(a, b)
	cur := &Curator{Store: store, Ledger: ledger, Recorder: rec, Apply: false}

	rep, err := cur.Run(t.Context(), "u1")
	if err != nil {
		t.Fatalf("suggest 模式运行失败: %v", err)
	}
	if rep.Runs != 3 {
		t.Fatalf("回看轨迹数 = %d，期望 3", rep.Runs)
	}
	if rep.Skills != 2 {
		t.Fatalf("技能数 = %d，期望 2", rep.Skills)
	}
	if len(rep.Duplicates) != 1 || rep.Duplicates[0] != [2]string{"skill-a", "skill-b"} {
		t.Fatalf("重复技能 = %v，期望 [[skill-a skill-b]]", rep.Duplicates)
	}
	if len(rep.Candidates) == 0 {
		t.Fatal("期望至少一条候选")
	}
	if len(rep.Applied) != 0 || store.upsertCount() != 0 {
		t.Fatalf("suggest 模式不应改库：applied=%v upserts=%d", rep.Applied, store.upsertCount())
	}
	if _, ok, err := cur.LastReport(dir); err != nil || !ok {
		t.Fatalf("suggest 模式应落报告：ok=%v err=%v", ok, err)
	}

	// auto 模式：最多改 MaxMutates 条。
	cur.Apply = true
	cur.MaxMutates = 2
	rep, err = cur.Run(t.Context(), "u1")
	if err != nil {
		t.Fatalf("auto 模式运行失败: %v", err)
	}
	if len(rep.Applied) != 2 {
		t.Fatalf("auto 模式改动数 = %d（%v），期望 2", len(rep.Applied), rep.Applied)
	}
	if store.upsertCount() != 2 {
		t.Fatalf("auto 模式 upsert 次数 = %d，期望 2", store.upsertCount())
	}
	if got, _, _ := store.Get("u1", "skill-b"); got.Enabled {
		t.Fatal("skill-b 应被归档（使用更少的重复项）")
	}
	if got, _, _ := store.Get("u1", "skill-a"); !got.Enabled {
		t.Fatal("skill-a 应保持启用")
	}

	// 幂等：再跑一次不应产生任何新改动。
	rep, err = cur.Run(t.Context(), "u1")
	if err != nil {
		t.Fatalf("第二次 auto 运行失败: %v", err)
	}
	if len(rep.Applied) != 0 {
		t.Fatalf("重复运行应幂等，实际改动: %v", rep.Applied)
	}
	if store.upsertCount() != 2 {
		t.Fatalf("重复运行不应再 upsert，实际 %d 次", store.upsertCount())
	}
}

// TestCuratorMaxMutates 验证 MaxMutates 上限与默认值。
func TestCuratorMaxMutates(t *testing.T) {
	dir := t.TempDir()
	rec, now := fixedRecorder(t, dir)
	for i := 0; i < 3; i++ {
		writeRun(t, rec, fmt.Sprintf("run-%d", i), now, []ToolCallRecord{
			{Name: "fs_read", Args: `{"path":"config.json"}`, OK: true}, {Name: "fs_write", Args: `{"path":"result.json","content":"ok"}`, OK: true},
			{Name: "fs_read", Args: `{"path":"config.json"}`, OK: true}, {Name: "fs_write", Args: `{"path":"result.json","content":"ok"}`, OK: true},
		}, "ok", "")
	}
	store := newFakeStore()
	cur := &Curator{Store: store, Recorder: rec, Apply: true, MaxMutates: 1}
	rep, err := cur.Run(t.Context(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Applied) != 1 {
		t.Fatalf("MaxMutates=1 时改动数 = %d，期望 1", len(rep.Applied))
	}
}

// TestRecorderOffProducesNoFiles 覆盖契约 §7.10：关闭档不得产生任何轨迹文件。
func TestRecorderOffProducesNoFiles(t *testing.T) {
	// 用一个还不存在的子目录：关闭档连目录都不该创建。
	dir := filepath.Join(t.TempDir(), "evolve-off")
	if rec := RecorderFor(LevelOff, dir); rec != nil {
		t.Fatalf("LevelOff 应返回 nil Recorder，实际 %v", rec)
	}
	rec := RecorderFor(LevelOff, dir)
	run := rec.Start(RunMeta{RunID: "off"}) // nil 接收者：必须是安全空操作
	run.Tool(ToolCallRecord{Name: "fs_read", OK: true})
	run.SetTotals(1, 10)
	if err := run.Finish("ok", ""); err != nil {
		t.Fatalf("关闭档 Finish 不应报错: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("关闭档不应创建目录 %s（stat err=%v）", dir, err)
	}
}

// TestTrajectoryListTruncateAndCompress 覆盖轨迹的读回、截断、归档。
func TestTrajectoryListTruncateAndCompress(t *testing.T) {
	dir := t.TempDir()
	rec, now := fixedRecorder(t, dir)

	writeRun(t, rec, "recent", now, []ToolCallRecord{{Name: "fs_read", OK: true}}, "ok", "")
	old := now.AddDate(0, 0, -10)
	writeRun(t, rec, "old", old, []ToolCallRecord{{Name: "fs_write", OK: true}}, "ok", "")

	// 超长 args/summary 必须被截断到 32KB 以内并打标。
	writeRun(t, rec, "huge", now, []ToolCallRecord{
		{Name: "big", Args: strings.Repeat("a", 40000), Summary: strings.Repeat("b", 40000), OK: true},
	}, "ok", "")

	all, err := rec.List(0)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List 条数 = %d，期望 3", len(all))
	}
	found := false
	for _, r := range all {
		if r.RunID != "huge" {
			continue
		}
		found = true
		if len(r.ToolCalls) != 1 {
			t.Fatalf("huge 轨迹调用数 = %d，期望 1", len(r.ToolCalls))
		}
		if !strings.Contains(r.ToolCalls[0].Args, truncatedMark) {
			t.Fatalf("超长 args 应带截断标记: %.40q", r.ToolCalls[0].Args)
		}
	}
	if !found {
		t.Fatal("没读到 huge 轨迹")
	}
	// 每一行都必须 ≤32KB。
	files, err := os.ReadDir(filepath.Join(dir, dirTrajectories))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, dirTrajectories, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if len(line) > maxJSONLineBytes {
				t.Fatalf("%s 单行 %d 字节，超过上限 %d", f.Name(), len(line), maxJSONLineBytes)
			}
		}
	}

	recent, err := rec.List(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("最近 3 天轨迹数 = %d，期望 2", len(recent))
	}

	rep, err := rec.CompressBefore(now.AddDate(0, 0, -5))
	if err != nil {
		t.Fatalf("CompressBefore 失败: %v", err)
	}
	if rep.Files != 1 || rep.Runs != 1 {
		t.Fatalf("压缩结果 files=%d runs=%d，期望 1/1", rep.Files, rep.Runs)
	}
	archived, err := os.ReadDir(filepath.Join(dir, dirArchive))
	if err != nil {
		t.Fatalf("归档目录不存在: %v", err)
	}
	if len(archived) != 2 { // 月份 jsonl + summary.json
		t.Fatalf("归档目录文件数 = %d，期望 2", len(archived))
	}
	if _, err := os.Stat(filepath.Join(dir, dirTrajectories, old.Format("2006-01-02")+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("旧轨迹文件应被删除（stat err=%v）", err)
	}
}

// TestRecorderConcurrentFinish 验证并发写同一文件不串行、不丢行。
func TestRecorderConcurrentFinish(t *testing.T) {
	dir := t.TempDir()
	rec, now := fixedRecorder(t, dir)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run := rec.Start(RunMeta{RunID: fmt.Sprintf("run-%02d", i), StartedAt: now})
			run.Tool(ToolCallRecord{Name: "fs_read", OK: true})
			if err := run.Finish("ok", ""); err != nil {
				t.Errorf("并发 Finish 失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	recs, err := rec.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("并发写后读到 %d 条，期望 %d 条", len(recs), n)
	}
}
