package model

import "time"

// 自媒体「稿件 · 素材 · 发布」三件套的数据模型。
//
// 设计约束（见 docs/content/需求分析-自媒体稿件素材发布.md）：
//   - 全部按 user_id 隔离，所有查询必须带 user_id；
//   - 只增表，不改既有表语义，AutoMigrate 追加即可；
//   - 正文只存 Markdown，导出格式在读取时生成；
//   - 一稿多平台版本用 content_publication.variant_* 承载，不再单独开 variant 表。

// ContentAsset 是一条素材：图片、视频、音频、文本片段或链接。
//
// 文件本体不进库（图片/视频动辄几十 MB），只登记引用：FileKey 是本地相对键，
// Url 是外链；两者至少有一个。Sha256 用来在同一用户内去重——同一张图反复登记
// 只会得到同一条记录，引用它的稿件共用一份元信息。
type ContentAsset struct {
	Id          uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      string    `gorm:"column:user_id;size:36;not null;index:idx_content_asset_user_kind,priority:1;uniqueIndex:idx_content_asset_user_sha,priority:1"`
	Kind        string    `gorm:"column:kind;size:16;not null;index:idx_content_asset_user_kind,priority:2"` // image/video/audio/text/link
	Title       string    `gorm:"column:title;size:255;not null"`
	Description string    `gorm:"column:description;type:text"`
	Tags        string    `gorm:"column:tags;size:512"` // JSON 数组文本，与 AgentSkill.Tags 保持一致
	Source      string    `gorm:"column:source;size:512"`
	LicenseNote string    `gorm:"column:license_note;size:512"` // 版权/授权备注：能不能商用、要不要署名
	FileKey     string    `gorm:"column:file_key;size:512"`
	Url         string    `gorm:"column:url;size:1024"`
	Sha256      string    `gorm:"column:sha256;type:char(64);uniqueIndex:idx_content_asset_user_sha,priority:2"`
	Bytes       int64     `gorm:"column:bytes;not null;default:0"`
	Width       int       `gorm:"column:width;not null;default:0"`
	Height      int       `gorm:"column:height;not null;default:0"`
	DurationMs  int       `gorm:"column:duration_ms;not null;default:0"`
	CreatedAt   time.Time `gorm:"column:created_at;index:idx_content_asset_user_created,priority:1"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// ContentDraft 是一篇稿件的主记录，BodyMd 永远是「当前版本」的正文。
//
// 历史正文在 ContentDraftVersion 里，每次保存都追加一条版本，回滚也是追加
// （把旧正文重新写成新版本）—— 这样版本链只增不改，永远不会丢内容。
type ContentDraft struct {
	Id      uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID  string `gorm:"column:user_id;size:36;not null;index:idx_content_draft_user_status,priority:1;index:idx_content_draft_user_updated,priority:1"`
	Title   string `gorm:"column:title;size:255;not null"`
	Topic   string `gorm:"column:topic;size:255"`
	BodyMd  string `gorm:"column:body_md;type:mediumtext;not null"`
	Status  string `gorm:"column:status;size:16;not null;index:idx_content_draft_user_status,priority:2"` // idea/draft/review/final/scheduled/published/archived
	Tags    string `gorm:"column:tags;size:512"`
	DueAt   *time.Time `gorm:"column:due_at"`
	// CurrentVersion 与版本表里的最大版本号一致；用列存一份是为了列表页不用 join。
	CurrentVersion int       `gorm:"column:current_version;not null;default:0"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;index:idx_content_draft_user_updated,priority:2"`
}

// ContentDraftVersion 是稿件的一个历史版本。
//
// 保存即追加：正文、摘要、改动人。回滚不改历史，而是把目标版本正文再追加一条，
// 所以「回滚过」这件事在版本链里看得见。
type ContentDraftVersion struct {
	Id        uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    string    `gorm:"column:user_id;size:36;not null;index:idx_content_ver_user_draft,priority:1"`
	DraftID   uint      `gorm:"column:draft_id;not null;index:idx_content_ver_user_draft,priority:2;uniqueIndex:idx_content_ver_draft_version,priority:1"`
	Version   int       `gorm:"column:version;not null;uniqueIndex:idx_content_ver_draft_version,priority:2"`
	BodyMd    string    `gorm:"column:body_md;type:mediumtext;not null"`
	Summary   string    `gorm:"column:summary;size:512"`
	CreatedBy string    `gorm:"column:created_by;size:64"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// ContentDraftAsset 是稿件与素材的多对多关联。
//
// Role 说明这条素材在稿子里干什么（配图/引用/参考），Anchor 记录引用位置
// （段落序号或小标题），这样「这条素材被谁用过、用在哪」都能回答。
type ContentDraftAsset struct {
	Id        uint      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    string    `gorm:"column:user_id;size:36;not null;index:idx_content_da_user_draft,priority:1"`
	DraftID   uint      `gorm:"column:draft_id;not null;index:idx_content_da_user_draft,priority:2;uniqueIndex:idx_content_da_unique,priority:1;index:idx_content_da_asset,priority:1"`
	AssetID   uint      `gorm:"column:asset_id;not null;uniqueIndex:idx_content_da_unique,priority:2;index:idx_content_da_asset,priority:2"`
	Role      string    `gorm:"column:role;size:16;not null;uniqueIndex:idx_content_da_unique,priority:3"` // image/quote/reference
	Anchor    string    `gorm:"column:anchor;size:255"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// ContentPublication 是一条发布台账：排期、实际发布、失败重试、指标都记在这里。
//
// 平台版本（VariantTitle/VariantBodyMd）为空时表示「直接用稿件正文」；
// 有值时该渠道用这份定制版，素材关联仍共享稿件上的关联。
type ContentPublication struct {
	Id           uint       `gorm:"column:id;primaryKey;autoIncrement"`
	UserID       string     `gorm:"column:user_id;size:36;not null;index:idx_content_pub_user_channel,priority:1;index:idx_content_pub_user_planned,priority:1"`
	DraftID      uint       `gorm:"column:draft_id;not null;index:idx_content_pub_draft"`
	Channel      string     `gorm:"column:channel;size:32;not null;index:idx_content_pub_user_channel,priority:2"` // wechat/zhihu/xiaohongshu/bilibili/weibo/shipinhao
	VariantTitle string     `gorm:"column:variant_title;size:255"`
	VariantBody  string     `gorm:"column:variant_body_md;type:mediumtext"`
	PlannedAt    *time.Time `gorm:"column:planned_at;index:idx_content_pub_user_planned,priority:2"`
	PublishedAt  *time.Time `gorm:"column:published_at"`
	Url          string     `gorm:"column:url;size:1024"`
	Status       string     `gorm:"column:status;size:16;not null;index:idx_content_pub_user_channel,priority:3"` // planned/published/failed/withdrawn
	Error        string     `gorm:"column:error;size:512"`
	Retries      int        `gorm:"column:retries;not null;default:0"`
	Metrics      string     `gorm:"column:metrics;type:text"` // JSON：阅读/点赞/收藏/涨粉
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (ContentAsset) TableName() string        { return "content_asset" }
func (ContentDraft) TableName() string        { return "content_draft" }
func (ContentDraftVersion) TableName() string { return "content_draft_version" }
func (ContentDraftAsset) TableName() string   { return "content_draft_asset" }
func (ContentPublication) TableName() string  { return "content_publication" }
