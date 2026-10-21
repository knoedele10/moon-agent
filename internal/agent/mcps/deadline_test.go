package mcps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// 对端连请求都不读时，超时也必须覆盖管道写入，不能永久阻塞。
func TestCallDeadlineIncludesBlockedWrite(t *testing.T) {
	conn, peer := net.Pipe()
	client := NewClient(conn, ClientOptions{CallTimeout: 40 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	done := make(chan error, 1)
	go func() { _, err := client.Call(context.Background(), "blocked", nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "超时") {
			t.Fatalf("应返回可识别的超时，收到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("写入没有受到请求期限限制")
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("超时后残留 %d 个等待请求", pending)
	}
}

// 已取消的请求不得写入传输，更不能在服务器执行副作用。
func TestCanceledCallDoesNotWrite(t *testing.T) {
	conn, peer := net.Pipe()
	client := NewClient(conn, ClientOptions{})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Call(ctx, "should-not-run", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应保留取消原因，收到 %v", err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	buf := make([]byte, 1)
	if n, _ := peer.Read(buf); n != 0 {
		t.Fatal("已取消请求仍写入了传输")
	}
}

// initialize 已响应、随后不读取 initialized 通知也不能挂死整个握手。
func TestInitializeDeadlineIncludesNotification(t *testing.T) {
	conn, peer := net.Pipe()
	client := NewClient(conn, ClientOptions{InitTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	go func() {
		sc := bufio.NewScanner(peer)
		if !sc.Scan() {
			return
		}
		var req rpcRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			return
		}
		_, _ = fmt.Fprintf(peer, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"serverInfo\":{\"name\":\"blocked\"}}}\n", req.ID)
	}()
	done := make(chan error, 1)
	go func() { done <- client.Initialize(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("应返回通知超时，收到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialized 通知没有受到握手期限限制")
	}
}
