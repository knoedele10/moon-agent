package evolve

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SkillItem 是一条技能在进化层眼里的样子。
//
// 刻意只保留 lint/去重/固化需要的字段：本包不认识数据库模型，web 层负责
// 把 model.AgentSkill 映射过来，避免 evolve 反向依赖 model。
type SkillItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Source      string `json:"source,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// SkillStore 是技能库接口，由 web 层用数据库实现。
type SkillStore interface {
	// List 列出某个用户的全部技能（含未启用）。
	List(userID string) ([]SkillItem, error)
	// Get 取一条技能；第二个返回值表示是否存在。
	Get(userID, name string) (SkillItem, bool, error)
	// Upsert 写入一条技能；created 表示是新建而不是覆盖。
	Upsert(userID string, it SkillItem) (created bool, err error)
}

// Issue 是一条体检问题。Severity 取值：warning | error。
type Issue struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

const (
	// SeverityWarning 是建议性问题：可以工作，但质量会打折。
	SeverityWarning = "warning"
	// SeverityError 是必须修的问题：危险内容或结构根本不成立，block 档会拦下。
	SeverityError = "error"
	// skillBodyMaxBytes 与 web 层 skill_write 的上限保持一致（64KB）。
	skillBodyMaxBytes = 64 * 1024
	// skillDescMinRunes / skillDescMaxRunes 是描述长度区间（契约 §4：20–200 字）。
	skillDescMinRunes = 20
	skillDescMaxRunes = 200
	// skillNameMaxRunes 与数据库列宽留余量（提示词里要列出来，必须短）。
	skillNameMaxRunes = 64
)

var (
	// skillNameRe 是技能名 slug 规则：小写字母/数字开头，允许连字符与下划线。
	skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// 正文必须能回答「什么时候用」，下面是可接受的章节/短语标记。
	whenUseMarkers = []string{"何时使用", "什么时候用", "使用场景", "when to use"}
	// 正文必须能回答「怎么做」。
	howToMarkers = []string{"怎么做", "怎么用", "步骤", "用法", "how to", "steps"}
)

// HasError 判断一组体检结果里是否有 error 级问题（block 档与写前校验用）。
func HasError(issues []Issue) bool {
	for _, it := range issues {
		if it.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Lint 对一条技能做结构 + 安全检查。
//
// 返回顺序稳定：先结构（名字、描述、正文大小、章节），后危险/密钥。
// 注意：Lint 已经包含 Dangerous 的全部检查，调用方不需要再单独调一次。
func Lint(it SkillItem) []Issue {
	issues := make([]Issue, 0, 4)
	name := strings.TrimSpace(it.Name)
	switch {
	case name == "":
		issues = append(issues, Issue{Code: "name_empty", Message: "技能名为空。", Severity: SeverityError})
	case !skillNameRe.MatchString(name):
		issues = append(issues, Issue{
			Code:     "name_invalid",
			Message:  fmt.Sprintf("技能名 %q 不合法：只能用小写字母、数字、连字符和下划线，且以字母或数字开头。", name),
			Severity: SeverityError,
		})
	case len([]rune(name)) > skillNameMaxRunes:
		issues = append(issues, Issue{
			Code:     "name_too_long",
			Message:  fmt.Sprintf("技能名 %d 字，超过上限 %d 字。", len([]rune(name)), skillNameMaxRunes),
			Severity: SeverityError,
		})
	}

	descRunes := utf8.RuneCountInString(strings.TrimSpace(it.Description))
	if descRunes < skillDescMinRunes {
		issues = append(issues, Issue{
			Code:     "description_short",
			Message:  fmt.Sprintf("描述只有 %d 字，太短：写清「它能做什么、什么时候用」（建议 %d–%d 字）。", descRunes, skillDescMinRunes, skillDescMaxRunes),
			Severity: SeverityWarning,
		})
	}
	if descRunes > skillDescMaxRunes {
		issues = append(issues, Issue{
			Code:     "description_long",
			Message:  fmt.Sprintf("描述 %d 字，超过 %d 字上限：提示词里的技能索引会被截断，路由信号会丢。", descRunes, skillDescMaxRunes),
			Severity: SeverityWarning,
		})
	}

	body := it.Body
	if strings.TrimSpace(body) == "" {
		issues = append(issues, Issue{Code: "body_empty", Message: "技能正文为空。", Severity: SeverityError})
	} else if len(body) > skillBodyMaxBytes {
		issues = append(issues, Issue{
			Code:     "body_too_large",
			Message:  fmt.Sprintf("技能正文 %d 字节，超过上限 %d 字节：正文会整段进上下文，必须瘦身。", len(body), skillBodyMaxBytes),
			Severity: SeverityError,
		})
	} else {
		lower := strings.ToLower(body)
		if !containsAny(lower, whenUseMarkers) {
			issues = append(issues, Issue{
				Code:     "missing_when_to_use",
				Message:  "正文缺少「何时使用」章节：没有触发条件的技能，模型不知道该在什么时候读它。",
				Severity: SeverityWarning,
			})
		}
		if !containsAny(lower, howToMarkers) {
			issues = append(issues, Issue{
				Code:     "missing_how_to",
				Message:  "正文缺少「怎么做」章节：技能要给出可照抄的步骤，不是背景介绍。",
				Severity: SeverityWarning,
			})
		}
	}

	issues = append(issues, Dangerous(body)...)
	return issues
}

// containsAny 判断文本是否含任一标记（文本调用方已转小写）。
func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// 危险命令模式：命中任意一条就是 error。
// 只放「一眼就是灾难」的模式，避免误伤正常示例（技能正文里经常要写命令）。
var dangerousPatterns = []struct {
	code string
	desc string
	re   *regexp.Regexp
}{
	{"dangerous_delete_root", "含删除根目录的危险命令（rm -rf /）", regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f[a-z]*\s+/(?:\s|$|["'` + "`" + `])`)},
	{"dangerous_format", "含格式化磁盘/分区的危险命令（mkfs / format X:）", regexp.MustCompile(`(?i)\bmkfs(?:\.\w+)?\b|\bformat\s+[a-z]:`)},
	{"dangerous_curl_pipe", "含 curl/wget 直接管道给 shell 的危险命令（curl … | sh）", regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|k|da)?sh\b`)},
	{"dangerous_dd", "含向设备写零的危险命令（dd if=/dev/zero of=/dev/…）", regexp.MustCompile(`(?i)\bdd\s+if=/dev/(?:zero|urandom)\s+of=/dev/`)},
	{"dangerous_chmod", "含递归 777 改根目录权限的危险命令（chmod -R 777 /）", regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\s+/(?:\s|$)`)},
	{"dangerous_fork_bomb", "含 fork 炸弹（:(){ :|:& };:）", regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`)},
	{"dangerous_shutdown", "含关机/重启命令", regexp.MustCompile(`(?i)\b(?:shutdown|reboot)\s+(?:-h|now|/s)`)},
	{"dangerous_windows_wipe", "含 Windows 全盘删除命令（del /s /q C:\\ 或 Remove-Item -Recurse -Force C:\\）", regexp.MustCompile(`(?i)\b(?:del|Remove-Item)\b[^\n]*(?:/s|/f|-Recurse|-Force)[^\n]*[A-Za-z]:\\`)},
	{"dangerous_diskpart", "含 diskpart clean / 清空磁盘命令", regexp.MustCompile(`(?i)\bdiskpart\b[^\n]*\bclean\b`)},
}

// 疑似密钥模式：正文里出现真实凭证就是 error（技能会被所有用户共享，泄漏面更大）。
var secretPatterns = []struct {
	code string
	desc string
	re   *regexp.Regexp
}{
	{"secret_private_key", "含私钥内容（-----BEGIN … PRIVATE KEY-----）", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"secret_aws_key", "含疑似 AWS Access Key（AKIA…）", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"secret_openai_key", "含疑似 OpenAI 风格密钥（sk-…）", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"secret_github_token", "含疑似 GitHub Token（ghp_/gho_/ghu_/ghs_/ghr_…）", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{"secret_slack_token", "含疑似 Slack Token（xox…-…）", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"secret_assignment", "含形如 password=/api_key=/token= 的明文凭证赋值", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token)\b\s*[:=]\s*["']?[A-Za-z0-9_\-+/=]{8,}`)},
}

