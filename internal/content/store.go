package content

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store 是内容库的数据访问层：只碰数据库，不碰 HTTP、不碰模型调用。
//
// 所有方法第一个参数都是 userID，并强制拼进 where —— 这是多用户隔离的唯一保证，
// 任何绕过它的查询都视为 bug（见 rules_test.go 的隔离用例与 store_db_test.go）。
type Store struct {
	DB *gorm.DB
}

// NewStore 绑定一个已就绪的 gorm 连接。
func NewStore(db *gorm.DB) *Store { return &Store{DB: db} }

func (s *Store) ready() error {
	if s == nil || s.DB == nil {
		return errors.New("content: 数据库未初始化")
	}
	return nil
}

func pageLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// ---------- 素材 ----------

// SaveAsset 登记素材；若同用户已有同 sha256 的记录，直接返回那一条（reused=true）。
//
// 去重是「按用户」的：两个用户存同一张图各自占一行，互不顶掉（与 KBDocument 的教训一致）。
func (s *Store) SaveAsset(userID string, a model.ContentAsset) (model.ContentAsset, bool, error) {
	if err := s.ready(); err != nil {
		return a, false, err
	}
	if strings.TrimSpace(userID) == "" {
		return a, false, errors.New("content: userID 不能为空")
	}
	if err := CheckAssetInput(a.Kind, a.Title, a.FileKey, a.Url); err != nil {
		return a, false, err
	}
	a.UserID = userID
	a.Title = CleanTitle(a.Title)
	a.Tags = EncodeTags(DecodeTags(a.Tags))
	if a.Sha256 = strings.ToLower(strings.TrimSpace(a.Sha256)); a.Sha256 != "" {
		var exist model.ContentAsset
		err := s.DB.Where("user_id = ? AND sha256 = ?", userID, a.Sha256).First(&exist).Error
		if err == nil {
			return exist, true, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return a, false, err
		}
	}
	if err := s.DB.Create(&a).Error; err != nil {
		return a, false, err
	}
	return a, false, nil
}

// GetAsset 按 id 取素材（跨用户不可见）。
func (s *Store) GetAsset(userID string, id uint) (model.ContentAsset, error) {
	var a model.ContentAsset
	if err := s.ready(); err != nil {
		return a, err
	}
	if err := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return a, ErrNotFound
		}
		return a, err
	}
	return a, nil
}

// ListAssets 按类型/标签/是否被引用筛选素材。
func (s *Store) ListAssets(userID string, f AssetFilter) ([]model.ContentAsset, error) {
	out := make([]model.ContentAsset, 0, 16)
	if err := s.ready(); err != nil {
		return out, err
	}
	q := s.DB.Model(&model.ContentAsset{}).Where("user_id = ?", userID)
	if f.Kind != "" {
		if !ValidAssetKind(f.Kind) {
			return out, ErrInvalidKind
		}
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Tag != "" {
		// 标签列是 JSON 文本：用 LIKE 命中即可（标签短、量级小，不值得上全文索引）。
		q = q.Where("tags LIKE ?", "%\""+strings.TrimSpace(f.Tag)+"\"%")
	}
	if f.Unused {
		q = q.Where("id NOT IN (SELECT asset_id FROM content_draft_asset WHERE user_id = ?)", userID)
	}
	return out, q.Order("created_at desc, id desc").Limit(pageLimit(f.Limit)).Offset(maxInt(f.Offset, 0)).Find(&out).Error
}

// UpdateAsset 改素材元信息（类型与去重哈希不允许改：改了就不是同一条素材了）。
func (s *Store) UpdateAsset(userID string, id uint, patch model.ContentAsset) (model.ContentAsset, error) {
	cur, err := s.GetAsset(userID, id)
	if err != nil {
		return cur, err
	}
	if patch.Title != "" {
		cur.Title = CleanTitle(patch.Title)
	}
	if patch.Description != "" {
		cur.Description = patch.Description
	}
	if patch.Source != "" {
		cur.Source = patch.Source
	}
	if patch.LicenseNote != "" {
		cur.LicenseNote = patch.LicenseNote
	}
	if patch.Url != "" {
		cur.Url = patch.Url
	}
	if patch.FileKey != "" {
		cur.FileKey = patch.FileKey
	}
	if patch.Tags != "" {
		cur.Tags = EncodeTags(DecodeTags(patch.Tags))
	}
	if err := s.DB.Save(&cur).Error; err != nil {
		return cur, err
	}
	return cur, nil
}

// DeleteAsset 删除素材；被稿件引用时返回 *AssetInUseError，不做级联删除。
//
// 不级联是因为「删一张图顺手删掉三处正文里的引用」对用户来说是惊吓而不是便利。
func (s *Store) DeleteAsset(userID string, id uint) error {
	if err := s.ready(); err != nil {
		return err
	}
	if _, err := s.GetAsset(userID, id); err != nil {
		return err
	}
	refs, err := s.DraftsUsingAsset(userID, id)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &AssetInUseError{Drafts: refs}
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND asset_id = ?", userID, id).Delete(&model.ContentDraftAsset{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ContentAsset{}).Error
	})
}

