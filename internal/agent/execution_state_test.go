package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestExecutionStateReplacedAndVerificationUpdated(t *testing.T) {
	s := &einoRunState{executed: 1, runToolState: runToolState{lastMutationSeq: 1}}
	msgs := s.rewrite(context.Background(), []*schema.Message{{Role: schema.User, Content: "创建文件"}})
	if !strings.Contains(msgs[len(msgs)-1].Content, "尚未调用核对工具") {
		t.Fatal("missing unverified modification status")
	}
	s.executed = 2
	s.lastVerifySeq = 2
	msgs = s.rewrite(context.Background(), msgs)
	count := 0
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Content, executionStatePrefix) {
			count++
			if !strings.Contains(msg.Content, "已调用核对工具") {
				t.Fatal("stale execution status")
			}
		}
	}
	if count != 1 {
		t.Fatalf("got %d state messages, want 1", count)
	}
}
