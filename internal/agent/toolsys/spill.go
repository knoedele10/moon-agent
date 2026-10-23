package toolsys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"blog-my/internal/agent"
)

const (
	// defaultSpillMaxBytes 是触发落盘的内容阈值：32KB，约 8K token。
	defaultSpillMaxBytes = 32 * 1024
	// defaultSpillHeadRunes 是落盘后回传给模型的预览预算（按 rune 计）。
	// 超过预算时按 spillPreviewHeadPercent / spillPreviewTailPercent 切成头部与尾部，合计仍是该预算。
	defaultSpillHeadRunes = 2000
	// 预览预算的头尾比例：日志类结果的关键信息经常在末尾，因此保留约 40% 尾部。
	spillPreviewHeadPercent = 60
	spillPreviewTailPercent = 40
	// readResultDefaultLines 是 read_tool_result 缺省的每次行数。
	readResultDefaultLines = 200
	// readResultMaxLines 是 read_tool_result 单次上限，防止模型一次又拉回 60KB。
	readResultMaxLines = 5000
	// spillHashLen 是文件名里内容哈希的十六进制长度。
	spillHashLen = 16
	// 单行 JSON 等结果也必须分页，避免行数上限被一条超长行绕过。
	readResultMaxRunes = 8000
)

// unsafeSessionChars 匹配文件名里不允许的字符。
var unsafeSessionChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// SpillPolicy 是「大结果落盘」策略。
//
// 超限内容写到 Dir 下的不可执行文本文件，回传内容换成「头尾预览 + 落盘路径 + 继续读方法」，
// 模型需要后文时用 ReadResultTool 分段读取，信息不会像纯截断那样丢掉。
type SpillPolicy struct {
	Dir      string // 空 → 默认数据目录/spill
	MaxBytes int    // 超过多少字节触发落盘；<=0 → 32KB
	// HeadRunes 是回传预览的 rune 预算，不是只保留头部。
	// <=0 时用 2000。内容超过预算时约 60% 取头部、40% 取尾部，两者之和等于预算，
	// 中间插入省略标记；不超过预算则整段放入预览，不改写。
	HeadRunes int
}

// normalized 补齐零值默认值。
func (p SpillPolicy) normalized() SpillPolicy {
	if p.MaxBytes <= 0 {
		p.MaxBytes = defaultSpillMaxBytes
	}
	if p.HeadRunes <= 0 {
		p.HeadRunes = defaultSpillHeadRunes
	}
	return p
}

// Wrap 返回一个「结果超限自动落盘」的工具包装。
//
// 落盘失败不会让工具调用变红：退化成同样的头尾预览并在文案里说明原因，
// 以免因为磁盘问题把一次正常调用报成错误。
func (p SpillPolicy) Wrap(t agent.Tool) agent.Tool {
	if t == nil {
		return nil
	}
	return spillTool{inner: t, policy: p.normalized()}
}

// spillTool 是 SpillPolicy.Wrap 的包装类型。
type spillTool struct {
	inner  agent.Tool
	policy SpillPolicy
}

func (s spillTool) Name() string               { return s.inner.Name() }
func (s spillTool) Description() string        { return s.inner.Description() }
func (s spillTool) Parameters() map[string]any { return s.inner.Parameters() }

func (s spillTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	res, err := s.inner.Execute(ctx, raw, ex)
	if err != nil {
		return res, err
	}
	total := len(res.Content)
	if total <= s.policy.MaxBytes {
		return res, nil
	}
	dir, err := spillDir(s.policy)
	if err != nil {
		res.Content = spillPreview(res.Content, s.policy.HeadRunes) +
			fmt.Sprintf("\n\n【结果过长且落盘失败：%v】完整内容未保留，请缩小调用范围后重试。", err)
		return res, nil
	}
	// 哈希与落盘必须用全文；预览只替换回传给模型的 Content。
	path, err := writeSpillFile(dir, sessionOf(ex), res.Content)
	if err != nil {
		res.Content = spillPreview(res.Content, s.policy.HeadRunes) +
			fmt.Sprintf("\n\n【结果过长且落盘失败：%v】完整内容未保留，请缩小调用范围后重试。", err)
		return res, nil
	}
	res.Content = spillPreview(res.Content, s.policy.HeadRunes) + spillNotice(total, path)
	if res.Display == nil {
		res.Display = map[string]any{}
	}
	res.Display["spilled"] = true
	res.Display["spill_path"] = path
	res.Display["spill_bytes"] = total
	return res, nil
}

