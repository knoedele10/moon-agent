package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Item 是一条已解析好的技能，Source 保留它来自哪个文件（便于溯源与重复导入）。
type Item struct {
	Name         string
	Description  string
	Body         string
	Source       string
	Version      string
	Origin       string
	AllowedTools []string
}

// ScanOptions 控制目录扫描。默认值（零值）就是“给技能库用”的那套：
// 跳过隐藏目录与 node_modules，同名只留层级最浅的一份。
type ScanOptions struct {
	// IncludeHidden 为 true 时进入以 . 开头的目录。
	// 默认 false：技能库里 .gbrain / .openclaw / .hermes 这类目录是工具自己
	// 同步出来的副本，扫进去只会用旧副本覆盖同名技能。
	IncludeHidden bool
	// MaxBodyRunes 大于 0 时截断正文（防止单个超大技能把数据库撑爆）。0 = 不截断。
	MaxBodyRunes int
}

// ScanReport 是一次扫描的结果。Files 是找到的 SKILL.md 总数（含被跳过的），
// 便于调用方解释“579 个文件为什么只导入 110 条”。
type ScanReport struct {
	Root          string
	Files         int
	Items         []Item
	SkippedHidden int
	Duplicates    []string
	Errors        []string
}

// ScanDir 递归找出 root 下的所有 SKILL.md 并解析。
//
// 去重规则：同名技能只保留**路径层级最浅**的那份（并列时取字典序小的），
// 被丢掉的名字记在 Duplicates 里。技能库里同名副本很常见（gstack 下每个
// 宿主目录都同步一份），全导进去只会让“最后一个赢”变成随机结果。
func ScanDir(root string, opt ScanOptions) (ScanReport, error) {
	rep := ScanReport{Root: root}
	abs, err := filepath.Abs(root)
	if err != nil {
		return rep, fmt.Errorf("解析路径失败: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return rep, fmt.Errorf("目录不存在: %w", err)
	}
	if !fi.IsDir() {
		return rep, fmt.Errorf("%s 不是目录", abs)
	}
	rep.Root = abs

	type cand struct {
		item  Item
		depth int
	}
	var cands []cand

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == abs {
				return nil
			}
			name := d.Name()
			if name == "node_modules" {
				return fs.SkipDir
			}
			if !opt.IncludeHidden && strings.HasPrefix(name, ".") {
				rep.SkippedHidden++
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), "SKILL.md") {
			return nil
		}
		rep.Files++

		raw, err := os.ReadFile(path)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		name, desc, body := ParseMarkdown(string(raw))
		meta := ParseMeta(string(raw))
		if strings.TrimSpace(name) == "" {
			// frontmatter 里没有 name 时用目录名兜底：技能名是模型找技能的钥匙，
			// 不能让它变成空串。
			name = filepath.Base(filepath.Dir(path))
		}
		if opt.MaxBodyRunes > 0 && len([]rune(body)) > opt.MaxBodyRunes {
			body = string([]rune(body)[:opt.MaxBodyRunes])
		}
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil {
			rel = path
		}
		cands = append(cands, cand{
			item: Item{
				Name: name, Description: desc, Body: body, Source: path,
				Version: meta.Version, Origin: meta.Source, AllowedTools: meta.AllowedTools,
			},
			depth: strings.Count(rel, string(filepath.Separator)),
		})
		return nil
	})
	if walkErr != nil {
		return rep, fmt.Errorf("扫描失败: %w", walkErr)
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].depth != cands[j].depth {
			return cands[i].depth < cands[j].depth
		}
		return cands[i].item.Source < cands[j].item.Source
	})
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		key := strings.ToLower(strings.TrimSpace(c.item.Name))
		if seen[key] {
			rep.Duplicates = append(rep.Duplicates, c.item.Name+" <- "+c.item.Source)
			continue
		}
		seen[key] = true
		rep.Items = append(rep.Items, c.item)
	}
	return rep, nil
}
