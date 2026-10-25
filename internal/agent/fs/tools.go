package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"blog-my/internal/agent"
)

const (
	defaultReadLimit = 2000
	maxImageBytes    = 10 << 20 // 10MB：再大就不往模型上下文里塞了
	// maxFullReadBytes 是「整文件读进内存」的上限。
	// 超过它改走流式按行读取：模型要的通常只是某一段，而一次 os.ReadFile
	// 读几百 MB 的日志会直接把进程内存打满。PDF/Office 需要全量字节解析，超限则明确拒绝。
	maxFullReadBytes = 8 << 20
)

// Tools 返回契约 §4 的四个工具，顺序稳定（即暴露给模型的顺序）。
func Tools() []agent.Tool {
	return []agent.Tool{ReadTool{}, WriteTool{}, EditTool{}, ReadImageTool{}}
}

// ToolsWithMutation 返回包含删除类能力的工具集（web 接线层用）。
//
// 与 Tools() 分开是为了让“旧集合”仍然可取：删除是不可逆操作，
// 需要显式选择才出现在给模型的工具列表里。
func ToolsWithMutation() []agent.Tool {
	return []agent.Tool{ReadTool{}, ListTool{}, WriteTool{}, EditTool{}, MoveTool{}, DeleteTool{}, ReadImageTool{}, OfficeEditTool{}}
}

// ---------- 参数 ----------

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type writeArgs struct {
	FilePath string  `json:"file_path"`
	Content  *string `json:"content"` // 契约：content 必填，但允许空串（空文件是合法内容）
}

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

type readImageArgs struct {
	FilePath string `json:"file_path"`
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &Error{Code: CodeInvalidArgs, Reason: "参数不是合法 JSON: " + err.Error()}
	}
	return nil
}

// guard 统一做取消检查（契约 §3.6：每个工具执行前检查 ctx 取消）。
func guard(ctx context.Context, ex *agent.Exec) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if ex != nil && ex.Signal != nil {
		select {
		case <-ex.Signal.Done():
			return ex.Signal.Err()
		default:
		}
	}
	return nil
}

func sessionOf(ex *agent.Exec) string {
	if ex == nil {
		return ""
	}
	return ex.SessionID
}

func rootOf(ex *agent.Exec) string {
	if ex == nil {
		return ""
	}
	return ex.Root
}

// resolveFor 解析路径并给出展示用路径。
func resolveFor(ex *agent.Exec, p string) (target, display string, err error) {
	if ex != nil {
		target, display, err = ResolveWorkspaces(rootOf(ex), ex.Roots, p)
		if err == nil {
			return target, display, nil
		}
		if !ex.AllowOutside {
			return "", strings.TrimSpace(p), err
		}
		if !filepath.IsAbs(strings.TrimSpace(p)) {
			var pathErr *Error
			if strings.TrimSpace(rootOf(ex)) != "" && errors.As(err, &pathErr) && pathErr.Code == CodeSandboxDenied {
				return "", strings.TrimSpace(p), &Error{Code: CodeSandboxDenied, Reason: OutsideNeedsAbsPath}
			}
			return "", strings.TrimSpace(p), err
		}
	}
	// AllowOutside=false（默认）时与改造前完全一致：只有工作区内的路径能过。
	// =true 时额外放行工作区外的绝对路径（见 outside.go）。
	if ex != nil && ex.AllowOutside {
		return resolveOutside(rootOf(ex), p)
	}
	return "", strings.TrimSpace(p), &Error{Code: CodeSandboxDenied}
}

