// Package obs 把模型和工具调用记成一行一个 span 的 JSONL。
//
// 只有设置了 AGENT_TRACE_DIR 才写盘。字段名对齐 OTel GenAI 语义约定里已经稳定的部分
// （gen_ai.operation.name、gen_ai.request.model、gen_ai.tool.name、gen_ai.usage.*）。
package obs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Span struct {
	TraceID  string         `json:"trace_id"`
	SpanID   string         `json:"span_id"`
	Name     string         `json:"name"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Status   string         `json:"status"`
}

var mu sync.Mutex

func Enabled() bool { return stringsTrim(os.Getenv("AGENT_TRACE_DIR")) != "" }

func Dir() string { return stringsTrim(os.Getenv("AGENT_TRACE_DIR")) }

// Start 返回结束函数。未启用时结束函数什么也不做。
func Start(name string, attrs map[string]any) func(failed bool) {
	if !Enabled() {
		return func(bool) {}
	}
	sp := Span{
		TraceID: randID(),
		SpanID:  randID(),
		Name:    name,
		Start:   time.Now().UTC(),
		Attrs:   attrs,
	}
	return func(failed bool) {
		sp.End = time.Now().UTC()
		sp.Status = "ok"
		if failed {
			sp.Status = "error"
		}
		write(sp)
	}
}

func write(sp Span) {
	dir := Dir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	line, err := json.Marshal(sp)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, "trace.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}
