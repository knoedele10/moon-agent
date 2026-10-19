package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"blog-my/internal/agent"
)

// OfficeEditTool 新建 / 修改 / 另存 Office 文档（docx / xlsx / pptx）。
//
// 与 write/edit 共用同一套安全约束：路径必须落在工作区内、覆盖已存在的文件前必须先 read。
// 与 write 的差别是它操作的是**整包 OOXML**，而不是纯文本字节。
type OfficeEditTool struct{}

func (OfficeEditTool) Name() string { return "office_edit" }

func (OfficeEditTool) Description() string {
	return "新建 / 修改 / 另存 Office 文档。" +
		"action=create 新建 docx 或 xlsx（pptx 不支持从零新建，请先用 PowerPoint 建模板）；" +
		"action=replace 在已有 docx/xlsx/pptx 里做字面量替换；" +
		"action=save_as 整包另存到新路径。覆盖已有文件前必须先 read。"
}

func (OfficeEditTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":    map[string]any{"type": "string", "enum": []string{"create", "replace", "save_as"}},
			"file_path": map[string]any{"type": "string", "description": "目标文件路径（相对工作区）"},
			"kind":      map[string]any{"type": "string", "enum": []string{"docx", "xlsx", "pptx"}, "description": "create 时的文档类型"},
			"title":     map[string]any{"type": "string", "description": "create docx 时的标题"},
			// pptx 只有在装了 OfficeCLI（拿它的骨架）时才支持新建。
			// 枚举里保留它，具体是否可用由执行时判断并给出替代建议。
			"lines":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "create docx 时的段落"},
			"slides":       map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "create pptx 时的页面：[{title, bullets:[...]}]，需本机装有 OfficeCLI"},
			"sheets":       map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "create xlsx 时的工作表：[{name, rows:[[\"a\",\"b\"]]}]"},
			"find":         map[string]any{"type": "string", "description": "replace 时要找的原文"},
			"replace_text": map[string]any{"type": "string", "description": "replace 时替换成什么"},
			"replace_all":  map[string]any{"type": "boolean", "description": "replace 时是否替换全部（默认只换第一处）"},
			"to_path":      map[string]any{"type": "string", "description": "save_as 的目标路径"},
		},
		"required": []string{"action", "file_path"},
	}
}

type officeArgs struct {
	Action      string           `json:"action"`
	FilePath    string           `json:"file_path"`
	Kind        string           `json:"kind"`
	Title       string           `json:"title"`
	Lines       []string         `json:"lines"`
	Sheets      []map[string]any `json:"sheets"`
	Slides      []map[string]any `json:"slides"`
	Find        string           `json:"find"`
	ReplaceText string           `json:"replace_text"`
	ReplaceAll  bool             `json:"replace_all"`
	ToPath      string           `json:"to_path"`
}

func (OfficeEditTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if err := guard(ctx, ex); err != nil {
		return agent.Result{}, err
	}
	var a officeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agent.Result{}, err
	}
	target, display, err := resolveFor(ex, a.FilePath)
	if err != nil {
		return agent.Result{}, err
	}

	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "create":
		return officeCreate(ex, target, display, a)
	case "replace":
		return officeReplace(ex, target, display, a)
	case "save_as":
		return officeSaveAs(ex, target, display, a)
	}
	return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "action 只能是 create / replace / save_as"}
}