// resolveForRead 是只读工具专用的解析：先看工作区，再退到只读的技能目录。
//
// 为什么单独开一个：技能里引用了大量自己的文件（workflows/、references/、scripts/），
// 模型必须能读到它们才能按技能做事；但技能目录绝不能被写——写类工具仍然只认工作区。
func resolveForRead(ex *agent.Exec, p string) (target, display string, err error) {
	workTarget, workDisplay, workErr := resolveFor(ex, p)
	// 关键：Resolve 是**词法**解析，不要求文件真的存在——
	// 所以“工作区解析成功”并不等于“工作区里有这个文件”。必须 Stat 一下才算命中，
	// 否则技能目录的读取永远轮不上（实测踩过：ppt-master/SKILL.md 报 file does not exist）。
	if workErr == nil {
		if _, serr := os.Stat(workTarget); serr == nil {
			return workTarget, workDisplay, nil
		}
	}

	if ex == nil || strings.TrimSpace(ex.ReadRoot) == "" {
		if workErr != nil {
			return "", strings.TrimSpace(p), workErr
		}
		return workTarget, workDisplay, nil // 没配只读根：维持原行为（由调用方报不存在）
	}

	t, rerr := Resolve(ex.ReadRoot, p)
	if rerr != nil {
		if workErr == nil {
			return workTarget, workDisplay, nil
		}
		return "", strings.TrimSpace(p), workErr
	}
	// 展示路径加前缀，让模型分得清“读的是技能文件”而不是工作区文件。
	return t, "skill/" + DisplayPath(ex.ReadRoot, t), nil
}

// recordAbsent / recordPresent：观察表写入（契约 §4.2）。
func recordAbsent(ex *agent.Exec, target string) {
	observations.set(sessionOf(ex), target, Observation{Kind: ObsAbsent})
}

func recordPresent(ex *agent.Exec, target string, fi os.FileInfo) {
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: versionOf(fi)})
}

// numberLines 生成 N<TAB>内容 的行号文本。offset 1 起，limit<=0 用默认值。
func numberLines(s string, offset, limit int) (text string, shown, total int, truncated bool) {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // 末尾换行不算一行
	}
	total = len(lines)
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}
	start := offset - 1
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteByte('\t')
		b.WriteString(strings.TrimSuffix(lines[i], "\r"))
		b.WriteByte('\n')
	}
	return b.String(), end - start, total, end < total
}

// ---------- read ----------

// ReadTool 读取文本文件。
type ReadTool struct{}

func (ReadTool) Name() string { return "read" }

func (ReadTool) Description() string {
	return "读取一个文件，返回带行号的内容（格式：行号<TAB>内容）。支持文本 / Markdown / 代码，" +
		"PDF（自动抽取文本层；扫描件或加密 PDF 读不出来，那种请用 read_image），以及 Word（.docx，含表格）、Excel（.xlsx，按行列）、PowerPoint（.pptx，按页）。" +
		"默认最多 2000 行，文件更长时用 offset/limit 分段读。修改文件前必须先 read，否则 write/edit/move/delete 会被拒绝。"
}

func (ReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "要读取的文件路径（相对根目录，或根目录内的绝对路径）"},
			"offset":    map[string]any{"type": "integer", "description": "从第几行开始读（1 起，默认 1）"},
			"limit":     map[string]any{"type": "integer", "description": "最多读多少行（默认 2000）"},
		},
		"required": []string{"file_path"},
	}
}