// ---------- 稿件 ----------

// CreateDraft 新建稿件，同时写入 v1 版本（正文与版本表不会出现不一致）。
func (s *Store) CreateDraft(userID, title, topic, body string, tags []string) (model.ContentDraft, error) {
	var d model.ContentDraft
	if err := s.ready(); err != nil {
		return d, err
	}
	if strings.TrimSpace(userID) == "" {
		return d, errors.New("content: userID 不能为空")
	}
	if err := CheckDraftInput(title, body); err != nil {
		return d, err
	}
	d = model.ContentDraft{
		UserID: userID, Title: CleanTitle(title), Topic: strings.TrimSpace(topic),
		BodyMd: body, Status: DraftDraft, Tags: EncodeTags(tags),
		CurrentVersion: 1,
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&d).Error; err != nil {
			return err
		}
		return tx.Create(&model.ContentDraftVersion{
			UserID: userID, DraftID: d.Id, Version: 1, BodyMd: body,
			Summary: "新建稿件", CreatedBy: userID,
		}).Error
	})
	if err != nil {
		return d, err
	}
	return s.GetDraft(userID, d.Id)
}

// GetDraft 取稿件当前状态（不含历史版本）。
func (s *Store) GetDraft(userID string, id uint) (model.ContentDraft, error) {
	var d model.ContentDraft
	if err := s.ready(); err != nil {
		return d, err
	}
	if err := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return d, ErrNotFound
		}
		return d, err
	}
	return d, nil
}

// ListDrafts 列稿件；不返回正文（列表页不需要几十 KB 的 mediumtext）。
func (s *Store) ListDrafts(userID string, f DraftFilter) ([]model.ContentDraft, error) {
	out := make([]model.ContentDraft, 0, 16)
	if err := s.ready(); err != nil {
		return out, err
	}
	q := s.DB.Model(&model.ContentDraft{}).Where("user_id = ?", userID)
	if f.Status != "" {
		if !ValidDraftStatus(f.Status) {
			return out, ErrInvalidStatus
		}
		q = q.Where("status = ?", f.Status)
	}
	if kw := strings.TrimSpace(f.Query); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("title LIKE ? OR body_md LIKE ?", like, like)
	}
	if f.Tag != "" {
		q = q.Where("tags LIKE ?", "%\""+strings.TrimSpace(f.Tag)+"\"%")
	}
	return out, q.Select("id, user_id, title, topic, status, tags, due_at, current_version, created_at, updated_at").
		Order("updated_at desc, id desc").Limit(pageLimit(f.Limit)).Offset(maxInt(f.Offset, 0)).Find(&out).Error
}

// SaveDraftBody 保存正文：追加一个版本并把稿件当前正文指过去。
//
// 用行锁把同一篇稿件的并发保存串起来，避免两个请求都算出 current+1 撞唯一键。
func (s *Store) SaveDraftBody(userID string, id uint, body, summary, by string) (model.ContentDraft, error) {
	var d model.ContentDraft
	if err := s.ready(); err != nil {
		return d, err
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", id, userID).First(&d).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if strings.TrimSpace(body) == "" {
			return errors.New("content: 正文不能为空")
		}
		if strings.TrimSpace(summary) == "" {
			summary = fmt.Sprintf("保存 v%d", d.CurrentVersion+1)
		}
		next := d.CurrentVersion + 1
		if err := tx.Create(&model.ContentDraftVersion{
			UserID: userID, DraftID: d.Id, Version: next, BodyMd: body,
			Summary: summary, CreatedBy: by,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.ContentDraft{}).Where("id = ? AND user_id = ?", id, userID).
			Updates(map[string]any{"body_md": body, "current_version": next}).Error
	})
	if err != nil {
		return d, err
	}
	return s.GetDraft(userID, id)
}

