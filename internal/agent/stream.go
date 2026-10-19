package agent

import (
	"context"
	"sync"

	"blog-my/internal/llm"
)

// StreamFunc 是 agent 与模型后端之间**唯一**的缝，签名与 (*llm.Registry).Stream 完全一致。
//
// 为什么需要它：契约 §3 的 Config 与 Run 签名是冻结的，里面没有模型调用入口；
// 而主循环又必须能在**不依赖真实 llm 后端**的情况下做确定性单测（契约 §9 验收要求）。
// 于是把「谁来调模型」抽成一个可替换的函数：生产环境由 web 接线层装 llm.Registry.Stream，
// 测试里装一个脚本化假模型。这是纯追加，不改 llm 包、不改 Config/Run 的签名与语义。
type StreamFunc func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error)

var (
	streamMu sync.RWMutex
	streamFn StreamFunc
)

// SetStreamer 安装模型调用实现。传 nil 表示卸载（主循环会返回 ErrNoStreamer）。
//
// **进程级单例，仅应在启动时设置一次**：它挂在包级变量上，不是逐请求的开关；
// 生产侧由 web 启动时装一次即可。测试内若需要并发用例，请自行串行
// （或在用例内按需安装/卸载），不要靠它在请求之间切换。
func SetStreamer(f StreamFunc) {
	streamMu.Lock()
	defer streamMu.Unlock()
	streamFn = f
}

// Streamer 返回当前安装的实现；未安装时为 nil。
func Streamer() StreamFunc {
	streamMu.RLock()
	defer streamMu.RUnlock()
	return streamFn
}

// RegistryStreamer 把 *llm.Registry 适配成 StreamFunc，供 web 接线层一行装上。
func RegistryStreamer(r *llm.Registry) StreamFunc {
	return func(ctx context.Context, modelID, apiKey string, req llm.ChatRequest) (<-chan llm.Event, error) {
		if r == nil {
			return nil, ErrNoStreamer
		}
		return r.Stream(ctx, modelID, apiKey, req)
	}
}