func (ReadTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a readArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	target, display, err := resolveForRead(ex, a.FilePath) // 读：工作区 + 只读技能目录
	if err != nil {
		return agent.Result{}, err
	}

	fi, err := os.Stat(target)
	if err != nil {
		// 契约 §4.2：read 失败也要记 absent（照 DSH）——
		// 这样之后 write 仍能创建，edit 报 FS_NOT_FOUND 而不是 FS_NOT_OBSERVED。
		recordAbsent(ex, target)
		if os.IsNotExist(err) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot read \"%s\": file does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备）"}
	}

	// 超大文本走流式窗口读取：只按 offset/limit 读需要的行，不把整个文件读进内存。
	// PDF/Office 必须拿到完整字节才能解析，超过上限时明确拒绝而不是硬扛 OOM。
	if fi.Size() > maxFullReadBytes {
		if isPDF(target, nil) || officeKind(target) != "" {
			return agent.Result{}, &Error{
				Code: CodeInvalidArgs, Path: display,
				Reason: fmt.Sprintf("\"%s\" 有 %.1f MB，超过 %d MB 的文档解析上限；请先拆分或转换成文本后再读", display, float64(fi.Size())/(1<<20), maxFullReadBytes>>20),
			}
		}
		text, shown, truncated, rerr := readTextWindow(target, a.Offset, a.Limit)
		if rerr != nil {
			recordAbsent(ex, target)
			return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(rerr, target, display)}
		}
		if shown == 0 {
			text = fmt.Sprintf("(从第 %d 行起没有内容：文件可能更短，或 offset 超出末尾)", normalizedOffset(a.Offset))
		}
		if truncated {
			text += fmt.Sprintf("... (文件较大（%.1f MB），已显示 %d 行；用 offset=%d 继续)\n",
				float64(fi.Size())/(1<<20), shown, normalizedOffset(a.Offset)+shown)
		}
		if fi2, err2 := os.Stat(target); err2 == nil {
			recordPresent(ex, target, fi2)
		}
		return agent.Result{
			Content: text,
			Display: map[string]any{"file_path": display, "shown": shown, "bytes": fi.Size(), "truncated": truncated, "streamed": true},
		}, nil
	}

	data, err := os.ReadFile(target)
	if err != nil {
		recordAbsent(ex, target)
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	// PDF 不是文本：直接当文本按行号读会吐出一堆二进制乱码，改走文本层抽取。
	if isPDF(target, data) {
		extracted, perr := ExtractPDFText(data)
		if perr != nil {
			return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: perr.Error()}
		}
		if fi2, err2 := os.Stat(target); err2 == nil {
			recordPresent(ex, target, fi2)
		}
		pdfText, pdfShown, pdfTotal, pdfTruncated := numberLines(extracted, a.Offset, a.Limit)
		if pdfTotal == 0 {
			pdfText = "(PDF 里没有可提取的文本层)"
		}
		if pdfTruncated {
			pdfText += fmt.Sprintf("... (共 %d 行，已显示 %d 行；用 offset=%d 继续)\n", pdfTotal, pdfShown, a.Offset+pdfShown)
		}
		return agent.Result{
			Content: pdfText,
			Display: map[string]any{"file_path": display, "lines": pdfTotal, "shown": pdfShown, "bytes": len(data), "truncated": pdfTruncated, "pdf": true},
		}, nil
	}
	// Word / Excel 同样是二进制：走 zip + XML 抽取（见 office.go）。
	if kind := officeKind(target); kind != "" {
		var (
			extracted string
			oerr      error
		)
		switch kind {
		case "docx":
			extracted, oerr = ExtractDocxText(data)
		case "pptx":
			extracted, oerr = ExtractPptxText(data)
		default:
			extracted, oerr = ExtractXlsxText(data)
		}
		if oerr != nil {
			return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: oerr.Error()}
		}
		if fi2, err2 := os.Stat(target); err2 == nil {
			recordPresent(ex, target, fi2)
		}
		docText, docShown, docTotal, docTruncated := numberLines(extracted, a.Offset, a.Limit)
		if docTotal == 0 {
			docText = "(文档里没有可提取的文本)"
		}
		if docTruncated {
			docText += fmt.Sprintf("... (共 %d 行，已显示 %d 行；用 offset=%d 继续)\n", docTotal, docShown, a.Offset+docShown)
		}
		return agent.Result{
			Content: docText,
			Display: map[string]any{"file_path": display, "lines": docTotal, "shown": docShown, "bytes": len(data), "truncated": docTruncated, "office": kind},
		}, nil
	}

	// 读成功后按「读之后的版本」记录：期间被外部改过也不会记成旧版本。
	if fi2, err2 := os.Stat(target); err2 == nil {
		recordPresent(ex, target, fi2)
	}

	text, shown, total, truncated := numberLines(string(data), a.Offset, a.Limit)
	if total == 0 {
		text = "(文件为空)"
	}
	if truncated {
		text += fmt.Sprintf("... (共 %d 行，已显示 %d 行；用 offset=%d 继续)\n", total, shown, a.Offset+shown)
	}
	return agent.Result{
		Content: text,
		Display: map[string]any{"file_path": display, "lines": total, "shown": shown, "bytes": len(data), "truncated": truncated},
	}, nil
}

// ---------- write ----------

// WriteTool 写入整个文件（创建或覆盖）。
type WriteTool struct{}

func (WriteTool) Name() string { return "write" }

