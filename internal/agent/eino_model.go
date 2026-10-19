package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// retryEinoModel keeps the old modelRound retry boundary: a busy upstream may
// be retried once only before the current model turn has produced a chunk.
// It never replays tool execution or a partially delivered assistant reply.
type retryEinoModel struct {
	inner  model.ToolCallingChatModel
	limit  int
	calls  *atomic.Int64
	budget *runBudget
}

var _ model.ToolCallingChatModel = (*retryEinoModel)(nil)

func (m *retryEinoModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &retryEinoModel{inner: bound, limit: m.limit, calls: m.calls, budget: m.budget}, nil
}

func (m *retryEinoModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if m.budget != nil {
		notice, err := m.budget.reserveModel()
		if err != nil {
			return nil, err
		}
		if notice != "" {
			input = append(append([]*schema.Message(nil), input...), &schema.Message{Role: schema.System, Content: notice})
		}
	}
	if err := m.reserveCall(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		msg, err := m.inner.Generate(ctx, input, opts...)
		if err == nil || attempt > 0 || ctx.Err() != nil || !retryableModelError(err) {
			return msg, err
		}
		if err = waitModelRetry(ctx); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("agent: Eino 模型重试流程异常结束")
}

func (m *retryEinoModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.budget != nil {
		notice, err := m.budget.reserveModel()
		if err != nil {
			return nil, err
		}
		if notice != "" {
			input = append(append([]*schema.Message(nil), input...), &schema.Message{Role: schema.System, Content: notice})
		}
	}
	if err := m.reserveCall(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		source, err := m.inner.Stream(ctx, input, opts...)
		if err == nil && source == nil {
			err = ErrNilStream
		}
		if err == nil {
			// Some providers report 503 from Recv rather than Stream. Prefetch one
			// chunk so both forms share the same safe retry boundary.
			var first *schema.Message
			first, err = source.Recv()
			if err == nil {
				reader, writer := schema.Pipe[*schema.Message](1)
				go func() {
					defer writer.Close()
					defer source.Close()
					if writer.Send(first, nil) {
						return
					}
					for {
						chunk, readErr := source.Recv()
						if errors.Is(readErr, io.EOF) {
							return
						}
						if writer.Send(chunk, readErr) || readErr != nil {
							return
						}
					}
				}()
				return reader, nil
			}
			source.Close()
			if errors.Is(err, io.EOF) {
				return schema.StreamReaderFromArray([]*schema.Message{}), nil
			}
		}
		if attempt > 0 || ctx.Err() != nil || !retryableModelError(err) {
			return nil, err
		}
		if err = waitModelRetry(ctx); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("agent: Eino 模型重试流程异常结束")
}

func (m *retryEinoModel) reserveCall() error {
	if m.calls != nil && m.limit > 0 && m.calls.Add(1) > int64(m.limit) {
		return fmt.Errorf("%w：已达到 %d 轮模型调用上限", ErrMaxIterations, m.limit)
	}
	return nil
}
