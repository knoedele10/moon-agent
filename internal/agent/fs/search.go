package fs

// 工作区检索工具：grep（按内容找）与 glob（按文件名找）。
//
// 为什么需要它们：read/list 只能「先知道文件名，再一个个打开」。
// 真实任务里模型往往只知道一个字符串、一个函数名、一个错误信息，
// 没有检索就只能瞎猜路径或把整个目录读一遍——既慢又烧上下文。
// Claude Code / Codex / ripgrep 系工具的共同结论是：**检索是一等能力**，不是 read 的附属。
//
// 安全边界与 read 完全一致：路径走 resolveForRead（工作区 + 只读技能目录），
// 逐文件仍只读、不跟随符号链接；不做任何写操作，也不执行 shell。
// 输出有双重上限（命中条数 + 总字符数），避免一次 grep 把上下文撑爆。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"blog-my/internal/agent"
)

const (
	defaultGrepLimit = 100
	maxGrepLimit     = 500
	defaultGlobLimit = 200
	maxGlobLimit     = 1000

	// maxSearchFileBytes 只读这么大的文件：更大的多半是日志/数据，交给 read 分段。
	maxSearchFileBytes = 2 << 20
	// maxSearchFiles 限制一次检索最多看多少个文件，防止在 node_modules 级目录里走丢。
	maxSearchFiles = 50000
	// maxGrepLineRunes 单行内容上限：压缩过的 JS 一行几十万字符，不截断会占满输出。
	maxGrepLineRunes = 300
	// maxGrepOutputRunes 一次 grep 返回给模型的总字符上限。
	maxGrepOutputRunes = 12000
	// maxGrepContext 上下文行数的硬上限。
	maxGrepContext = 5
	// binarySniffBytes 判定二进制时看文件头部多少字节。
	binarySniffBytes = 8000
)

// searchSkipDirs 是遍历时跳过的目录名（大小写不敏感）。
//
// 白名单式跳过而不是读 .gitignore：跨平台、无额外依赖、行为可预期。
// 这些都是依赖缓存/构建产物/VCS 元数据，检索它们几乎只产生噪声；
// 确实需要时模型可以用 read 直接打开具体文件。
var searchSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "bower_components": true,
	"vendor": true, "__pycache__": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "bin": true, "out": true, "target": true,
	".next": true, ".nuxt": true, ".cache": true, "coverage": true,
	".idea": true, ".vscode": true,
	".gocache": true, ".gomodcache": true, "gomodcache": true, "gocache": true,
	".gotmp": true, ".tmp-chrome": true,
}

// searchTools 返回检索工具（只读）。
func SearchTools() []agent.Tool { return []agent.Tool{GrepTool{}, GlobTool{}} }

// skipSearchDir 判断遍历时是否跳过该目录。
func skipSearchDir(name string) bool {
	lower := strings.ToLower(name)
	if searchSkipDirs[lower] {
		return true
	}
	// 构建缓存目录常带后缀（.gocache-hermes、.gomodcache-hermes）。
	for prefix := range searchSkipDirs {
		if strings.HasPrefix(prefix, ".") && strings.HasPrefix(lower, prefix+"-") {
			return true
		}
	}
	return false
}

// ---------- grep ----------

type grepArgs struct {
	Pattern        string `json:"pattern"`
	Path           string `json:"path"`
	Glob           string `json:"glob"`
	CaseSensitive  bool   `json:"case_sensitive"`
	FixedStrings   bool   `json:"fixed_strings"`
	Context        int    `json:"context"`
	Limit          int    `json:"limit"`
	IncludeIgnored bool   `json:"include_ignored"`
}

// GrepTool 在工作区文件内容里按正则搜索。
type GrepTool struct{}

func (GrepTool) Name() string { return "grep" }

func (GrepTool) Description() string {
	return "在工作区里按正则搜索文件内容，返回 路径:行号: 内容。适合找函数名、变量、报错文案、配置项。" +
		"path 默认整个工作区，可传子目录或单个文件；glob 可限定文件类型（如 *.go、**/*.ts）；" +
		"context 可带前后文，limit 限制命中条数。大小写不敏感，可用 case_sensitive 打开；" +
		"fixed_strings=true 时按字面量而不是正则。只读，自动跳过 .git/node_modules/build 等目录。"
}

