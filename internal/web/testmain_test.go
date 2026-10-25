package web

import (
	"os"
	"testing"
)

// enableBackgroundCurationForTest 让依赖后台固化的用例临时恢复协程。
// 默认在 TestMain 里关闭，避免与「把 model.DB 临时设为 nil」的用例竞争。
func enableBackgroundCurationForTest(t *testing.T) {
	t.Helper()
	backgroundCurationDisabled.Store(false)
	t.Cleanup(func() { backgroundCurationDisabled.Store(true) })
}

// web 的部分测试会走真实文件工具；检查点隔离到临时目录，避免写开发机用户缓存。
func TestMain(m *testing.M) {
	// 后台固化会读全局 model.DB，而部分用例会把它临时设为 nil；测试期关掉，
	// 避免 `go test -race` 报「测试之间共享全局状态」的假阳性。
	backgroundCurationDisabled.Store(true)
	dir, err := os.MkdirTemp("", "blog-my-web-checkpoints-*")
	if err == nil {
		_ = os.Setenv("AGENT_CHECKPOINT_DIR", dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