func (WriteTool) Description() string {
	return "把一个文件的完整内容写入磁盘（不存在则创建）。" +
		"覆盖已存在的文件前必须先 read 过它：没读过的文件不会被覆盖，改小的改动请用 edit。"
}

func (WriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "要写入的文件路径（相对根目录，或根目录内的绝对路径）"},
			"content":   map[string]any{"type": "string", "description": "完整文件内容（允许空串）"},
		},
		"required": []string{"file_path", "content"},
	}
}

func (WriteTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a writeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	if a.Content == nil {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "content 必填（允许空串，但不能缺省）"}
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}
	data := []byte(*a.Content)

	// 观察快照在加锁之前取：与 edit 同理，基准是「模型决定写入时它看到的版本」。
	o := observations.get(sessionOf(ex), target)

	l := lockTarget(sessionOf(ex), target)
	l.Lock()
	defer l.Unlock()

	createIfAbsent := o.Kind != ObsPresent
	if o.Kind == ObsPresent {
		fi, err := os.Stat(target)
		switch {
		case err != nil:
			createIfAbsent = true // 记录说存在、磁盘上已消失 → 当创建处理
		case !fi.Mode().IsRegular():
			return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备）"}
		case versionOf(fi) != o.Version:
			return agent.Result{}, &Error{
				Code: CodeStaleVersion, Path: display,
				Reason: fmt.Sprintf("cannot write \"%s\": the file changed on disk since it was read (expected %s, found %s)", display, o.Version, versionOf(fi)),
			}
		default:
			createIfAbsent = false
		}
	}

	// 创建新文件时按需补父目录：模型很自然地会写 notes/a.txt 这类嵌套路径，
	// 父目录缺失时 os.CreateTemp 会直接报 IO 错，用户只看到「工具失败」却不知为何。
	// 只在**工作区内**补目录：AllowOutside 档也不替模型在工作区外造目录树。
	if createIfAbsent {
		// within 守卫：target 就是根目录本身时，Dir(target) 会跑到根之外，不能在那里建目录。
		if IsWithinWorkspaces(ex.Root, ex.Roots, filepath.Dir(target)) {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
			}
		}
	}

	if !createIfAbsent {
		// 覆盖已有文件前留一份检查点，供 restore 撤回；失败不阻断写入。
		saveCheckpointForTarget(ex, target, display)
	}
	ver, created, err := writeFileAtomic(target, display, data, createIfAbsent)
	if err != nil {
		return agent.Result{}, err
	}
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: ver})

	verb := "Updated"
	if created {
		verb = "Created"
	}
	return agent.Result{
		Content: fmt.Sprintf("%s %s (%d 字节)", verb, display, len(data)),
		Display: map[string]any{"file_path": display, "created": created, "bytes": len(data), "version": ver},
	}, nil
}

// ---------- edit ----------

// EditTool 在文件内做字面量替换。
type EditTool struct{}

func (EditTool) Name() string { return "edit" }

func (EditTool) Description() string {
	return "在文件里把 old_string 替换成 new_string（字面量匹配，不是正则）。" +
		"old_string 必须与文件中内容完全一致（含缩进）；默认要求唯一匹配，替换多处用 replace_all=true。" +
		"编辑前必须 read 过该文件，且文件在读过之后不能被外部改过。"
}

func (EditTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path":   map[string]any{"type": "string", "description": "要编辑的文件路径"},
			"old_string":  map[string]any{"type": "string", "description": "要被替换掉的原文（必须与文件内容逐字符一致）"},
			"new_string":  map[string]any{"type": "string", "description": "替换成的新文本（可与 old_string 不同；不允许完全相同）"},
			"replace_all": map[string]any{"type": "boolean", "description": "true 时替换所有匹配；默认 false（要求唯一匹配）"},
		},
		"required": []string{"file_path", "old_string", "new_string"},
	}
}

