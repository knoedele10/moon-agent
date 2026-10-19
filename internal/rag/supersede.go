package rag

import (
	"context"

	"blog-my/internal/model"
)

// 记忆的“更新”：新事实把同主题的旧事实标记为失效（stale）。
//
// 为什么需要：记忆是 ADD-only 的——用户说“端口改成 9000”之后，“端口是 8300”那条
// 仍然躺在库里。新近加权只能让它排到后面，**不能让它消失**；一旦查询措辞更像旧那条，
// 模型还是会拿着过时信息回答。真正的“改主意”必须让旧记忆失效。
//
// 为什么用“标记失效”而不是删除：保留行才能审计（这条是什么时候、被谁取代的），
// 也让误判可回滚。检索两条路径都过滤 stale = 0，所以失效的记忆不会再出现。
//
// 判定用的是**词面覆盖率**（复用 hybrid.go 的 lexicalScore）：新事实的词有多少
// 出现在旧事实里。阈值取得比较高（0.75）——宁可漏掉一条旧记忆，也不要把
// “用户喜欢喝咖啡”和“用户喜欢喝茶”这种不同的事误判成互相取代。

const (
	// supersedeThreshold 词面覆盖率阈值。
	supersedeThreshold = 0.75
	// supersedeMax 一次最多让多少条旧记忆失效：防止一句宽泛的新事实清掉一大片。
	supersedeMax = 3
)

// shouldSupersede 判断 oldText 是否应当被 newText 取代。
func shouldSupersede(newText, oldText string) bool {
	if newText == "" || oldText == "" || newText == oldText {
		return false
	}
	return lexicalScore(newText, oldText) >= supersedeThreshold
}

// supersedeSimilarFacts 把该用户旧的同主题记忆标记为失效，返回处理条数。
// 失败只返回错误，由调用方决定是否忽略（记忆写入不该因此失败）。
func (i *Ingester) supersedeSimilarFacts(ctx context.Context, userID string, newDocID uint, newText string) (int, error) {
	var rows []model.KBDocument
	if err := i.store.db.WithContext(ctx).
		Where("user_id = ? AND source = ? AND stale = 0 AND id <> ?", userID, "memory", newDocID).
		Order("id desc").Limit(200).Find(&rows).Error; err != nil {
		return 0, err
	}

	n := 0
	for _, r := range rows {
		if n >= supersedeMax {
			break
		}
		if !shouldSupersede(newText, string(r.Content)) {
			continue
		}
		if err := i.store.db.WithContext(ctx).Model(&model.KBDocument{}).
			Where("id = ?", r.Id).Update("stale", true).Error; err != nil {
			return n, err
		}
		// 审计：谁取代了谁。Detail 写新事实，便于回溯。
		_ = i.store.AppendMemoryEvent(ctx, r.Id, userID, "SUPERSEDE", newText)
		n++
	}
	return n, nil
}
