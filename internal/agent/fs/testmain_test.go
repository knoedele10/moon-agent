package fs

import (
	"os"
	"testing"
)

// 所有 fs 测试共用一个临时检查点目录：写类工具现在会自动留副本，
// 不隔离的话跑一次测试就会往开发机的用户缓存里写东西。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "blog-my-fs-checkpoints-*")
	if err == nil {
		_ = os.Setenv(checkpointEnvDir, dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