func (EditTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a editArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	if strings.TrimSpace(a.OldString) == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "old_string 不能为空"}
	}
	if a.OldString == a.NewString {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "old_string 与 new_string 相同，没有可编辑的内容"}
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}

	// **观察快照必须在加锁之前取**：基准是「模型决定编辑时它看到的版本」，
	// 不是「轮到我时文件是什么版本」。
	//
	// 否则并发两个 edit 时，赢家提交后会把记录更新成写后的版本，
	// 输家再加锁就拿到「新鲜」的版本，守卫被绕过 —— 它会以「找不到匹配」告终，
	// 把模型引向错误的修法（去改 old_string，而不是重新读文件）。
	o := observations.get(sessionOf(ex), target)

	// 契约 §4.6：整个 edit 是一个临界区——探版本 → 校验守卫 → 读 → 匹配 → 重写。
	l := lockTarget(sessionOf(ex), target)
	l.Lock()
	defer l.Unlock()

	switch o.Kind {
	case ObsUnknown:
		return agent.Result{}, &Error{Code: CodeNotObserved, Path: display}
	case ObsAbsent:
		return agent.Result{}, &Error{Code: CodeNotFound, Path: display}
	}

	fi, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备）"}
	}

	// 版本守卫**必须在字面量匹配之前**：陈旧编辑要报 FS_STALE_VERSION，
	// 绝不能报「找不到匹配」——那会把模型引向错误的修法（它会去改 old_string，而不是重新读文件）。
	cur := versionOf(fi)
	if cur != o.Version {
		return agent.Result{}, &Error{
			Code: CodeStaleVersion, Path: display,
			Reason: fmt.Sprintf("cannot edit \"%s\": the file changed on disk since it was read (expected %s, found %s)", display, o.Version, cur),
		}
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	content := string(data)
	count := strings.Count(content, a.OldString)
	if count == 0 {
		return agent.Result{}, &Error{Code: CodeNoMatch, Path: display}
	}
	if count > 1 && !a.ReplaceAll {
		return agent.Result{}, &Error{Code: CodeAmbiguous, Path: display, Reason: fmt.Sprintf("%d times", count)}
	}
	replaced := count
	if !a.ReplaceAll {
		replaced = 1
	}
	newContent := strings.Replace(content, a.OldString, a.NewString, replaced)
	// 第一处匹配在旧内容里的字节偏移。替换只发生在它之后，所以这个偏移在新内容里同样有效，
	// 用来算「改到了第几行」——即便 new_string 为空（删除）也能定位。
	firstOffset := strings.Index(content, a.OldString)

	// 旧内容已经在手，直接留检查点；失败不阻断编辑。
	_ = checkpoints.save(sessionOf(ex), turnIDOf(ex), target, display, data)

	ver, _, err := writeFileAtomic(target, display, []byte(newContent), false)
	if err != nil {
		return agent.Result{}, err
	}
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: ver})

	feedback, startLine, endLine := editRedisplay(newContent, firstOffset, a.NewString, replaced)
	return agent.Result{
		Content: fmt.Sprintf("Edited %s (%d 处替换)\n%s", display, replaced, feedback),
		Display: map[string]any{
			"file_path": display, "replacements": replaced, "version": ver,
			"changed_lines": fmt.Sprintf("%d-%d", startLine, endLine),
		},
	}, nil
}

// editRedisplay 在编辑成功后回显修改区域（带行号），对齐 SWE-agent 的
// 「edit 之后自动重显」设计：模型不必再花一次 read 才能确认改成了什么样，
// 也更容易立刻发现缩进错位、多删了一行这类问题。
//
// 只回显修改处前后各 3 行、最多 60 行：既能看清上下文，又不会把大文件塞回上下文。
// 需要完整文件时模型仍应显式 read（那也是「修改后验证」的正规来源）。
func editRedisplay(newContent string, firstOffset int, newString string, replaced int) (feedback string, startLine, endLine int) {
	const contextLines = 3
	const maxLines = 60
	if firstOffset < 0 {
		return "（已写入；无法定位修改行，请 read 核对）", 0, 0
	}
	// 偏移落在多字节字符中间时 Count 仍然正确：我们只数换行符。
	startLine = 1 + strings.Count(newContent[:firstOffset], "\n")
	endLine = startLine + strings.Count(newString, "\n")
	if endLine < startLine {
		endLine = startLine
	}
	first := startLine - contextLines
	if first < 1 {
		first = 1
	}
	last := endLine + contextLines
	text, shown, total, truncated := numberLines(newContent, first, last-first+1)
	if shown == 0 {
		return "（已写入；修改区域为空）", startLine, endLine
	}
	var b strings.Builder
	fmt.Fprintf(&b, "修改区域回显（第 %d-%d 行，文件共 %d 行；本次改的是第 %d-%d 行）：\n",
		first, first+shown-1, total, startLine, endLine)
	b.WriteString(text)
	if truncated || first+shown-1 < endLine {
		b.WriteString("…（只回显修改区域附近；需要完整内容请 read）")
	}
	if replaced > 1 {
		fmt.Fprintf(&b, "\n注意：本次共替换 %d 处，上面只回显第一处附近。", replaced)
	}
	return b.String(), startLine, endLine
}