// officeCreate 新建 docx / xlsx。
func officeCreate(ex *agent.Exec, target, display string, a officeArgs) (agent.Result, error) {
	kind := normalizeCreateKind(a.Kind, target)
	// pptx 走 OfficeCLI：先用它的 create 拿到合法骨架（母版/版式/主题齐全），
	// 再由我们注入幻灯片——它这个版本的 add slide 报成功却不落盘，不可用。
	if kind == "pptx" && OfficeCLIPath() == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs,
			Reason: "新建 pptx 需要本机装有 OfficeCLI（用来生成合法的母版/版式骨架）；没装的话请先用 PowerPoint/WPS 建一个空白的 .pptx，再用 replace 改文字"}
	}
	if !officeCreateKind(kind) {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "kind 只能是 docx 或 xlsx"}
	}

	var (
		data []byte
		err  error
	)
	switch kind {
	case "docx":
		data, err = CreateDocx(a.Title, a.Lines)
	case "xlsx":
		sheets := make([]SheetInput, 0, len(a.Sheets))
		for i, s := range a.Sheets {
			name, _ := s["name"].(string)
			var rows [][]string
			if rawRows, ok := s["rows"].([]any); ok {
				for _, r := range rawRows {
					rowAny, ok := r.([]any)
					if !ok {
						continue
					}
					row := make([]string, 0, len(rowAny))
					for _, cell := range rowAny {
						row = append(row, fmt.Sprint(cell))
					}
					rows = append(rows, row)
				}
			}
			if name == "" {
				name = fmt.Sprintf("Sheet%d", i+1)
			}
			sheets = append(sheets, SheetInput{Name: name, Rows: rows})
		}
		if len(sheets) == 0 {
			sheets = []SheetInput{{Name: "Sheet1"}}
		}
		data, err = CreateXlsx(sheets)
	case "pptx":
		data, err = createPptxWithOfficeCLI(a)
	}
	if err != nil {
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: err.Error()}
	}

	l := lockTarget(sessionOf(ex), target)
	l.Lock()
	defer l.Unlock()

	// 与 write 同一套规则：没读过的文件不允许被覆盖。
	o := observations.get(sessionOf(ex), target)
	createIfAbsent := o.Kind != ObsPresent
	if o.Kind == ObsPresent {
		if fi, serr := os.Stat(target); serr == nil {
			if versionOf(fi) != o.Version {
				return agent.Result{}, &Error{Code: CodeStaleVersion, Path: display,
					Reason: fmt.Sprintf("cannot create \"%s\": the file changed on disk since it was read", display)}
			}
			createIfAbsent = false
		}
	}

	// 父目录不存在就按需创建，与 write 同一套根内守卫。
	// 这一步必须在写之前：writeFileAtomic 是在目标父目录里建临时文件，
	// 目录不存在会直接报 “cannot find the path specified”。
	if createIfAbsent {
		if IsWithinWorkspaces(ex.Root, ex.Roots, filepath.Dir(target)) {
			if merr := os.MkdirAll(filepath.Dir(target), 0o755); merr != nil {
				return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(merr, target, display)}
			}
		}
	}

	ver, created, werr := writeFileAtomic(target, display, data, createIfAbsent)
	if werr != nil {
		return agent.Result{}, werr
	}
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: ver})

	// 装了 OfficeCLI 就用它校验一遍 OpenXML——这是“我们自己注入内容”后的唯一客观检查。
	// 校验不过就删掉文件并如实报错，绝不把可疑的文档留在用户的目录里。
	if out, verr := OfficeCLIValidate(context.Background(), target); verr != nil {
		_ = os.Remove(target)
		recordAbsent(ex, target)
		return agent.Result{}, &Error{Code: CodeIOError, Path: display,
			Reason: "生成的文件没有通过 OpenXML 校验，已删除；" + strings.TrimSpace(out)}
	}

	verb := "Overwrote"
	if created {
		verb = "Created"
	}
	return agent.Result{
		Content: fmt.Sprintf("%s %s（%s，%d 字节）", verb, display, kind, len(data)),
		Display: map[string]any{"file_path": display, "kind": kind, "created": created, "bytes": len(data), "version": ver},
	}, nil
}