// spillNotice 是落盘后拼在预览后面的提示，必须让模型知道怎么读回后文。
func spillNotice(total int, path string) string {
	return fmt.Sprintf(
		"\n\n【结果已落盘】原始结果共 %d 字节，超过单次回传上限，已完整保存到：\n%s\n"+
			"需要后面的内容请调用工具 read_tool_result，参数：file_path=%q，offset=起始行号（从 1 开始），limit=行数（默认 %d）。\n"+
			"不要把上面的头部当成完整结果。",
		total, path, path, readResultDefaultLines)
}

// spillPreview 按预算生成模型可见预览。
//
// budget 即 HeadRunes。内容的 rune 数不超过预算时原样返回（不插入省略标记，
// 也不把非法 UTF-8 重编码）。超过预算时头部约占 60%、尾部约占 40%，
// 两者之和等于预算，中间用省略标记隔开，避免两段被当成连续原文。
func spillPreview(content string, budget int) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(content)
	if len(runes) <= budget {
		return content
	}
	headN, tailN := splitPreviewBudget(budget)
	var b strings.Builder
	if headN > 0 {
		b.WriteString(string(runes[:headN]))
	}
	omitted := len(runes) - headN - tailN
	b.WriteString(spillPreviewGap(omitted))
	if tailN > 0 {
		b.WriteString(string(runes[len(runes)-tailN:]))
	}
	return b.String()
}

// splitPreviewBudget 把预览预算分成头部与尾部的 rune 数，二者之和等于 budget。
// 头部约为 60%，尾部约为 40%；budget>=2 时两边至少各 1 个 rune。
func splitPreviewBudget(budget int) (headN, tailN int) {
	if budget <= 0 {
		return 0, 0
	}
	headN = budget * spillPreviewHeadPercent / (spillPreviewHeadPercent + spillPreviewTailPercent)
	tailN = budget - headN
	if budget >= 2 {
		if headN < 1 {
			headN = 1
			tailN = budget - headN
		}
		if tailN < 1 {
			tailN = 1
			headN = budget - tailN
		}
	}
	return headN, tailN
}

// spillPreviewGap 隔开头尾。omitted 是被省略的 rune 数。
func spillPreviewGap(omitted int) string {
	return fmt.Sprintf("\n\n…【中间省略 %d 个字符】…\n\n", omitted)
}

// spillDir 解析并创建落盘目录（权限 0700）。
func spillDir(p SpillPolicy) (string, error) {
	dir := strings.TrimSpace(p.Dir)
	if dir == "" {
		dir = defaultSpillDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("落盘目录不合法：%w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("无法创建落盘目录：%w", err)
	}
	// MkdirAll 对已存在的目录不改权限，这里显式收紧到 0700（Windows 上是只读位，无害）。
	if err := os.Chmod(abs, 0o700); err != nil && !os.IsNotExist(err) {
		// 目录已经建好，chmod 失败不阻断落盘（部分文件系统不支持）。
		_ = err
	}
	return abs, nil
}

// defaultSpillDir 返回默认落盘目录：用户配置目录/blog-my/spill。
func defaultSpillDir() string {
	if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "blog-my", "spill")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".blog-my", "spill")
	}
	return filepath.Join(os.TempDir(), "blog-my-spill")
}

// writeSpillFile 把内容写进 dir，文件名含 session 与内容哈希。
//
// 同一份内容重复落盘直接复用（按内容哈希命中已有文件），不重复占磁盘；
// 写入走「临时文件 + rename」，避免并发工具调用时读到写了一半的文件。
func writeSpillFile(dir, session, content string) (string, error) {
	name := spillFileName(session, content)
	final := filepath.Join(dir, name)
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	tmp, err := os.CreateTemp(dir, ".spill-*")
	if err != nil {
		return "", fmt.Errorf("无法创建临时文件：%w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("无法设置落盘文件权限：%w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		cleanup()
		return "", fmt.Errorf("写入落盘文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("关闭落盘文件失败：%w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		// 并发写入同一份内容时目标可能刚好被另一个 goroutine 建好：复用即可。
		if _, statErr := os.Stat(final); statErr == nil {
			return final, nil
		}
		return "", fmt.Errorf("保存落盘文件失败：%w", err)
	}
	return final, nil
}

// spillFileName 由 session 与内容哈希组成稳定文件名。
func spillFileName(session, content string) string {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])[:spillHashLen]
	sid := spillSessionPart(session)
	return sid + "-" + hash + ".txt"
}