// ---------- read_image ----------

// ReadImageTool 读取图片并把字节作为 Result.Image 交给循环回灌。
type ReadImageTool struct{}

func (ReadImageTool) Name() string { return "read_image" }

func (ReadImageTool) Description() string {
	return "读取一张图片（png/jpeg/gif/webp/bmp），把图片本身交给模型查看。" +
		"文本文件请用 read。"
}

func (ReadImageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "要读取的图片路径"},
		},
		"required": []string{"file_path"},
	}
}

func (ReadImageTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a readImageArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}

	fi, err := os.Stat(target)
	if err != nil {
		recordAbsent(ex, target)
		if os.IsNotExist(err) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot read image \"%s\": file does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备）"}
	}
	if fi.Size() > maxImageBytes {
		return agent.Result{}, &Error{Code: CodeImageTooLarge, Path: display, Reason: fmt.Sprintf("%d 字节超过上限 %d 字节", fi.Size(), maxImageBytes)}
	}

	data, err := os.ReadFile(target)
	if err != nil {
		recordAbsent(ex, target)
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("\"%s\" 不是图片（嗅探到 %s）", display, mime)}
	}
	if fi2, err2 := os.Stat(target); err2 == nil {
		recordPresent(ex, target, fi2)
	}

	return agent.Result{
		Content: fmt.Sprintf("已读取图片 %s（%s，%d 字节）", display, mime, len(data)),
		Display: map[string]any{"file_path": display, "mime": mime, "bytes": len(data)},
		Image:   &agent.ImagePart{MIME: mime, Data: data},
	}, nil
}

// ---------- list ----------

const defaultListLimit = 200

