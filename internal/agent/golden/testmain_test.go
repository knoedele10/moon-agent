package golden

import (
	"os"
	"testing"
)

// golden 用例会真正执行 write/edit；把检查点隔离到临时目录，避免污染用户缓存。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "blog-my-golden-checkpoints-*")
	if err == nil {
		_ = os.Setenv("AGENT_CHECKPOINT_DIR", dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
