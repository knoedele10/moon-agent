// Package trigger 观察工作区文件变化。它只出快照，不启动 agent。
package trigger

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry 是一个普通文件的路径、大小和修改时间。
type Entry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModUnix int64  `json:"mod_unix"`
}

// Snapshot 遍历 root 下的普通文件。跳过 .git 与 node_modules。limit <= 0 时最多 2000 个。
func Snapshot(root string, limit int) ([]Entry, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, os.ErrInvalid
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var out []Entry
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= limit {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return nil
		}
		out = append(out, Entry{Path: filepath.ToSlash(rel), Size: info.Size(), ModUnix: info.ModTime().Unix()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// Changed 对比两份快照。before 为 nil 表示还没有基线，返回 nil，避免第一次就把所有文件当成变化。
func Changed(before, after []Entry) []string {
	if before == nil {
		return nil
	}
	prev := map[string]Entry{}
	for _, e := range before {
		prev[e.Path] = e
	}
	seen := map[string]bool{}
	var changed []string
	for _, e := range after {
		old, ok := prev[e.Path]
		seen[e.Path] = true
		if !ok || old.Size != e.Size || old.ModUnix != e.ModUnix {
			changed = append(changed, e.Path)
		}
	}
	for _, e := range before {
		if !seen[e.Path] {
			changed = append(changed, e.Path)
		}
	}
	sort.Strings(changed)
	return changed
}