// SetDraftStatus 走状态机改状态；非法跳转返回 ErrBadTransition。
func (s *Store) SetDraftStatus(userID string, id uint, status string) (model.ContentDraft, error) {
	var d model.ContentDraft
	if err := s.ready(); err != nil {
		return d, err
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", id, userID).First(&d).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := CanTransitionDraft(d.Status, status); err != nil {
			return err
		}
		if d.Status == status {
			return nil
		}
		return tx.Model(&model.ContentDraft{}).Where("id = ? AND user_id = ?", id, userID).
			Update("status", status).Error
	})
	if err != nil {
		return d, err
	}
	return s.GetDraft(userID, id)
}

// ListVersions 取版本链（新版本在前）。
func (s *Store) ListVersions(userID string, id uint) ([]model.ContentDraftVersion, error) {
	out := make([]model.ContentDraftVersion, 0, 8)
	if err := s.ready(); err != nil {
		return out, err
	}
	if _, err := s.GetDraft(userID, id); err != nil {
		return out, err
	}
	return out, s.DB.Where("user_id = ? AND draft_id = ?", userID, id).Order("version desc").Find(&out).Error
}

// RollbackDraft 把正文恢复到某个历史版本。
//
// 回滚不是「改历史」而是「再追加一个内容等于旧版的新版本」：版本链只增不改，
// 所以「谁在什么时候回滚过」在版本记录里看得见，回滚本身也能被再回滚。
func (s *Store) RollbackDraft(userID string, id uint, version int, by string) (model.ContentDraft, error) {
	if err := s.ready(); err != nil {
		return model.ContentDraft{}, err
	}
	var target model.ContentDraftVersion
	if err := s.DB.Where("user_id = ? AND draft_id = ? AND version = ?", userID, id, version).First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ContentDraft{}, ErrNotFound
		}
		return model.ContentDraft{}, err
	}
	return s.SaveDraftBody(userID, id, target.BodyMd, fmt.Sprintf("回滚到 v%d", version), by)
}

// ---------- 稿件 ↔ 素材 ----------

// LinkAsset 关联素材；同一 (draft, asset, role) 重复关联时更新锚点而不是报错。
func (s *Store) LinkAsset(userID string, draftID, assetID uint, role, anchor string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !ValidRole(role) {
		return errors.New("content: 素材用途不合法")
	}
	role = NormalizeRole(role)
	if _, err := s.GetDraft(userID, draftID); err != nil {
		return err
	}
	if _, err := s.GetAsset(userID, assetID); err != nil {
		return err
	}
	link := model.ContentDraftAsset{UserID: userID, DraftID: draftID, AssetID: assetID, Role: role, Anchor: strings.TrimSpace(anchor)}
	return s.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "draft_id"}, {Name: "asset_id"}, {Name: "role"}},
		DoUpdates: clause.AssignmentColumns([]string{"anchor"}),
	}).Create(&link).Error
}

// UnlinkAsset 解除关联；不存在视为成功（幂等）。
func (s *Store) UnlinkAsset(userID string, draftID, assetID uint, role string) error {
	if err := s.ready(); err != nil {
		return err
	}
	q := s.DB.Where("user_id = ? AND draft_id = ? AND asset_id = ?", userID, draftID, assetID)
	if strings.TrimSpace(role) != "" {
		q = q.Where("role = ?", NormalizeRole(role))
	}
	return q.Delete(&model.ContentDraftAsset{}).Error
}

// DraftAssets 返回稿件关联的素材（含关联信息，按加入时间正序）。
func (s *Store) DraftAssets(userID string, draftID uint) ([]model.ContentDraftAsset, []model.ContentAsset, error) {
	links := make([]model.ContentDraftAsset, 0, 8)
	assets := make([]model.ContentAsset, 0, 8)
	if err := s.ready(); err != nil {
		return links, assets, err
	}
	if _, err := s.GetDraft(userID, draftID); err != nil {
		return links, assets, err
	}
	if err := s.DB.Where("user_id = ? AND draft_id = ?", userID, draftID).Order("id asc").Find(&links).Error; err != nil {
		return links, assets, err
	}
	if len(links) == 0 {
		return links, assets, nil
	}
	ids := make([]uint, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.AssetID)
	}
	if err := s.DB.Where("user_id = ? AND id IN ?", userID, ids).Find(&assets).Error; err != nil {
		return links, assets, err
	}
	return links, assets, nil
}

// DraftsUsingAsset 返回引用了某素材的稿件 id（删除前的拦截依据）。
func (s *Store) DraftsUsingAsset(userID string, assetID uint) ([]uint, error) {
	ids := make([]uint, 0, 4)
	if err := s.ready(); err != nil {
		return ids, err
	}
	return ids, s.DB.Model(&model.ContentDraftAsset{}).Where("user_id = ? AND asset_id = ?", userID, assetID).
		Distinct().Pluck("draft_id", &ids).Error
}