func (GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":         map[string]any{"type": "string", "description": "要搜索的内容：正则表达式，或 fixed_strings=true 时的字面量"},
			"path":            map[string]any{"type": "string", "description": "搜索根目录或单个文件，默认 .（主工作区）；其他工作区用 @workspace2/ 前缀"},
			"glob":            map[string]any{"type": "string", "description": "只搜索匹配该通配符的文件，如 *.go、**/*.ts；不传则搜索全部文本文件"},
			"case_sensitive":  map[string]any{"type": "boolean", "description": "是否区分大小写，默认 false"},
			"fixed_strings":   map[string]any{"type": "boolean", "description": "把 pattern 当普通文本而不是正则，默认 false"},
			"context":         map[string]any{"type": "integer", "description": "每条命中前后各带几行上下文（0-5，默认 0）"},
			"limit":           map[string]any{"type": "integer", "description": "最多返回多少条命中（默认 100，上限 500）"},
			"include_ignored": map[string]any{"type": "boolean", "description": "是否也搜索 .git/node_modules/build 等默认跳过目录，默认 false"},
		},
		"required": []string{"pattern"},
	}
}

func (GrepTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a grepArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	pattern := strings.TrimSpace(a.Pattern)
	if pattern == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "pattern 不能为空"}
	}
	expr := pattern
	if a.FixedStrings {
		expr = regexp.QuoteMeta(pattern)
	}
	if !a.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("正则表达式无效（%q）：%v；可先用 fixed_strings=true 按字面量搜索", pattern, err)}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultGrepLimit
	}
	if limit > maxGrepLimit {
		limit = maxGrepLimit
	}
	contextLines := a.Context
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > maxGrepContext {
		contextLines = maxGrepContext
	}
	fileGlob := strings.TrimSpace(a.Glob)
	var fileMatcher *globMatcher
	if fileGlob != "" {
		fileMatcher, err = newGlobMatcher(fileGlob)
		if err != nil {
			return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("glob 无效（%q）：%v", fileGlob, err)}
		}
	}

	if strings.TrimSpace(a.Path) == "" {
		a.Path = "."
	}
	target, display, err := resolveForRead(ex, a.Path)
	if err != nil {
		return agent.Result{}, err
	}
	fi, statErr := os.Stat(target)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot search \"%s\": path does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(statErr, target, display)}
	}

	var files []searchFile
	if fi.IsDir() {
		files, err = collectSearchFiles(ctx, target, display, fileMatcher, a.IncludeIgnored, maxSearchFiles)
		if err != nil {
			return agent.Result{}, err
		}
	} else {
		if fi.Size() > maxSearchFileBytes {
			// 单个文件也会被遍历逻辑跳过；直接说清楚，别让模型以为「文件里没有」。
			return agent.Result{}, &Error{
				Code: CodeInvalidArgs, Path: display,
				Reason: fmt.Sprintf("\"%s\" 有 %.1f MB，超过单次搜索上限 %d MB；请用 read 的 offset/limit 分段查看，或缩小搜索范围", display, float64(fi.Size())/(1<<20), maxSearchFileBytes>>20),
			}
		}
		files = []searchFile{{abs: target, display: display}}
	}

	out := &grepOutput{limit: limit, withContext: contextLines > 0}
	scanned := 0
	for _, f := range files {
		if err := guard(ctx, ex); err != nil {
			return agent.Result{}, err
		}
		if out.full() {
			break
		}
		// 单个文件过大时跳过：它多半是日志/数据，交给 read 分段读更合适。
		if info, statErr := os.Stat(f.abs); statErr != nil || info.Size() > maxSearchFileBytes {
			continue
		}
		scanned++
		truncated, err := grepFile(f, re, contextLines, out)
		if err != nil {
			// 单个文件读失败不让整次搜索失败：跳过它，继续找别的文件。
			continue
		}
		if truncated {
			out.truncated = true
		}
	}

	if out.matches == 0 {
		hint := fmt.Sprintf("未找到匹配：pattern=%q，搜索了 %d 个文件", pattern, scanned)
		if fileGlob != "" {
			hint += "（文件过滤 " + fileGlob + "）"
		}
		hint += "。可以换关键词、去掉 glob 限制，或用 glob 工具先确认文件是否存在。"
		return agent.Result{Content: hint, Display: map[string]any{
			"pattern": pattern, "path": display, "files_scanned": scanned, "matches": 0,
		}}, nil
	}

	header := fmt.Sprintf("在 %d 个文件里找到 %d 条匹配", scanned, out.matches)
	if out.truncated || out.outputCapped {
		header += "（已截断）"
	}
	body := header + "\n" + out.builder.String()
	if out.outputCapped {
		body += fmt.Sprintf("\n…（输出已达 %d 字符上限，请缩小 path/glob 或减小 context 后再搜）\n", maxGrepOutputRunes)
	} else if out.truncated {
		body += "\n…（还有更多匹配未显示，请缩小范围或提高 limit）\n"
	}
	return agent.Result{
		Content: body,
		Display: map[string]any{
			"pattern": pattern, "path": display, "files_scanned": scanned,
			"matches": out.matches, "truncated": out.truncated || out.outputCapped,
		},
	}, nil
}

