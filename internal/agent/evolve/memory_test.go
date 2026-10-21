package evolve

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryAddDedupeAndRemove 覆盖契约 §7.8 的去重与删除。
func TestMemoryAddDedupeAndRemove(t *testing.T) {
	mem := &MemoryFile{Path: filepath.Join(t.TempDir(), "memory.json")}

	created, err := mem.Add("用户偏好", "用户喜欢简洁的回答。")
	if err != nil || !created {
		t.Fatalf("首次写入应成功且 created=true：created=%v err=%v", created, err)
	}
	// 规范化后相同（大小写/空白/句末标点不同）必须视为重复。
	created, err = mem.Add("用户偏好", "  用户喜欢简洁的回答 ")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("规范化后相同的记忆不应重复写入")
	}
	created, err = mem.Add("", "user prefers tabs")
	if err != nil || !created {
		t.Fatalf("英文记忆首次写入失败: %v", err)
	}
	created, err = mem.Add("", "  USER prefers   tabs  ")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("大小写/空白不同的同一条记忆不应重复写入")
	}

	all, err := mem.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("记忆条数 = %d，期望 2（去重生效）", len(all))
	}
	if all[0].Topic != "用户偏好" {
		t.Fatalf("未指定主题时应保留原主题，实际 %q", all[0].Topic)
	}
	if all[1].Topic != "通用" {
		t.Fatalf("未指定主题的条目应归入「通用」，实际 %q", all[1].Topic)
	}

	// 按 ID 删除。
	removed, err := mem.Remove(all[1].ID)
	if err != nil || !removed {
		t.Fatalf("按 ID 删除失败：removed=%v err=%v", removed, err)
	}
	removed, err = mem.Remove(all[1].ID)
	if err != nil || removed {
		t.Fatalf("重复删除应返回 false：removed=%v err=%v", removed, err)
	}
	if got, _ := mem.All(); len(got) != 1 {
		t.Fatalf("删除后条数 = %d，期望 1", len(got))
	}
}

// TestMemorySearchAndBlockTrim 覆盖契约 §7.8 的检索与 Block 裁剪。
func TestMemorySearchAndBlockTrim(t *testing.T) {
	mem := &MemoryFile{Path: filepath.Join(t.TempDir(), "memory.json"), MaxRunes: 80}
	texts := []string{
		"回答要简洁，不要长篇大论。",
		"构建 Windows 安装包前先跑单元测试。",
		"用户的时区是 Asia/Shanghai。",
		"提交前必须执行 gofmt。",
		"数据库迁移只能由 Lead 批准。",
	}
	for i, text := range texts {
		if _, err := mem.Add("开发约定", text); err != nil {
			t.Fatalf("写入第 %d 条记忆失败: %v", i, err)
		}
	}
	hits := mem.Search("gofmt", 3)
	if len(hits) != 1 || !strings.Contains(hits[0].Text, "gofmt") {
		t.Fatalf("按关键词检索结果不符合预期: %+v", hits)
	}
	if hits[0].Hits != 1 {
		t.Fatalf("命中次数 = %d，期望 1", hits[0].Hits)
	}
	// 命中次数应持久化。
	again := mem.Search("gofmt", 3)
	if len(again) != 1 || again[0].Hits != 2 {
		t.Fatalf("命中次数未持久化: %+v", again)
	}

	block := mem.Block()
	if !strings.HasPrefix(block, "<memory>") || !strings.HasSuffix(block, "</memory>\n") {
		t.Fatalf("Block 格式不对: %q", block)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(block, "<memory>\n"), "\n</memory>\n")
	if runes := len([]rune(body)); runes > 80 {
		t.Fatalf("Block 正文 %d 字，超过 MaxRunes=80", runes)
	}
	if !strings.Contains(body, "数据库迁移") {
		t.Fatalf("裁剪应保留最新记忆（数据库迁移），实际:\n%s", body)
	}
}

// TestMemoryBlockEmpty 验证没有记忆时不注入空块。
func TestMemoryBlockEmpty(t *testing.T) {
	mem := &MemoryFile{Path: filepath.Join(t.TempDir(), "memory.json")}
	if got := mem.Block(); got != "" {
		t.Fatalf("空记忆的 Block 应为空串，实际 %q", got)
	}
}
