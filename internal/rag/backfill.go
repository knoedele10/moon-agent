package rag

import (
	"context"
	"log"
	"sync"
	"time"
)

// 补算向量：把「写入时嵌入服务不可用、向量留空」的记忆重新嵌入。
//
// 背景：为了让“用户刚说过的事”不在服务中断时凭空消失，写入路径在嵌入失败时会
// 先把内容存下来（向量留空）。那些记忆暂时只能被关键词检索命中（见 store_lexical.go）。
// 这个文件负责在嵌入服务恢复后把它们补上，让语义检索也能覆盖到。
//
// 触发方式是**自愈式**的：不做定时任务，而是在某次查询成功嵌入之后顺手跑一轮——
// 能嵌入查询就说明服务活着，这时候补算是安全的，也省掉了一个调度器。

const (
	backfillBatch     = 32               // 单批嵌入多少条
	backfillMaxPerRun = 200              // 单轮最多补多少条，避免一次性打爆本地模型
	backfillMinGap    = 10 * time.Minute // 两次补算之间的最小间隔
)

var (
	backfillMu   sync.Mutex
	backfillLast time.Time
)

// maybeBackfill 在“嵌入服务显然可用”时触发一轮补算；非阻塞，失败只记日志。
func (i *Ingester) maybeBackfill() {
	backfillMu.Lock()
	if time.Since(backfillLast) < backfillMinGap {
		backfillMu.Unlock()
		return
	}
	backfillLast = time.Now()
	backfillMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[memory] 补算向量时发生意外（不影响对话）：%v", r)
			}
		}()
		n, err := i.BackfillEmbeddings(context.Background())
		if err != nil {
			log.Printf("[memory] 补算向量失败（不影响对话）：%v", err)
			return
		}
		if n > 0 {
			log.Printf("[memory] 已补算 %d 条缺失向量", n)
		}
	}()
}

// BackfillEmbeddings 把缺向量的块补上，返回补算条数。
// 同步执行，方便测试直接调用；生产路径由 maybeBackfill 在后台跑。
func (i *Ingester) BackfillEmbeddings(ctx context.Context) (int, error) {
	total := 0
	for total < backfillMaxPerRun {
		rows, err := i.store.ChunksMissingEmbedding(ctx, backfillBatch)
		if err != nil {
			return total, err
		}
		if len(rows) == 0 {
			break
		}

		texts := make([]string, 0, len(rows))
		for _, r := range rows {
			texts = append(texts, r.Text)
		}
		vecs, err := i.emb.Embed(ctx, texts)
		if err != nil {
			// 服务又挂了：停下，等下一轮触发。
			return total, err
		}
		if len(vecs) != len(rows) {
			return total, nil // 数量对不上就不冒险写，等下一轮
		}

		for idx, r := range rows {
			if err := i.store.UpdateChunkEmbedding(ctx, r.Id, vecs[idx]); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

// ChunksMissingEmbedding 找出还没算向量的块（dim=0 或向量为空）。
func (s *Store) ChunksMissingEmbedding(ctx context.Context, limit int) ([]struct {
	Id   uint
	Text string
}, error) {
	if limit <= 0 {
		limit = backfillBatch
	}
	var rows []struct {
		Id   uint
		Text string
	}
	err := s.db.WithContext(ctx).
		Table("kb_chunk").
		Select("id, `text`").
		Where("dim = 0 OR embedding IS NULL OR LENGTH(embedding) = 0").
		Order("id asc").Limit(limit).Scan(&rows).Error
	return rows, err
}

// UpdateChunkEmbedding 写入一个块的向量与维度。
func (s *Store) UpdateChunkEmbedding(ctx context.Context, chunkID uint, vec []float32) error {
	if len(vec) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).
		Table("kb_chunk").
		Where("id = ?", chunkID).
		Updates(map[string]any{"embedding": encodeVec(vec), "dim": len(vec)}).Error
}