type listArgs struct {
	DirPath string `json:"dir_path"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
}

// ListTool 列出工作区里某个目录的直接子项。
//
// 只读工具：用户把工作区指到一个真实目录后，模型需要能先“看一眼有什么”，
// 否则只能靠猜文件名（尤其是删文件之前）。
type ListTool struct{}

func (ListTool) Name() string { return "list" }

func (ListTool) Description() string {
	return "列出活动工作区里某个目录的直接子项（不传 dir_path 就是主工作区根目录）。" +
		"其他活动工作区使用 @workspace2/、@workspace3/ 等前缀选择；不确定有哪些文件时先用它看一眼。" +
		"目录很大时用 limit/offset 分页。只读，不改任何东西。"
}

func (ListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dir_path": map[string]any{"type": "string", "description": "要列出的目录；缺省为主工作区根目录，其他活动工作区使用 @workspace2/、@workspace3/ 等前缀"},
			"limit":    map[string]any{"type": "integer", "description": "最多列出多少项（默认 200）"},
			"offset":   map[string]any{"type": "integer", "description": "从第几项开始（默认 0）；上一页末尾会给出 next_offset"},
		},
	}
}

func (ListTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a listArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	if strings.TrimSpace(a.DirPath) == "" {
		a.DirPath = "."
	}
	target, display, err := resolveForRead(ex, a.DirPath) // 列目录同样允许看技能目录
	if err != nil {
		return agent.Result{}, err
	}

	fi, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot list \"%s\": directory does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if !fi.IsDir() {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("\"%s\" 不是目录（list 只列目录）", display)}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}

	limit := a.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	total := len(entries)
	start := a.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	shown := total - start
	if shown > limit {
		shown = limit
	}

	var b strings.Builder
	for i := start; i < start+shown; i++ {
		e := entries[i]
		if e.IsDir() {
			b.WriteString(e.Name() + "/\n")
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			b.WriteString(fmt.Sprintf("%s (%d 字节)\n", e.Name(), info.Size()))
		} else {
			b.WriteString(e.Name() + "\n")
		}
	}
	text := b.String()
	if total == 0 {
		text = "(空目录)"
	} else if shown == 0 {
		text = fmt.Sprintf("(从第 %d 项起没有内容：共 %d 项，offset 超出末尾)", start, total)
	}
	nextOffset := 0
	if start+shown < total {
		nextOffset = start + shown
		text += fmt.Sprintf("... (共 %d 项，已显示第 %d-%d 项；用 offset=%d 继续，或用 dir_path 缩小范围)\n",
			total, start+1, start+shown, nextOffset)
	}
	displayInfo := map[string]any{"dir_path": display, "entries": total, "shown": shown, "offset": start}
	if nextOffset > 0 {
		displayInfo["next_offset"] = nextOffset
	}
	return agent.Result{
		Content: text,
		Display: displayInfo,
	}, nil
}

// ---------- delete ----------

type deleteArgs struct {
	FilePath string `json:"file_path"`
}

// DeleteTool 删除根目录内的一个普通文件。
//
// 删除不可逆，所以比 write/edit 多两道限制：**必须先 read 过**（观察守卫），
// 且**只删普通文件**（目录、设备一律拒绝，避免一句 delete 把整个目录树删了）。
// 路径默认限定在根目录内；AllowOutside 档放行工作区外的绝对路径（写类工具会先经确认壳）。
type DeleteTool struct{}

func (DeleteTool) Name() string { return "delete" }

func (DeleteTool) Description() string {
	return "删除工作区里的一个文件（不可逆）。删除前必须先 read 过它，否则会被拒绝；" +
		"只能删普通文件，目录一律拒绝。不确定路径时先用 list。"
}

func (DeleteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "要删除的文件路径（相对根目录或根内绝对路径）"},
		},
		"required": []string{"file_path"},
	}
}

func (DeleteTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a deleteArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}

	fi, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			recordAbsent(ex, target)
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot delete \"%s\": file does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件（目录或设备），delete 只删普通文件"}
	}

	l := lockTarget(sessionOf(ex), target)
	l.Lock()
	defer l.Unlock()

	// 观察守卫：没见过就不许删（与 write/edit 同一套“不看过不改”策略）。
	// 版本不一致同样拒绝：模型读到的是旧内容，删的可能是别人刚改过的文件。
	o := observations.get(sessionOf(ex), target)
	switch o.Kind {
	case ObsPresent:
		if versionOf(fi) != o.Version {
			return agent.Result{}, &Error{
				Code: CodeStaleVersion, Path: display,
				Reason: fmt.Sprintf("cannot delete \"%s\": the file changed on disk since it was read (expected %s, found %s)", display, o.Version, versionOf(fi)),
			}
		}
	default:
		return agent.Result{}, &Error{Code: CodeNotObserved, Path: display}
	}

	// 删除同样留检查点：恢复时按原路径重建。
	saveCheckpointForTarget(ex, target, display)
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			recordAbsent(ex, target)
			return agent.Result{}, &Error{Code: CodeNotFound, Path: display, Reason: fmt.Sprintf("cannot delete \"%s\": file does not exist", display)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	recordAbsent(ex, target)

	return agent.Result{
		Content: fmt.Sprintf("Deleted %s (%d 字节)", display, fi.Size()),
		Display: map[string]any{"file_path": display, "bytes": fi.Size(), "deleted": true},
	}, nil
}

// ---------- move（重命名 / 移动）----------

type moveArgs struct {
	FromPath string `json:"from_path"`
	ToPath   string `json:"to_path"`
}

// MoveTool 把根目录内的一个文件重命名或移动到另一个路径。
//
// 与 delete 同样受观察守卫约束（必须先 read 过源文件，且版本未变），
// 并且**不覆盖**已存在的目标：重命名到已有文件名属于不可逆操作，一律拒绝。
// 两端路径默认限定在根目录内；AllowOutside 档放行工作区外的绝对路径（写类工具会先经确认壳）。
type MoveTool struct{}

func (MoveTool) Name() string { return "move" }

func (MoveTool) Description() string {
	return "把工作区里的一个文件重命名或移动到另一个路径（目录不存在会自动创建）。" +
		"源文件必须先 read 过，否则会被拒绝；目标已存在时不覆盖，直接报错。"
}

func (MoveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"from_path": map[string]any{"type": "string", "description": "源文件路径（相对根目录或根内绝对路径）"},
			"to_path":   map[string]any{"type": "string", "description": "目标路径（相对根目录或根内绝对路径）"},
		},
		"required": []string{"from_path", "to_path"},
	}
}

func (MoveTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a moveArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	if strings.TrimSpace(a.FromPath) == "" || strings.TrimSpace(a.ToPath) == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "from_path 与 to_path 都必填"}
	}
	src, srcDisplay, err := resolveFor(ex, a.FromPath)
	if err != nil {
		return agent.Result{}, err
	}
	dst, dstDisplay, err := resolveFor(ex, a.ToPath)
	if err != nil {
		return agent.Result{}, err
	}
	if src == dst {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "from_path 与 to_path 相同，无需移动"}
	}

	fi, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			recordAbsent(ex, src)
			return agent.Result{}, &Error{Code: CodeNotFound, Path: srcDisplay, Reason: fmt.Sprintf("cannot move \"%s\": file does not exist", srcDisplay)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: srcDisplay, Reason: ioCause(err, src, srcDisplay)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "源不是普通文件（目录或设备），move 只处理普通文件"}
	}

	// 两个目标都加锁；按路径排序取锁，避免 A→B 与 B→A 并发时互锁。
	first, second := src, dst
	if second < first {
		first, second = second, first
	}
	l1 := lockTarget(sessionOf(ex), first)
	l1.Lock()
	defer l1.Unlock()
	if second != first {
		l2 := lockTarget(sessionOf(ex), second)
		l2.Lock()
		defer l2.Unlock()
	}

	// 观察守卫：源文件必须先看过，且磁盘版本没变。
	o := observations.get(sessionOf(ex), src)
	switch o.Kind {
	case ObsPresent:
		if versionOf(fi) != o.Version {
			return agent.Result{}, &Error{
				Code: CodeStaleVersion, Path: srcDisplay,
				Reason: fmt.Sprintf("cannot move \"%s\": the file changed on disk since it was read (expected %s, found %s)", srcDisplay, o.Version, versionOf(fi)),
			}
		}
	default:
		return agent.Result{}, &Error{Code: CodeNotObserved, Path: srcDisplay}
	}

	// 不覆盖已存在的目标。
	if _, err := os.Stat(dst); err == nil {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: fmt.Sprintf("cannot move to \"%s\": target already exists — pick another name or delete it first", dstDisplay)}
	} else if !os.IsNotExist(err) {
		return agent.Result{}, &Error{Code: CodeIOError, Path: dstDisplay, Reason: ioCause(err, dst, dstDisplay)}
	}

	// 目标父目录按需创建，与 write 同一套根内守卫。
	if IsWithinWorkspaces(ex.Root, ex.Roots, filepath.Dir(dst)) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return agent.Result{}, &Error{Code: CodeIOError, Path: dstDisplay, Reason: ioCause(err, dst, dstDisplay)}
		}
	}

	// 移动前按源路径留检查点；恢复源路径即可拿回内容。
	saveCheckpointForTarget(ex, src, srcDisplay)
	if err := os.Rename(src, dst); err != nil {
		return agent.Result{}, &Error{Code: CodeIOError, Path: dstDisplay, Reason: ioCause(err, dst, dstDisplay)}
	}
	recordAbsent(ex, src)
	if fi2, err2 := os.Stat(dst); err2 == nil {
		recordPresent(ex, dst, fi2)
	}

	return agent.Result{
		Content: fmt.Sprintf("Moved %s -> %s (%d 字节)", srcDisplay, dstDisplay, fi.Size()),
		Display: map[string]any{"from": srcDisplay, "to": dstDisplay, "bytes": fi.Size(), "moved": true},
	}, nil
}
