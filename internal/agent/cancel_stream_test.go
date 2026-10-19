package agent

import (
	"context"
	"errors"
	"testing"

	"blog-my/internal/llm"
	"github.com/cloudwego/eino/schema"
)

func TestDrainCancellationWinsConcurrentStreamEnd(t *testing.T) {
	for _, doneEvent := range []bool{false, true} {
		for range 100 {
			ctx, cancel := context.WithCancel(context.Background())
			ch := make(chan llm.Event, 1)
			if doneEvent {
				ch <- llm.Event{Type: llm.EventDone}
			}
			close(ch)
			cancel()
			_, _, err := drain(ctx, ch, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("doneEvent=%v: canceled stream returned %v", doneEvent, err)
			}
		}
	}
}

func TestEinoScanCancellationWinsStreamEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := schema.StreamReaderFromArray([]*schema.Message{})
	_, err := scanEinoOutput(&einoRunState{}, ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream returned %v", err)
	}
}