// Dangerous 检查正文里的危险命令与疑似凭证泄漏。
//
// 独立导出是为了让调用方能在「只改一段正文」时单独体检，不用整条重跑 Lint。
func Dangerous(body string) []Issue {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	issues := make([]Issue, 0, 2)
	for _, p := range dangerousPatterns {
		if p.re.MatchString(body) {
			issues = append(issues, Issue{Code: p.code, Message: p.desc, Severity: SeverityError})
		}
	}
	for _, p := range secretPatterns {
		if p.re.MatchString(body) {
			issues = append(issues, Issue{Code: p.code, Message: p.desc, Severity: SeverityError})
		}
	}
	return issues
}

// Duplicates 用 Jaccard 相似度找内容雷同的技能对，返回按字典序稳定的技能名对。
//
// threshold <= 0 时取 0.8。token 以字母/数字为界切分，中文按单字切，
// 这样中文技能也能有可用的相似度信号（不需要分词器，也不引入依赖）。
func Duplicates(items []SkillItem, threshold float64) [][2]string {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	tokenSets := make([]map[string]struct{}, len(items))
	for i, it := range items {
		tokenSets[i] = textTokens(it.Name + "\n" + it.Description + "\n" + it.Body)
	}
	out := make([][2]string, 0)
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if jaccard(tokenSets[i], tokenSets[j]) >= threshold {
				out = append(out, [2]string{items[i].Name, items[j].Name})
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a][0] != out[b][0] {
			return out[a][0] < out[b][0]
		}
		return out[a][1] < out[b][1]
	})
	return out
}

// textTokens 把文本切成 token 集合（小写 ASCII 词 + 中文单字）。
func textTokens(s string) map[string]struct{} {
	set := make(map[string]struct{}, len(s)/2+1)
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			set[b.String()] = struct{}{}
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// 非 ASCII 文字（中文/日文等）按单字成 token。
			flush()
			set[string(r)] = struct{}{}
		default:
			flush()
		}
	}
	flush()
	return set
}

// jaccard 计算两个集合的 Jaccard 相似度；任一为空集时为 0。
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
