package content

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"blog-my/internal/model"
)

// 本文件是「自动任务」里不碰数据库的那一半：成品包渲染、文件名归一、重试退避。
//
// 单独拆出来有两个原因：
//   - 渲染和退避是纯函数，测试不需要 MySQL（见 job_test.go）；
//   - 真正落盘的渠道（本地导出 / 将来的对象存储 / 平台草稿箱）只由 Publisher 接口决定，
//     runner 不关心成品包去哪，换实现不用改调度逻辑。

// Publisher 把一份导出结果「发出去」。
//
// 默认实现 PackagePublisher 只是写成本地 Markdown 成品包；项目明确不做真实平台发帖
// （各平台无公开写接口且有风控），所以接口留在这里是为了以后接「平台草稿箱 / 对象存储」
// 时不用动 runner。
type Publisher interface {
	// Publish 返回成品包的落点（本地绝对路径或 URL），失败返回 error。
	Publish(ctx context.Context, e Export, pub model.ContentPublication) (string, error)
}

// RenderPackage 把导出结果渲染成「可直接粘贴」的 Markdown 成品包。
//
// 结构固定为：标题 → 元信息（渠道/计划时间/稿件/标签/导出时间）→ 正文 → 素材清单。
// 元信息用引用块，粘贴到公众号/知乎时不会混进正文；素材清单给出 file_key 或 url，
// 方便一条条上传。
func RenderPackage(e Export, pub model.ContentPublication, now time.Time) string {
	var b strings.Builder

	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = strings.TrimSpace(e.Draft.Title)
	}
	if title == "" {
		title = "未命名稿件"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "> 渠道：`%s`　计划时间：%s　稿件：#%d\n",
		emptyDash(pub.Channel), formatPlanned(pub.PlannedAt), e.Draft.Id)
	if tags := DecodeTags(e.Draft.Tags); len(tags) > 0 {
		fmt.Fprintf(&b, "> 标签：%s\n", strings.Join(tags, " / "))
	}
	fmt.Fprintf(&b, "> 导出时间：%s（排期自动任务生成，人工粘贴后再回填链接）\n\n", now.Local().Format("2006-01-02 15:04"))

	b.WriteString("## 正文\n\n")
	body := strings.TrimSpace(e.BodyMd)
	if body == "" {
		body = "（正文为空）"
	}
	b.WriteString(body)
	b.WriteString("\n")

	if len(e.Assets) > 0 {
		b.WriteString("\n## 素材清单\n\n")
		for i, a := range e.Assets {
			loc := strings.TrimSpace(a.FileKey)
			if loc == "" {
				loc = strings.TrimSpace(a.Url)
			}
			line := fmt.Sprintf("%d. [%s] %s", i+1, emptyDash(a.Kind), emptyDash(a.Title))
			if loc != "" {
				line += " — `" + loc + "`"
			}
			if note := strings.TrimSpace(a.LicenseNote); note != "" {
				line += "（授权：" + note + "）"
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// ExportFileName 生成成品包文件名：`pub-<id>-<channel>-<标题 slug>.md`。
//
// 带上 pub id 是为了「同一篇稿子改了排期重新导出」时覆盖同一份文件，而不是
// 在目录里堆一串同名副本；渠道与标题只是给人看的。
func ExportFileName(pub model.ContentPublication, title string) string {
	slug := Slugify(title, 40)
	if slug == "" {
		slug = fmt.Sprintf("draft-%d", pub.DraftID)
	}
	return fmt.Sprintf("pub-%d-%s-%s.md", pub.Id, Slugify(pub.Channel, 16), slug)
}

// Slugify 把标题归一成能当文件名的片段：保留字母/数字/汉字，其余压成单个 '-'。
//
// 不用正则是为了避免「中文标题整个被删空」；长度按 rune 截断，不会把 UTF-8 切坏。
func Slugify(s string, max int) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if max > 0 {
		if rs := []rune(out); len(rs) > max {
			out = strings.Trim(string(rs[:max]), "-")
		}
	}
	return out
}

// BackoffFor 算出第 attempts 次失败后要等多久再试：base, 2*base, 4*base…，上限 max。
//
// attempts 从 1 开始（第一次失败等 base）。参数非法时用 1 分钟 / 1 小时兜底，
// 避免配置写 0 之后变成「立刻重试、打爆磁盘」。
func BackoffFor(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Minute
	}
	if max <= 0 {
		max = time.Hour
	}
	if max < base {
		max = base
	}
	if attempts <= 1 {
		return base
	}
	if attempts > 20 {
		return max
	}
	d := base << (attempts - 1)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// PackagePublisher 是默认发布器：把成品包写成 Markdown 文件。
//
// 目录结构是 `<Dir>/<user 段>/<文件名>`；不按月份再分层，因为一个博主的排期量
// 按天算也就几十条，多一层目录只会让「找上周那篇」多点几次。
type PackagePublisher struct {
	Dir string
	Now func() time.Time // 测试注入；为空用 time.Now
}

// Publish 渲染并落盘，返回绝对路径（写进台账 url，界面上可直接定位文件）。
func (p PackagePublisher) Publish(ctx context.Context, e Export, pub model.ContentPublication) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := strings.TrimSpace(p.Dir)
	if dir == "" {
		dir = filepath.Join("artifacts", "content-publish")
	}
	userDir := filepath.Join(dir, SafeSegment(pub.UserID, 64))
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return "", fmt.Errorf("建导出目录失败：%w", err)
	}
	path := filepath.Join(userDir, ExportFileName(pub, e.Title))
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	if err := os.WriteFile(path, []byte(RenderPackage(e, pub, now)), 0o644); err != nil {
		return "", fmt.Errorf("写成品包失败：%w", err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, nil
}

// SafeSegment 把任意 id 归一成单个安全的路径片段，防止 userID / 渠道名里的
// ../ 或盘符把文件写到导出目录之外。
func SafeSegment(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "unknown"
	}
	if max > 0 {
		if rs := []rune(out); len(rs) > max {
			out = string(rs[:max])
		}
	}
	return out
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}

func formatPlanned(t *time.Time) string {
	if t == nil {
		return "未指定"
	}
	return t.Local().Format("2006-01-02 15:04")
}