// 清洗会把不同会话压成同名，因此总是附上原始会话哈希，
// 也防止攻击者把另一会话清洗后的名字直接当作自己的会话名。
func spillSessionPart(session string) string {
	sid := sanitizeFilePart(session)
	if sid == "" {
		sid = "nosession"
	}
	if session != "" {
		sum := sha256.Sum256([]byte(session))
		sid += "-" + hex.EncodeToString(sum[:])[:spillHashLen]
	}
	return sid
}

func ownsSpill(path, session string) bool {
	tail, ok := strings.CutPrefix(filepath.Base(path), spillSessionPart(session)+"-")
	if !ok || !strings.HasSuffix(tail, ".txt") || len(tail) != spillHashLen+4 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(tail, ".txt"))
	return err == nil
}

// sanitizeFilePart 把任意 session id 变成安全文件名片段。
func sanitizeFilePart(session string) string {
	part := unsafeSessionChars.ReplaceAllString(strings.TrimSpace(session), "_")
	part = strings.Trim(part, "._-")
	runes := []rune(part)
	if len(runes) > 48 {
		part = string(runes[:48])
	}
	return part
}

// sessionOf 取执行上下文的 session id（可能为空）。
func sessionOf(ex *agent.Exec) string {
	if ex == nil {
		return ""
	}
	return ex.SessionID
}

// ReadResultTool 返回 read_tool_result（file_path, offset?, limit?）工具。
//
// 它只允许读落盘目录内的文件：模型不能拿它绕过文件沙箱读任意路径。
// offset 是 1 起的行号，limit 缺省 200 行、上限 5000 行。
func ReadResultTool(p SpillPolicy) agent.Tool {
	return readResultTool{policy: p.normalized()}
}

// readResultTool 是 read_tool_result 的实现。
type readResultTool struct {
	policy SpillPolicy
}

// Name 返回工具名。
func (r readResultTool) Name() string { return "read_tool_result" }

// Description 返回工具描述（中文，写给模型看）。
func (r readResultTool) Description() string {
	return "读取之前因内容过长而落盘的工具结果。只在某条工具结果里出现「结果已落盘」时使用；" +
		"file_path 用那条提示里给出的路径，offset 是起始行号（从 1 开始），limit 是行数（默认 200，上限 5000）。" +
		"超长行会分段返回；继续读取时同时传回结果里的 next_offset 和 next_char_offset，后者填入 char_offset。" +
		"不要用它读其它文件，普通文件请用 read 工具。"
}

// Parameters 返回 JSON Schema。
func (r readResultTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": "落盘结果的完整路径（来自「结果已落盘」提示）",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "起始行号，从 1 开始；缺省 1",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("最多返回多少行；缺省 %d，上限 %d", readResultDefaultLines, readResultMaxLines),
			},
			"char_offset": map[string]any{
				"type":        "integer",
				"description": "起始行内跳过的字符数，默认 0；续读超长行时填上一页的 next_char_offset",
			},
		},
		"required": []string{"file_path"},
	}
}

