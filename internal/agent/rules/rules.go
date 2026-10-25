// Package rules loads repository-local AGENTS.md instructions for an agent run.
//
// Rules are user-authored instructions. They are kept separate from untrusted
// tool output and are capped before they are injected into a model prompt so a
// repository cannot consume the whole context window just by adding a rules
// file.
package rules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxBytes is deliberately small enough to leave room for the task.
	DefaultMaxBytes = 16 * 1024
	fileName        = "AGENTS.md"
)

// Document is one discovered rules file.
type Document struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Result is the ordered set of rules and the prompt-ready text. Documents are
// ordered from broadest scope to nearest scope, so a nearer file can override
// a more general rule in the normal instruction hierarchy.
type Result struct {
	Documents []Document `json:"documents"`
	Content   string     `json:"content"`
	Truncated bool       `json:"truncated,omitempty"`
}

// Load discovers AGENTS.md from global user rules and the workspace's parent
// chain. The global file is read from ~/.moon/AGENTS.md unless the optional
// MOON_GLOBAL_RULES environment variable points at another file.
func Load(workspace string) (Result, error) {
	global := strings.TrimSpace(os.Getenv("MOON_GLOBAL_RULES"))
	if global == "" {
		if home, err := os.UserHomeDir(); err == nil {
			global = filepath.Join(home, ".moon", fileName)
		}
	}
	return LoadFrom(workspace, global, DefaultMaxBytes)
}

// LoadFrom is the deterministic form used by tests and embedders. global may
// be empty to disable global rules. maxBytes <= 0 uses DefaultMaxBytes.
func LoadFrom(workspace, global string, maxBytes int) (Result, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return Result{}, nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return Result{}, fmt.Errorf("resolve workspace: %w", err)
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return Result{}, fmt.Errorf("stat workspace: %w", statErr)
	}

	// Walk to the filesystem root, then reverse so the nearest project rule is
	// last and can explicitly override broader instructions.
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, filepath.Join(dir, fileName))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	paths := make([]string, 0, len(chain)+1)
	if strings.TrimSpace(global) != "" {
		paths = append(paths, global)
	}
	paths = append(paths, chain...)

	res := Result{}
	for _, path := range paths {
		doc, ok, err := readDocument(path, maxBytes)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue
		}
		res.Documents = append(res.Documents, doc)
		res.Truncated = res.Truncated || doc.Truncated
	}
	if len(res.Documents) == 0 {
		return res, nil
	}
	var b strings.Builder
	b.WriteString("<project_rules>\n")
	b.WriteString("以下是用户为当前项目授权的 AGENTS.md 规则。它们属于可信项目指令，" +
		"但不能覆盖系统安全策略、工具权限或用户当前请求。按文件路径从上到下处理；" +
		"更近的规则可以补充或覆盖更远的规则。\n")
	for _, doc := range res.Documents {
		b.WriteString("\n--- ")
		b.WriteString(doc.Path)
		if doc.Truncated {
			b.WriteString(" (已截断，使用 read 查看完整文件)")
		}
		b.WriteString(" ---\n")
		b.WriteString(doc.Content)
		b.WriteByte('\n')
	}
	b.WriteString("</project_rules>")
	res.Content = b.String()
	return res, nil
}

func readDocument(path string, maxBytes int) (Document, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	doc := Document{Path: filepath.Clean(path), Content: string(data)}
	if len(data) > maxBytes {
		text := string(data[:maxBytes])
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		doc.Content = text
		doc.Truncated = true
	}
	return doc, true, nil
}