// searchFile 是遍历结果里的一个候选文件。
type searchFile struct {
	abs     string // 绝对路径（已确认位于工作区内）
	display string // 给模型看的相对路径（统一 /）
}

// collectSearchFiles 遍历目录，返回候选文件。
//
// 不跟随符号链接（WalkDir 本身不跟随），跳过缓存/构建目录；
// 文件过滤用与 glob 工具相同的匹配器，保证两个工具的行为一致。
func collectSearchFiles(ctx context.Context, root, displayRoot string, matcher *globMatcher, includeIgnored bool, maxFiles int) ([]searchFile, error) {
	var files []searchFile
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// 无权限的目录/文件直接跳过，不让一个坏节点终止整次搜索。
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if !includeIgnored && skipSearchDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接、设备文件一律不跟
		}
		if len(files) >= maxFiles {
			return io.EOF // 用 EOF 终止遍历，下面吞掉
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matcher != nil && !matcher.Match(relSlash) {
			return nil
		}
		files = append(files, searchFile{abs: p, display: joinDisplay(displayRoot, relSlash)})
		return nil
	})
	if err != nil && err != io.EOF && ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return files, nil
}

// browseLabel 把检索根与相对路径拼成展示路径；根是 "." 时不重复前缀。
func joinDisplay(root, rel string) string {
	root = strings.TrimSuffix(strings.ReplaceAll(root, "\\", "/"), "/")
	if root == "" || root == "." {
		return rel
	}
	return path.Join(root, rel)
}

// grepOutput 汇总一次搜索的输出与上限状态。
type grepOutput struct {
	builder      strings.Builder
	limit        int
	matches      int
	runes        int // 已写入的字符数（避免每次重算整段 builder）
	withContext  bool
	truncated    bool
	outputCapped bool
}

func (o *grepOutput) full() bool { return o.matches >= o.limit || o.outputCapped }

// write 追加一行；返回 false 表示已达总字符上限，调用方应停止。
func (o *grepOutput) write(line string) bool {
	if o.outputCapped {
		return false
	}
	n := utf8.RuneCountInString(line)
	if o.runes+n > maxGrepOutputRunes {
		o.outputCapped = true
		return false
	}
	o.runes += n
	o.builder.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		o.builder.WriteByte('\n')
	}
	return true
}

// grepFile 在单个文件里找匹配，直接累加 out.matches。
// 返回 truncated=true 表示命中数或输出已达上限，调用方应停止继续扫描。
func grepFile(f searchFile, re *regexp.Regexp, contextLines int, out *grepOutput) (truncated bool, err error) {
	data, err := os.ReadFile(f.abs)
	if err != nil {
		return false, err
	}
	if isBinary(data) {
		return false, nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	lastEmitted := 0 // 已输出的最大行号（1 起），避免上下文重复
	for i, line := range lines {
		if out.full() {
			return true, nil
		}
		if !re.MatchString(line) {
			continue
		}
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		if start <= lastEmitted {
			start = lastEmitted // lastEmitted 是行号，下一行下标等于行号
		}
		end := i + contextLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := start; j <= end; j++ {
			if j < 0 || j >= len(lines) {
				continue
			}
			marker := "-"
			if j == i {
				marker = ":"
			}
			lineText := clipSearchLine(lines[j])
			if !out.write(fmt.Sprintf("%s:%d%s %s\n", f.display, j+1, marker, lineText)) {
				out.matches++
				return true, nil
			}
			lastEmitted = j + 1
		}
		out.matches++
	}
	return false, nil
}

// isBinary 用「头部是否含 NUL」判断二进制；和 git/ripgrep 的做法一致。
func isBinary(data []byte) bool {
	head := data
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// clipSearchLine 截断超长行，避免压缩产物/长 JSON 一行吃掉全部输出。
func clipSearchLine(s string) string {
	s = strings.TrimSuffix(s, "\r")
	if utf8.RuneCountInString(s) <= maxGrepLineRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxGrepLineRunes]) + "…"
}

// ---------- glob ----------

type globArgs struct {
	Pattern        string `json:"pattern"`
	Path           string `json:"path"`
	Limit          int    `json:"limit"`
	IncludeIgnored bool   `json:"include_ignored"`
}

// GlobTool 按文件名/路径通配符列出工作区里的文件。
type GlobTool struct{}

func (GlobTool) Name() string { return "glob" }

