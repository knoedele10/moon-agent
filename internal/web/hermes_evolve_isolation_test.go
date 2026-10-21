package web

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"blog-my/internal/agent/evolve"
)

type isolatedEvolveStore struct {
	mu    sync.Mutex
	items map[string]evolve.SkillItem
}

func (s *isolatedEvolveStore) List(string) ([]evolve.SkillItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []evolve.SkillItem
	for _, item := range s.items {
		rows = append(rows, item)
	}
	return rows, nil
}
func (s *isolatedEvolveStore) Get(_ string, name string) (evolve.SkillItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found := s.items[name]
	return item, found, nil
}
func (s *isolatedEvolveStore) Upsert(_ string, item evolve.SkillItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.items[item.Name]
	s.items[item.Name] = item
	return !found, nil
}

func TestHermesEvolutionUserFilesAreIsolated(t *testing.T) {
	t.Setenv("AGENT_EVOLVE", "suggest")
	t.Setenv("AGENT_EVOLVE_DIR", t.TempDir())
	a, b := newHermesEvolve("../user-a"), newHermesEvolve("user-b")
	if a.dir == b.dir || filepath.Dir(a.dir) != filepath.Join(hermesDataDir(), "users") {
		t.Fatalf("用户目录未隔离：%s %s", a.dir, b.dir)
	}
	if _, err := a.memory.Add("偏好", "只属于 A 的私有工作习惯"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.PromptBlock(), "只属于 A") {
		t.Fatal("用户 B 读取到 A 的记忆")
	}
	a.RecordSkillUse("private-skill", true)
	if stats, err := b.ledger.Stats(); err != nil || len(stats) != 0 {
		t.Fatalf("B 看到 A 的账本：%+v %v", stats, err)
	}
	a.store = &isolatedEvolveStore{items: make(map[string]evolve.SkillItem)}
	if _, err := a.Curate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, exists := b.LastReport(); exists {
		t.Fatal("B 看到 A 的固化报告")
	}
}

func TestHermesEvolutionSkillViewsDoNotClaimSuccess(t *testing.T) {
	t.Setenv("AGENT_EVOLVE", "record")
	t.Setenv("AGENT_EVOLVE_DIR", t.TempDir())
	e := newHermesEvolve("feedback-user")
	e.BeginRun(evolve.RunMeta{RunID: "feedback"})
	e.ObserveToolCall("read", "skill_read", `{"name":"viewed-only"}`)
	e.ObserveToolResult("read", "skill_read", true, false, "ok")
	e.RecordSelectedSkills([]string{"unfinished"}, "await_user")
	stats, _ := e.ledger.Stats()
	if len(stats) != 0 {
		t.Fatalf("查看/等待输入不能认定任务成功失败：%+v", stats)
	}
	e.ObserveToolCall("run", "skill_run", `{"script":"runner/scripts/check.py"}`)
	e.ObserveToolResult("run", "skill_run", false, true, "failed")
	e.RecordSelectedSkills([]string{"runner", "selected", "selected"}, "ok")
	stats, err := e.ledger.Stats()
	if err != nil || len(stats) != 2 {
		t.Fatalf("反馈统计不正确：%+v %v", stats, err)
	}
	for _, item := range stats {
		if item.Uses != 1 {
			t.Fatalf("同一次运行重复记账：%+v", item)
		}
		if item.Name == "runner" && item.Fail != 1 {
			t.Fatalf("脚本失败被任务成功掩盖：%+v", item)
		}
	}
}

func TestHermesEvolutionAutoActuallyCurates(t *testing.T) {
	t.Setenv("AGENT_EVOLVE", "auto")
	t.Setenv("AGENT_EVOLVE_DIR", t.TempDir())
	e := newHermesEvolve("auto-user")
	e.store = &isolatedEvolveStore{items: make(map[string]evolve.SkillItem)}
	for _, id := range []string{"first", "second"} {
		run := e.recorder.Start(evolve.RunMeta{RunID: id, UserID: e.userID})
		run.Tool(evolve.ToolCallRecord{Name: "read_file", Args: `{"path":"config.json"}`, OK: true})
		run.Tool(evolve.ToolCallRecord{Name: "search_files", Args: `{"query":"ready"}`, OK: true})
		if err := run.Finish("ok", ""); err != nil {
			t.Fatal(err)
		}
	}
	e.CurateAsync(true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if report, exists := e.LastReport(); exists && len(report.Applied) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("auto 模式没有在后台固化已验证技能")
}

func TestHermesEvolutionAutoRequiresTurnPermission(t *testing.T) {
	t.Setenv("AGENT_EVOLVE", "auto")
	t.Setenv("AGENT_EVOLVE_DIR", t.TempDir())
	e := newHermesEvolve("restricted-user")
	e.store = &isolatedEvolveStore{items: make(map[string]evolve.SkillItem)}
	e.CurateAsync()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if report, exists := e.LastReport(); exists {
			if len(report.Applied) != 0 {
				t.Fatalf("无本轮权限却修改技能：%v", report.Applied)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("后台建议报告未生成")
}

func TestHermesEvolutionCancellationIsNotSkillFailure(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		outcome, _ := trajectoryOutcome(err, "", false)
		if outcome != "interrupted" {
			t.Fatalf("取消不能认定为任务失败：%s", outcome)
		}
	}
}
