package content

import (
	"errors"
	"strings"
	"time"

	"blog-my/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 本文件是自动任务（排期到点导出）用到的数据访问方法，接在 store.go 的发布台账之上。
//
// 这里只回答三件事：
//  1. 现在哪些排期到期了？（DuePublications / CountDuePublications）
//  2. 这条排期归我处理吗？（ClaimPublication：用 next_attempt_at 当租约）
//  3. 处理完了怎么记账？（MarkExported / RecordExportFailure）
//
// 「到点导出」不做真实发帖，所以成功只推进到 exported，不写 published_at ——
// 线上链接只有人贴完才知道，自动任务不猜。

// duePublications 是「到期且可尝试」的统一条件：
// 状态还是 planned、有排期时间、已经到点、没有别的 worker 的租约、关联了稿件。
//
// draft_id = 0 的排期（外部内容/占位）不参与自动导出：没有正文可导，让它原样留在台账里，
// 人去手工登记，比自动把它打成 failed 更合理。
func duePublications(db *gorm.DB, userID string, now time.Time) *gorm.DB {
	q := db.Model(&model.ContentPublication{}).
		Where("status = ? AND draft_id <> 0 AND planned_at IS NOT NULL AND planned_at <= ?", PubPlanned, now).
		Where("(next_attempt_at IS NULL OR next_attempt_at <= ?)", now)
	if uid := strings.TrimSpace(userID); uid != "" {
		q = q.Where("user_id = ?", uid)
	}
	return q
}

// DuePublications 列出到期的排期（按计划时间从早到晚）。
//
// userID 为空表示后台调度：扫全库（多用户桌面共用一份库时也只有 worker 会用这个口径，
// 它不回传任何数据给某个用户）。HTTP 侧必须带 userID。
func (s *Store) DuePublications(userID string, now time.Time, limit int) ([]model.ContentPublication, error) {
	out := make([]model.ContentPublication, 0, 16)
	if err := s.ready(); err != nil {
		return out, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return out, duePublications(s.DB, userID, now).
		Order("planned_at asc, id asc").Limit(limit).Find(&out).Error
}

// CountDuePublications 数当前用户还有多少条到期待导出（状态接口用）。
func (s *Store) CountDuePublications(userID string, now time.Time) (int64, error) {
	var n int64
	if err := s.ready(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(userID) == "" {
		return 0, errors.New("content: userID 不能为空")
	}
	return n, duePublications(s.DB, userID, now).Count(&n).Error
}

// GetPublication 按 id 取一条台账（跨用户不可见）。
func (s *Store) GetPublication(userID string, id uint) (model.ContentPublication, error) {
	var p model.ContentPublication
	if err := s.ready(); err != nil {
		return p, err
	}
	err := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return p, ErrNotFound
	}
	return p, err
}

// ClaimPublication 领取一条到期排期：把 next_attempt_at 推到 leaseUntil 作为租约。
//
// 条件更新是原子的，所以两个 worker（或手动触发撞上后台调度）只会有一个 RowsAffected=1，
// 另一个拿到 ok=false 直接跳过。租约到期而没人续，说明处理它的进程挂了，
// 这条排期会被下一轮自动重新领取 —— 不需要额外的「处理中」状态。
func (s *Store) ClaimPublication(id uint, now, leaseUntil time.Time) (model.ContentPublication, bool, error) {
	var p model.ContentPublication
	if err := s.ready(); err != nil {
		return p, false, err
	}
	if !leaseUntil.After(now) {
		leaseUntil = now.Add(10 * time.Minute)
	}
	res := s.DB.Model(&model.ContentPublication{}).
		Where("id = ? AND status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", id, PubPlanned, now).
		Updates(map[string]any{"next_attempt_at": leaseUntil})
	if res.Error != nil {
		return p, false, res.Error
	}
	if res.RowsAffected == 0 {
		return p, false, nil
	}
	if err := s.DB.Where("id = ?", id).First(&p).Error; err != nil {
		return p, false, err
	}
	return p, true, nil
}

// MarkExported 把一条排期推进到 exported，并记下成品包位置。
//
// 成功即清掉上一次的失败原因和租约：重试成功不该在台账上留着旧的 error。
func (s *Store) MarkExported(p model.ContentPublication, path string) (model.ContentPublication, error) {
	if err := s.ready(); err != nil {
		return p, err
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var row model.ContentPublication
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", p.Id, p.UserID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := CanTransitionPub(row.Status, PubExported); err != nil {
			return err
		}
		updates := map[string]any{
			"status":          PubExported,
			"url":             strings.TrimSpace(path),
			"error":           "",
			"next_attempt_at": nil,
		}
		return tx.Model(&model.ContentPublication{}).Where("id = ? AND user_id = ?", p.Id, p.UserID).
			Updates(updates).Error
	})
	if err != nil {
		return p, err
	}
	return s.GetPublication(p.UserID, p.Id)
}

// RecordExportFailure 记一次失败：递增 retries、留下原因、排下一次重试。
//
// retryAt 为空表示不再重试（次数用尽），此时状态推进到 failed 等人工处理；
// 否则状态保持 planned，靠 next_attempt_at 做退避 —— 台账上看起来还是「待发」，
// 只是这一轮不会再被扫到。
func (s *Store) RecordExportFailure(p model.ContentPublication, cause string, retryAt *time.Time, maxAttempts int) (model.ContentPublication, error) {
	if err := s.ready(); err != nil {
		return p, err
	}
	cause = truncateRunes(strings.TrimSpace(cause), 500)
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var row model.ContentPublication
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", p.Id, p.UserID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// 导出期间用户可能已经撤回/手动标记了，这时候不该把状态改回去，只留一条原因。
		if row.Status != PubPlanned && row.Status != PubExported {
			return tx.Model(&model.ContentPublication{}).Where("id = ? AND user_id = ?", p.Id, p.UserID).
				Updates(map[string]any{"error": cause}).Error
		}
		attempts := row.Retries + 1
		updates := map[string]any{"retries": attempts, "error": cause}
		if maxAttempts > 0 && attempts >= maxAttempts {
			if err := CanTransitionPub(row.Status, PubFailed); err != nil {
				return err
			}
			updates["status"] = PubFailed
			updates["next_attempt_at"] = nil
		} else {
			updates["next_attempt_at"] = retryAt
		}
		return tx.Model(&model.ContentPublication{}).Where("id = ? AND user_id = ?", p.Id, p.UserID).
			Updates(updates).Error
	})
	if err != nil {
		return p, err
	}
	return s.GetPublication(p.UserID, p.Id)
}

// truncateRunes 按字符截断，避免把一个汉字切成半个字节写进 varchar。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}