// ---------- 发布台账 ----------

// SavePublication 新建一条排期/发布记录（DraftID=0 表示不属于任何稿件）。
func (s *Store) SavePublication(userID string, p model.ContentPublication) (model.ContentPublication, error) {
	if err := s.ready(); err != nil {
		return p, err
	}
	p.UserID = userID
	p.Channel = strings.ToLower(strings.TrimSpace(p.Channel))
	if p.Channel == "" {
		return p, errors.New("content: 渠道不能为空")
	}
	if p.Status == "" {
		p.Status = PubPlanned
	}
	if err := CanTransitionPub("", p.Status); err != nil {
		return p, err
	}
	if p.DraftID != 0 {
		if _, err := s.GetDraft(userID, p.DraftID); err != nil {
			return p, err
		}
	}
	if err := s.DB.Create(&p).Error; err != nil {
		return p, err
	}
	return p, nil
}

// UpdatePublication 改发布记录：状态跳转走状态机，失败要留原因。
func (s *Store) UpdatePublication(userID string, id uint, status, url, cause string, metrics string) (model.ContentPublication, error) {
	var p model.ContentPublication
	if err := s.ready(); err != nil {
		return p, err
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		updates := map[string]any{}
		if status != "" {
			if err := CanTransitionPub(p.Status, status); err != nil {
				return err
			}
			updates["status"] = status
			if status == PubFailed {
				updates["retries"] = p.Retries + 1
				updates["error"] = cause
			}
			if status == PubPublished {
				now := time.Now()
				updates["published_at"] = &now
				updates["error"] = ""
			}
		} else if cause != "" {
			// 没改状态也能补失败原因（例如「发出去但没拿到链接」）。
			updates["error"] = cause
		}
		if url != "" {
			updates["url"] = url
		}
		if metrics != "" {
			updates["metrics"] = metrics
		}
		if len(updates) == 0 {
			return nil
		}
		return tx.Model(&model.ContentPublication{}).Where("id = ? AND user_id = ?", id, userID).Updates(updates).Error
	})
	if err != nil {
		return p, err
	}
	if err := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return p, err
	}
	return p, nil
}

// ListPublications 按稿件/渠道/状态/时间窗筛选发布台账。
func (s *Store) ListPublications(userID string, f PubFilter) ([]model.ContentPublication, error) {
	out := make([]model.ContentPublication, 0, 16)
	if err := s.ready(); err != nil {
		return out, err
	}
	q := s.DB.Model(&model.ContentPublication{}).Where("user_id = ?", userID)
	if f.DraftID != 0 {
		q = q.Where("draft_id = ?", f.DraftID)
	}
	if f.Channel != "" {
		q = q.Where("channel = ?", strings.ToLower(strings.TrimSpace(f.Channel)))
	}
	if f.Status != "" {
		if !ValidPubStatus(f.Status) {
			return out, ErrInvalidStatus
		}
		q = q.Where("status = ?", f.Status)
	}
	if f.From != nil {
		q = q.Where("planned_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("planned_at < ?", *f.To)
	}
	return out, q.Order("planned_at desc, id desc").Limit(pageLimit(f.Limit)).Offset(maxInt(f.Offset, 0)).Find(&out).Error
}

// ---------- 导出 ----------

// ExportDraft 生成某渠道的成品包：有平台定制版就用定制版，否则用稿件正文。
func (s *Store) ExportDraft(userID string, draftID uint, channel string) (Export, error) {
	d, err := s.GetDraft(userID, draftID)
	if err != nil {
		return Export{}, err
	}
	_, assets, err := s.DraftAssets(userID, draftID)
	if err != nil {
		return Export{}, err
	}
	out := Export{Draft: d, Title: d.Title, BodyMd: d.BodyMd, Assets: assets}
	if ch := strings.ToLower(strings.TrimSpace(channel)); ch != "" {
		var p model.ContentPublication
		err := s.DB.Where("user_id = ? AND draft_id = ? AND channel = ?", userID, draftID, ch).
			Order("id desc").First(&p).Error
		switch {
		case err == nil:
			if strings.TrimSpace(p.VariantTitle) != "" {
				out.Title = p.VariantTitle
			}
			if strings.TrimSpace(p.VariantBody) != "" {
				out.BodyMd = p.VariantBody
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 没有平台定制版就用主正文，不算错误。
		default:
			return out, err
		}
		out.Channels = []string{ch}
	}
	return out, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
