package agent

import (
	"context"
	"time"
)

func retryableModelError(err error) bool {
	if err == nil {
		return false
	}
	cat := classifyToolError(err.Error())
	return cat == CatTransient || cat == CatRateLimited || cat == CatTimeout
}

// The engines retain their own streaming retry boundary; only the backoff and
// cancellation behavior are shared.
func waitModelRetry(ctx context.Context) error {
	timer := time.NewTimer(600 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