// officeReplace 在已有文档里做字面量替换。
func officeReplace(ex *agent.Exec, target, display string, a officeArgs) (agent.Result, error) {
	if !officeEditable(target) {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "replace 只支持 docx / xlsx / pptx"}
	}
	if strings.TrimSpace(a.Find) == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "find 不能为空"}
	}

	l := lockTarget(sessionOf(ex), target)
	l.Lock()
	defer l.Unlock()

	fi, err := os.Stat(target)
	if err != nil {
		recordAbsent(ex, target)
		return agent.Result{}, &Error{Code: CodeNotFound, Path: display,
			Reason: fmt.Sprintf("cannot edit \"%s\": file does not exist — create it first", display)}
	}
	if !fi.Mode().IsRegular() {
		return agent.Result{}, &Error{Code: CodeSandboxDenied, Reason: "目标不是普通文件"}
	}

	// 与 edit 一致：必须读过，且版本没变，否则可能改在别人刚改过的内容上。
	o := observations.get(sessionOf(ex), target)
	if o.Kind != ObsPresent {
		return agent.Result{}, &Error{Code: CodeNotObserved, Path: display}
	}
	if versionOf(fi) != o.Version {
		return agent.Result{}, &Error{Code: CodeStaleVersion, Path: display,
			Reason: fmt.Sprintf("cannot edit \"%s\": the file changed on disk since it was read", display)}
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return agent.Result{}, &Error{Code: CodeIOError, Path: display, Reason: ioCause(err, target, display)}
	}
	newData, n, rerr := ReplaceInOfficeZip(data, a.Find, a.ReplaceText, a.ReplaceAll)
	if rerr != nil {
		return agent.Result{}, &Error{Code: CodeNoMatch, Path: display, Reason: rerr.Error()}
	}

	ver, _, werr := writeFileAtomic(target, display, newData, false)
	if werr != nil {
		return agent.Result{}, werr
	}
	observations.set(sessionOf(ex), target, Observation{Kind: ObsPresent, Version: ver})

	return agent.Result{
		Content: fmt.Sprintf("Edited %s (%d 处替换)", display, n),
		Display: map[string]any{"file_path": display, "replacements": n, "bytes": len(newData), "version": ver},
	}, nil
}

// officeSaveAs 整包另存。
func officeSaveAs(ex *agent.Exec, src, srcDisplay string, a officeArgs) (agent.Result, error) {
	if !officeEditable(src) {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "save_as 的源文件必须是 docx / xlsx / pptx"}
	}
	if strings.TrimSpace(a.ToPath) == "" {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "to_path 不能为空"}
	}
	dst, dstDisplay, err := resolveFor(ex, a.ToPath)
	if err != nil {
		return agent.Result{}, err
	}
	if dst == src {
		return agent.Result{}, &Error{Code: CodeInvalidArgs, Reason: "to_path 与 file_path 相同，无需另存"}
	}

	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.Result{}, &Error{Code: CodeNotFound, Path: srcDisplay,
				Reason: fmt.Sprintf("cannot save_as \"%s\": source does not exist", srcDisplay)}
		}
		return agent.Result{}, &Error{Code: CodeIOError, Path: srcDisplay, Reason: ioCause(err, src, srcDisplay)}
	}

	l := lockTarget(sessionOf(ex), dst)
	l.Lock()
	defer l.Unlock()

	// 不覆盖已存在的目标（与 move 同一策略：写坏别人文件是最难查的事故）。
	if _, serr := os.Stat(dst); serr == nil {
		return agent.Result{}, &Error{Code: CodeInvalidArgs,
			Reason: fmt.Sprintf("cannot save_as \"%s\": target already exists — pick another name", dstDisplay)}
	}

	if IsWithinWorkspaces(ex.Root, ex.Roots, filepath.Dir(dst)) {
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			return agent.Result{}, &Error{Code: CodeIOError, Path: dstDisplay, Reason: ioCause(merr, dst, dstDisplay)}
		}
	}

	ver, _, werr := writeFileAtomic(dst, dstDisplay, data, true)
	if werr != nil {
		return agent.Result{}, werr
	}
	observations.set(sessionOf(ex), dst, Observation{Kind: ObsPresent, Version: ver})

	return agent.Result{
		Content: fmt.Sprintf("Saved %s -> %s (%d 字节)", srcDisplay, dstDisplay, len(data)),
		Display: map[string]any{"from": srcDisplay, "to": dstDisplay, "bytes": len(data), "version": ver},
	}, nil
}
