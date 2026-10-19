package model

import "time"

// KBDocument 是知识库里的一个文档（文本或图片）。
//
// Content 统一存原始字节：kind=text 时是 UTF-8 正文，kind=image 时是原始图片字节。
// 图片不落磁盘、直接进库，这样备份、迁移、删除都只涉及数据库一处，
// 也不会因为换机器丢了图片目录。
type KBDocument struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;index;size:36;uniqueIndex:idx_kb_doc_user_sha,priority:1"`
	Kind   string `gorm:"column:kind;size:16;not null"`
	Title  string `gorm:"column:title;size:255;not null"`
	Source string `gorm:"column:source;size:512"`
	Mime   string `gorm:"column:mime;size:64"`
	Bytes  int64  `gorm:"column:bytes"`
	// Sha256 的唯一性必须是**按用户**的：它曾经是全局唯一索引，结果两个用户存
	// 同一条内容时，后写的那条直接撞唯一键失败（记忆/文档凭空消失）。
	// 改成 (user_id, sha256) 复合唯一——跨用户不再互相顶掉，同用户内仍然幂等去重。
	Sha256     string `gorm:"column:sha256;type:char(64);uniqueIndex:idx_kb_doc_user_sha,priority:2"`
	Content    []byte `gorm:"column:content;type:longblob;not null"`
	Meta       string `gorm:"column:meta;type:text"`
	ChunkCount int    `gorm:"column:chunk_count;not null;default:0"`
	// Scope 把「陪伴记忆」与「工作记忆」分开（借鉴 AAAAGENT 的核心设计：不把所有工程
	// 记录塞进陪伴记忆）。取值 "life"（生活/陪伴）、"work"（项目/技术）、""（未分类，兼容旧数据）。
	// 检索时按 scope 加权而非硬过滤：同 scope 有加成，不同 scope 不惩罚（旧数据为空串）。
	Scope string `gorm:"column:scope;size:16;index"`
	// Importance / Activity / Emotion / ReinforcedAt 是召回公式（见 rag/dynamics.go）的输入：
	//   - Importance：事实的重要性 0~1，越高活跃度衰减越慢；默认 0.5。
	//   - Activity：活跃度 A(t) 的基准值，默认 1.0；被明确重提时按 A+0.2*(1-A) 强化。
	//   - Emotion：情绪强度 0~1，默认 0（无情绪信息）；衰减比活跃度快（半衰期 7 天）。
	//   - ReinforcedAt：最近一次强化时间，可空（为空时用 CreatedAt 作为衰减起点）。
	Importance   float64    `gorm:"column:importance;not null;default:0.5"`
	Activity     float64    `gorm:"column:activity;not null;default:1"`
	Emotion      float64    `gorm:"column:emotion;not null;default:0"`
	ReinforcedAt *time.Time `gorm:"column:reinforced_at"`
	// Stale 标记“已被更新的同主题记忆取代”。检索时排除，但行保留下来做审计。
	// 记忆是 ADD-only 的，没有这个字段就会出现“端口 8300”和“改成 9000”同时可被检索到。
	Stale     bool      `gorm:"column:stale;not null;default:false"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// KBChunk 是文档切出的一个可检索块。
//
// Embedding 是 []float32 的小端原始字节（bge-m3 为 1024 维 = 4096 字节）。
// 之所以不存成 JSON：JSON 会把每个浮点变成十几个字符，体积大十倍以上，
// 而这个字段是整张表里最占空间的部分。
type KBChunk struct {
	Id         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	DocumentID uint      `gorm:"column:document_id;index;not null"`
	Seq        int       `gorm:"column:seq;not null"`
	Kind       string    `gorm:"column:kind;size:16;not null"`
	Text       string    `gorm:"column:text;type:mediumtext;not null"`
	ImagePath  string    `gorm:"column:image_path;size:512"`
	Embedding  []byte    `gorm:"column:embedding;type:longblob;not null"`
	Dim        int       `gorm:"column:dim;not null"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (KBDocument) TableName() string { return "kb_document" }
func (KBChunk) TableName() string    { return "kb_chunk" }
