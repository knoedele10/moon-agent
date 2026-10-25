// Package content 是自媒体「素材 · 稿件 · 发布」三件套的数据访问层。
//
// 这一层只做数据库：建表结构在 internal/model/content.go，仓储在本包。
// 之所以单独成包而不塞进 web：一来可以脱离 HTTP 单测（rules_test.go 不需要数据库），
// 二来将来接 Agent 工具、脚本、定时任务都能复用同一套校验，不会各写一份漂移。
package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/model"
)

// 素材类型：与前端筛选、导入脚本共用同一份枚举，避免「图片」在不同地方写四种拼法。
const (
	AssetImage = "image"
	AssetVideo = "video"
	AssetAudio = "audio"
	AssetText  = "text"
	AssetLink  = "link"
)

// 稿件状态机：灵感 → 草稿 → 待审 → 定稿 → 已排期 → 已发布 → 归档。
//
// 允许「退回」：待审打回草稿，定稿打回待审，已排期退回定稿；任何状态都能归档，
// 归档可以重开成草稿。不允许跳级（灵感直接已发布），因为那会绕过人工审一遍的约定。
const (
	DraftIdea      = "idea"
	DraftDraft     = "draft"
	DraftReview    = "review"
	DraftFinal     = "final"
	DraftScheduled = "scheduled"
	DraftPublished = "published"
	DraftArchived  = "archived"
)

// 发布台账状态。
//
// 多出来的 exported 是「自动任务已经到点把成品包导出来了，但人还没粘贴」：
// 项目不做真实平台发帖（见 docs/content），所以自动任务不能直接把状态写成
// published —— 那会让台账谎报一条根本不存在的线上链接。人工贴完再走
// exported → published 并回填 url，台账才干净。
const (
	PubPlanned   = "planned"
	PubExported  = "exported"
	PubPublished = "published"
	PubFailed    = "failed"
	PubWithdrawn = "withdrawn"
)

// 素材在稿件里的用途。
const (
	RoleImage = "image"
	RoleQuote = "quote"
	RoleRef   = "reference"
)

var (
	// ErrNotFound 统一表示「对当前用户不可见」：跨用户与真不存在返回同一个错误，
	// 避免调用方用错误信息探测别人的数据是否存在。
	ErrNotFound = errors.New("content: 记录不存在")
	// ErrInvalidKind / ErrInvalidStatus 是入参校验失败（400）。
	ErrInvalidKind   = errors.New("content: 素材类型不合法")
	ErrInvalidStatus = errors.New("content: 状态不合法")
	// ErrBadTransition 是状态机不允许的跳转（409）。
	ErrBadTransition = errors.New("content: 状态跳转不允许")
)

// AssetInUseError 表示素材正被稿件引用，删除被拒绝。
// 带上引用它的稿件 id，调用方可以提示「先解除这 3 篇稿子的引用」。
type AssetInUseError struct {
	Drafts []uint
}

func (e *AssetInUseError) Error() string {
	return fmt.Sprintf("content: 素材正被 %d 篇稿件引用，不能删除", len(e.Drafts))
}

// draftTransitions 是稿件状态机：from → 允许的 to 集合。
var draftTransitions = map[string]map[string]bool{
	DraftIdea:      {DraftDraft: true, DraftArchived: true},
	DraftDraft:     {DraftReview: true, DraftIdea: true, DraftArchived: true},
	DraftReview:    {DraftFinal: true, DraftDraft: true, DraftArchived: true},
	DraftFinal:     {DraftScheduled: true, DraftReview: true, DraftArchived: true},
	DraftScheduled: {DraftPublished: true, DraftFinal: true, DraftArchived: true},
	DraftPublished: {DraftArchived: true},
	DraftArchived:  {DraftDraft: true, DraftIdea: true},
}

// pubTransitions 是发布台账状态机：失败可以重排（planned），已发可以撤回。
//
// exported 是自动任务的产物，不是终态：贴完要显式推到 published，想撤回也能直接 withdrawn；
// 排期改了时间则退回 planned 重新排队。
var pubTransitions = map[string]map[string]bool{
	PubPlanned:   {PubExported: true, PubPublished: true, PubFailed: true, PubWithdrawn: true},
	PubExported:  {PubPublished: true, PubPlanned: true, PubFailed: true, PubWithdrawn: true},
	PubFailed:    {PubPlanned: true, PubExported: true, PubPublished: true, PubWithdrawn: true},
	PubPublished: {PubWithdrawn: true},
	PubWithdrawn: {PubPlanned: true},
}