func (GlobTool) Description() string {
	return "按通配符查找工作区里的文件，返回相对路径列表（按字典序）。" +
		"pattern 支持 **（任意层级目录）、*（单层任意字符）、?（单字符），例如 **/*.go、src/**/test_*.js、*.md；" +
		"不含 / 的 pattern 会匹配任意目录下的文件名。path 可限定搜索根目录。" +
		"只读，自动跳过 .git/node_modules/build 等目录；需要按内容找时用 grep。"
}

func (GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":         map[string]any{"type": "string", "description": "文件通配符，如 **/*.go、src/**/*.ts、*.md"},
			"path":            map[string]any{"type": "string", "description": "搜索根目录，默认 .（主工作区）；其他工作区用 @workspace2/ 前缀"},
			"limit":           map[string]any{"type": "integer", "description": "最多返回多少个文件（默认 200，上限 1000）"},
			"include_ignored": map[string]any{"type": "boolean", "description": "是否也搜索 .git/node_modules/build 等默认跳过目录，默认 false"},
		},
		"required": []string{"pattern"},
	}
}

func (GlobTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a globArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	pattern := strings.TrimSpace(a.Pattern)
	if pattern == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "pattern 不能为空，例如 **/*.go"}
	}
	matcher, err := newGlobMatcher(pattern)
	if err != nil {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("pattern 无效（%q）：%v", pattern, err)}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultGlobLimit
	}
	if limit > maxGlobLimit {
		limit = maxGlobLimit
	}
	if strings.TrimSpace(a.Path) == "" {
		a.Path = "."
	}
	target, display, err := resolveForRead(ex, a.Path)
	if err != nil {
		return agent.Result{}, err
	}
	fi, statErr := os.Stat(target)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot list \"%s\": path does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(statErr, target, display)}
	}
	if !fi.IsDir() {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("\"%s\" 不是目录（glob 需要目录作为搜索根）", display)}
	}

	files, _ := collectSearchFiles(ctx, target, display, matcher, a.IncludeIgnored, maxSearchFiles)
	sort.Slice(files, func(i, j int) bool { return files[i].display < files[j].display })

	total := len(files)
	shown := total
	if shown > limit {
		shown = limit
	}
	if total == 0 {
		return agent.Result{
			Content: fmt.Sprintf("没有找到匹配 %q 的文件（搜索根：%s）。可以去掉部分层级、把 pattern 改成 **/* 看看目录结构，或确认 path 是否正确。", pattern, display),
			Display: map[string]any{"pattern": pattern, "path": display, "files": 0},
		}, nil
	}
	var b strings.Builder
	for i := 0; i < shown; i++ {
		b.WriteString(files[i].display)
		b.WriteByte('\n')
	}
	if shown < total {
		fmt.Fprintf(&b, "...（共 %d 个文件，已列出 %d 个；请提高 limit 或缩小 pattern）\n", total, shown)
	}
	return agent.Result{
		Content: b.String(),
		Display: map[string]any{"pattern": pattern, "path": display, "files": total, "shown": shown},
	}, nil
}

// compileGlob 把通配符编译成正则。支持 **、*、?，其余字符按字面量处理。
//
// 与 filepath.Match 的区别：标准库不支持 **（跨目录层级），而 **/*.go 是模型最常写的形态。
// 这里统一转成斜杠路径后再匹配，Windows 与 Unix 行为一致。
func compileGlob(pattern string) (*regexp.Regexp, error) {
	pattern = strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == "" {
		return nil, fmt.Errorf("pattern 不能为空")
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// ** 后面紧跟 / 时，允许零层或多层目录：a/**/b 也能匹配 a/b。
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// globMatcher 是编译后的通配符，保留原 pattern 用于「无斜杠时按文件名匹配」。
type globMatcher struct {
	re      *regexp.Regexp
	pattern string
}

// newGlobMatcher 编译 pattern；调用方负责把错误转成可修复的提示。
func newGlobMatcher(pattern string) (*globMatcher, error) {
	re, err := compileGlob(pattern)
	if err != nil {
		return nil, err
	}
	return &globMatcher{re: re, pattern: strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")}, nil
}

// Match 判断相对路径是否匹配。
//
// 不含 / 的 pattern 同时匹配「完整相对路径」与「文件名」：
// 模型写 *.md 时通常想找任意层级下的 Markdown，而不是只找根目录那几个。
func (m *globMatcher) Match(rel string) bool {
	if m == nil || m.re == nil {
		return true
	}
	if m.re.MatchString(rel) {
		return true
	}
	if !strings.Contains(m.pattern, "/") {
		base := rel
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			base = rel[i+1:]
		}
		return m.re.MatchString(base)
	}
	return false
}
