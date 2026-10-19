package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// 准备一个“技能目录 + 工作区”，技能目录里放一个会打印并写文件的脚本。
func skillRunEnv(t *testing.T) (skillsDir, workspace string) {
	t.Helper()
	skillsDir = t.TempDir()
	workspace = t.TempDir()

	script := `import os, sys
print("hello from skill", sys.argv[1] if len(sys.argv) > 1 else "")
with open("out.txt", "w", encoding="utf-8") as f:
    f.write("written by skill")
`
	if err := os.WriteFile(filepath.Join(skillsDir, "hello.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skillsDir, "ppt-master", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "ppt-master", "scripts", "gen.py"), []byte(`print("gen ok")`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SKILLS_DIR", skillsDir)
	return skillsDir, workspace
}

func runSkill(t *testing.T, workspace string, args map[string]any) (agent.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return skillRunTool{userID: "u1"}.Execute(context.Background(), raw, &agent.Exec{SessionID: "s", Root: workspace})
}

// 正向：脚本能跑起来，输出能拿到，且它在“工作目录”里写的文件真的落在工作区。
func TestSkillRunExecutesScriptInWorkspace(t *testing.T) {
	_, workspace := skillRunEnv(t)

	res, err := runSkill(t, workspace, map[string]any{"script": "hello.py", "args": []string{"arg1"}})
	if err != nil {
		t.Fatalf("脚本应当执行成功: %v", err)
	}
	if !strings.Contains(res.Content, "hello from skill arg1") {
		t.Fatalf("应当返回脚本输出: %q", res.Content)
	}

	// 工作目录 = 工作区：脚本写的相对路径必须落在工作区里，而不是技能目录。
	if _, serr := os.Stat(filepath.Join(workspace, "out.txt")); serr != nil {
		t.Fatalf("脚本产物应当落在工作区: %v", serr)
	}
	// 而且不该写进技能目录（技能目录对模型是只读的，脚本也不该往里写）。
	if _, serr := os.Stat(filepath.Join(agentSkillsDir(), "out.txt")); serr == nil {
		t.Fatal("脚本产物不该落在技能目录里")
	}
}

func TestSkillRunNestedScriptPath(t *testing.T) {
	_, workspace := skillRunEnv(t)
	res, err := runSkill(t, workspace, map[string]any{"script": "ppt-master/scripts/gen.py"})
	if err != nil {
		t.Fatalf("子目录脚本应当能跑: %v", err)
	}
	if !strings.Contains(res.Content, "gen ok") {
		t.Fatalf("输出不对: %q", res.Content)
	}
}

// 负向：越界、非 .py、不存在，都必须被挡在“启动进程”之前。
func TestSkillRunRejectsBadTargets(t *testing.T) {
	_, workspace := skillRunEnv(t)

	bad := []map[string]any{
		{"script": "../outside.py"},                        // 目录穿越
		{"script": "..\\outside.py"},                       // Windows 风格穿越
		{"script": filepath.Join("C:", "Windows", "x.py")}, // 绝对路径
		{"script": "hello.txt"},                            // 非 .py
		{"script": ""},                                     // 空
		{"script": "not-exist.py"},                         // 不存在
	}
	for _, args := range bad {
		if _, err := runSkill(t, workspace, args); err == nil {
			t.Fatalf("%v 应当被拒绝", args)
		}
	}
}

// 技能目录没配置时，明确报“未配置”，而不是静默失败或去猜路径。
func TestSkillRunWithoutSkillsDir(t *testing.T) {
	t.Setenv("AGENT_SKILLS_DIR", "")
	if _, err := runSkill(t, t.TempDir(), map[string]any{"script": "hello.py"}); err == nil ||
		!strings.Contains(err.Error(), "技能目录未配置") {
		t.Fatalf("应当明确报未配置: %v", err)
	}
}

// 环境变量清洗：机密不该顺手交给第三方技能脚本。
func TestMinimalEnvHidesSecrets(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-should-not-leak")
	env := minimalEnv("/tmp/skills", &agent.Exec{Root: "/tmp/ws"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "DEEPSEEK_API_KEY=") {
			t.Fatal("机密环境变量不该传给技能脚本")
		}
	}
	joined := strings.Join(env, ";")
	for _, want := range []string{"SKILL_DIR=/tmp/skills", "AGENT_WORKSPACE=/tmp/ws", "PYTHONIOENCODING=utf-8"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应当带上 %q，实际 %v", want, env)
		}
	}
}