// ValidAssetKind 判断素材类型是否合法。
func ValidAssetKind(kind string) bool {
	switch kind {
	case AssetImage, AssetVideo, AssetAudio, AssetText, AssetLink:
		return true
	}
	return false
}

// ValidDraftStatus 判断稿件状态是否合法。
func ValidDraftStatus(status string) bool {
	_, ok := draftTransitions[status]
	return ok
}

// ValidPubStatus 判断发布状态是否合法。
func ValidPubStatus(status string) bool {
	_, ok := pubTransitions[status]
	return ok
}

// CanTransitionDraft 判断稿件状态跳转是否允许；同状态视为幂等成功。
func CanTransitionDraft(from, to string) error {
	if !ValidDraftStatus(to) {
		return ErrInvalidStatus
	}
	if from == to {
		return nil
	}
	if from == "" {
		// 新建稿件允许直接落在任何「非终态」上（导入历史数据时常见）。
		if to == DraftPublished || to == DraftArchived {
			return ErrBadTransition
		}
		return nil
	}
	if allowed, ok := draftTransitions[from]; ok && allowed[to] {
		return nil
	}
	return fmt.Errorf("%w：%s → %s", ErrBadTransition, from, to)
}

// CanTransitionPub 判断发布状态跳转是否允许；同状态视为幂等成功。
func CanTransitionPub(from, to string) error {
	if !ValidPubStatus(to) {
		return ErrInvalidStatus
	}
	if from == to || from == "" {
		return nil
	}
	if allowed, ok := pubTransitions[from]; ok && allowed[to] {
		return nil
	}
	return fmt.Errorf("%w：%s → %s", ErrBadTransition, from, to)
}

// ValidRole 判断素材用途是否合法；空串按「参考」处理（历史数据兼容）。
func ValidRole(role string) bool {
	switch role {
	case "", RoleImage, RoleQuote, RoleRef:
		return true
	}
	return false
}

// NormalizeRole 把空用途归一成 reference。
func NormalizeRole(role string) string {
	if strings.TrimSpace(role) == "" {
		return RoleRef
	}
	return strings.TrimSpace(role)
}

// EncodeTags 把标签数组编成 JSON 文本（沿用以 AgentSkill.Tags 的存法，列表页不用再解析）。
func EncodeTags(tags []string) string {
	clean := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	if len(clean) == 0 {
		return ""
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeTags 解析标签 JSON；坏数据返回空切片而不是报错（列表页不该因为一条脏数据整页失败）。
func DecodeTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	return tags
}

// CleanTitle 归一标题：去掉首尾空白与内部连续空白，限制长度（MySQL 列宽 255）。
func CleanTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}
	return title
}

// CheckDraftInput 校验新建/保存稿件的必填项。
func CheckDraftInput(title, body string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("content: 标题不能为空")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("content: 正文不能为空")
	}
	return nil
}

// CheckAssetInput 校验素材必填项：至少要有本地键或外链之一，否则登记了也没法用。
func CheckAssetInput(kind, title, fileKey, url string) error {
	if !ValidAssetKind(kind) {
		return ErrInvalidKind
	}
	if strings.TrimSpace(title) == "" {
		return errors.New("content: 素材标题不能为空")
	}
	if strings.TrimSpace(fileKey) == "" && strings.TrimSpace(url) == "" {
		return errors.New("content: 素材需要 file_key 或 url 之一")
	}
	return nil
}

// DraftFilter 是稿件列表的筛选条件；零值表示不筛。
type DraftFilter struct {
	Status string
	Query  string // 标题/正文关键词
	Tag    string
	Limit  int
	Offset int
}

// AssetFilter 是素材列表的筛选条件。
type AssetFilter struct {
	Kind   string
	Tag    string
	Unused bool // 只列没被任何稿件引用的素材
	Limit  int
	Offset int
}

// PubFilter 是发布台账的筛选条件。
type PubFilter struct {
	DraftID uint
	Channel string
	Status  string
	From    *time.Time
	To      *time.Time
	Limit   int
	Offset  int
}

// Export 是一篇稿件按渠道导出的「可直接粘贴」成品包。
type Export struct {
	Draft    model.ContentDraft
	Title    string
	BodyMd   string
	Assets   []model.ContentAsset
	Channels []string
}