// readResultArgs 是 read_tool_result 的参数。
type readResultArgs struct {
	FilePath   string `json:"file_path"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	CharOffset int    `json:"char_offset"`
}

// Execute 读取落盘文件的一个行区间。
func (r readResultTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var args readResultArgs
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return failure("SPILL_BAD_ARGS", "参数不是合法 JSON："+err.Error()), nil
		}
	}
	path := strings.TrimSpace(args.FilePath)
	if path == "" {
		return failure("SPILL_PATH_MISSING", "缺少必填参数 \"file_path\"：请填「结果已落盘」提示里给出的完整路径。"), nil
	}
	dir, err := spillDir(r.policy)
	if err != nil {
		return failure("SPILL_DIR_UNAVAILABLE", "落盘目录不可用："+err.Error()), nil
	}
	target, err := resolveSpillTarget(dir, path)
	if err != nil {
		return failure("SPILL_PATH_DENIED", err.Error()), nil
	}
	if _, err := os.Stat(target); err == nil && !ownsSpill(target, sessionOf(ex)) {
		return failure("SPILL_SESSION_DENIED", "只能读取当前会话生成的落盘结果，请使用本会话工具结果中给出的路径。"), nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return failure("SPILL_NOT_FOUND",
				"落盘结果不存在或已被清理："+target+"。请重新调用原来的工具生成结果。"), nil
		}
		return failure("SPILL_READ_FAILED", "读取落盘结果失败："+err.Error()), nil
	}
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = readResultDefaultLines
	}
	if limit > readResultMaxLines {
		limit = readResultMaxLines
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	totalLines := len(lines)
	if offset > totalLines {
		return agent.Result{
			Content: fmt.Sprintf("文件：%s（共 %d 行）。offset=%d 已超出文件末尾，没有可显示的内容。\n"+
				"请把 offset 改回 1 到 %d 之间的行号再试。", target, totalLines, offset, totalLines),
			Display: map[string]any{
				"code": "SPILL_OFFSET_OUT_OF_RANGE", "path": target,
				"offset": offset, "total_lines": totalLines, "total_bytes": len(data),
			},
		}, nil
	}
	end := offset - 1 + limit
	if end > totalLines {
		end = totalLines
	}
	charOffset := args.CharOffset
	firstLineRunes := len([]rune(lines[offset-1]))
	if charOffset < 0 || charOffset > firstLineRunes {
		return failure("SPILL_CHAR_OFFSET_OUT_OF_RANGE", fmt.Sprintf("char_offset 应在 0 到 %d 之间，请使用上一页给出的 next_char_offset。", firstLineRunes)), nil
	}
	bodyRunes := []rune(strings.Join(lines[offset-1:end], "\n"))
	bodyRunes = bodyRunes[charOffset:]
	nextOffset, nextCharOffset := end+1, 0
	partial := len(bodyRunes) > readResultMaxRunes
	if partial {
		bodyRunes = bodyRunes[:readResultMaxRunes]
		nextOffset = offset
		nextCharOffset = charOffset
		for _, ch := range bodyRunes {
			if ch == '\n' {
				nextOffset++
				nextCharOffset = 0
			} else {
				nextCharOffset++
			}
		}
	}
	body := string(bodyRunes)
	header := fmt.Sprintf("文件：%s（共 %d 行、%d 字节）；本次显示第 %d–%d 行。\n\n",
		target, totalLines, len(data), offset, end)
	footer := ""
	if partial {
		footer = fmt.Sprintf("\n\n（本页达到字符上限；继续读取请设置 offset=%d，char_offset=%d，内容仍完整保存在原文件中。）", nextOffset, nextCharOffset)
	} else if end < totalLines {
		footer = fmt.Sprintf("\n\n（还有 %d 行未显示；继续读取请把 offset 设为 %d。）", totalLines-end, end+1)
	}
	return agent.Result{
		Content: header + body + footer,
		Display: map[string]any{
			"code":             "SPILL_READ_OK",
			"path":             target,
			"offset":           offset,
			"limit":            limit,
			"total_lines":      totalLines,
			"total_bytes":      len(data),
			"next_offset":      nextOffset,
			"next_char_offset": nextCharOffset,
		},
	}, nil
}

// resolveSpillTarget 把请求路径解析成落盘目录内的真实路径；越界一律拒绝。
func resolveSpillTarget(dir, path string) (string, error) {
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("落盘路径不合法：%w", err)
	}
	// 解析符号链接后再判断，防止用链接逃出落盘目录。
	if real, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = real
	}
	realDir := dir
	if resolved, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		realDir = resolved
	}
	if !pathWithin(realDir, abs) {
		return "", fmt.Errorf("只能读取落盘结果目录内的文件（%s），收到：%s。"+
			"普通文件请改用 read 工具；确实需要重新读取原结果时，请重新调用生成它的工具。", realDir, path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// 交给 Execute 的 ReadFile 统一报 SPILL_NOT_FOUND，文案更准确。
			return abs, nil
		}
		return "", fmt.Errorf("无法访问落盘路径：%w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("落盘路径是一个目录，不是结果文件：%s。", abs)
	}
	return abs, nil
}

// pathWithin 判断 target 是否在 dir 内（dir 自身不算）。
func pathWithin(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// failure 构造一条 is_error 的工具结果（错误码进 Display["code"]）。
func failure(code, message string) agent.Result {
	return agent.Result{
		IsError: true,
		Content: message,
		Display: map[string]any{"code": code},
	}
}
