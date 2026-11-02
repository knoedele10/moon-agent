package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func isolateQQConns(t *testing.T) {
	t.Helper()
	qqConnsMu.Lock()
	old := qqConns
	qqConns = map[string]*qqConn{}
	qqConnsMu.Unlock()
	t.Cleanup(func() {
		qqConnsMu.Lock()
		qqConns = old
		qqConnsMu.Unlock()
	})
}

func dialQQTestConn(t *testing.T) *websocket.Conn {
	t.Helper()
	isolateQQConns(t)
	gin.SetMode(gin.TestMode)
	t.Setenv("QQ_ONEBOT_ENABLED", "1")
	t.Setenv("QQ_ONEBOT_TOKEN", "secret")
	r := gin.New()
	r.GET("/onebot/v11/ws", qqOneBotWS)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/onebot/v11/ws",
		http.Header{"Authorization": {"Bearer secret"}, "X-Self-ID": {"100"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for qqGetConn("100") == nil {
		if time.Now().After(deadline) {
			t.Fatal("onebot connection was not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ws
}

type qqTestAction struct {
	Action string `json:"action"`
	Echo   string `json:"echo"`
	Params struct {
		UserID  int64 `json:"user_id"`
		GroupID int64 `json:"group_id"`
		Message []struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		} `json:"message"`
	} `json:"params"`
}

func readQQAction(t *testing.T, ws *websocket.Conn) qqTestAction {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame qqTestAction
	if err := ws.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func ackQQAction(t *testing.T, ws *websocket.Conn, echo string, messageID any) {
	t.Helper()
	if err := ws.WriteJSON(map[string]any{
		"status":  "ok",
		"retcode": 0,
		"data":    map[string]any{"message_id": messageID},
		"echo":    echo,
	}); err != nil {
		t.Fatal(err)
	}
}

type qqSendResult struct {
	ref string
	err error
}

func startQQSend(thread model.SAThread, out model.SAOutbound, reply *model.SATrigger) <-chan qqSendResult {
	ch := make(chan qqSendResult, 1)
	go func() {
		ref, err := (saQQChannel{}).Send(context.Background(), thread, out, reply)
		ch <- qqSendResult{ref: ref, err: err}
	}()
	return ch
}

func waitQQSend(t *testing.T, ch <-chan qqSendResult) qqSendResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("qq send did not return")
		return qqSendResult{}
	}
}

func TestSAChannelQQSentRefFromEcho(t *testing.T) {
	ws := dialQQTestConn(t)
	thread := model.SAThread{ThreadKey: "qq:private:200", Source: model.SASourceQQ}
	out := model.SAOutbound{Id: 7, Text: "你好"}
	ch := startQQSend(thread, out, nil)
	frame := readQQAction(t, ws)
	if frame.Action != "send_private_msg" || frame.Params.UserID != 200 {
		t.Fatalf("frame action=%s user=%d", frame.Action, frame.Params.UserID)
	}
	if !strings.HasPrefix(frame.Echo, "sa-7-") {
		t.Fatalf("echo = %q", frame.Echo)
	}
	if len(frame.Params.Message) != 1 || frame.Params.Message[0].Type != "text" || frame.Params.Message[0].Data["text"] != "你好" {
		t.Fatalf("message = %+v", frame.Params.Message)
	}
	ackQQAction(t, ws, frame.Echo, 321)
	res := waitQQSend(t, ch)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.ref != "321" {
		t.Fatalf("sent_ref = %q", res.ref)
	}
}

func TestSAChannelQQSentRefQuotesSourceMsgID(t *testing.T) {
	ws := dialQQTestConn(t)
	thread := model.SAThread{ThreadKey: "qq:group:300", Source: model.SASourceQQ}
	// 超过单条上限，确认引用只挂在第一条，sent_ref 取第一条回执。
	out := model.SAOutbound{Id: 8, Text: strings.Repeat("a", qqMaxChunk) + "b"}
	reply := &model.SATrigger{SourceMsgID: "55"}
	ch := startQQSend(thread, out, reply)

	first := readQQAction(t, ws)
	if first.Action != "send_group_msg" || first.Params.GroupID != 300 {
		t.Fatalf("first action=%s group=%d", first.Action, first.Params.GroupID)
	}
	if len(first.Params.Message) != 2 || first.Params.Message[0].Type != "reply" || first.Params.Message[0].Data["id"] != "55" {
		t.Fatalf("first message = %+v", first.Params.Message)
	}
	if first.Params.Message[1].Type != "text" || first.Params.Message[1].Data["text"] != strings.Repeat("a", qqMaxChunk) {
		t.Fatalf("first text = %+v", first.Params.Message[1])
	}
	ackQQAction(t, ws, first.Echo, "9001")

	second := readQQAction(t, ws)
	if len(second.Params.Message) != 1 || second.Params.Message[0].Type != "text" || second.Params.Message[0].Data["text"] != "b" {
		t.Fatalf("second message = %+v", second.Params.Message)
	}
	ackQQAction(t, ws, second.Echo, "9002")

	res := waitQQSend(t, ch)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.ref != "9001" {
		t.Fatalf("sent_ref = %q, want first message_id", res.ref)
	}
}

func TestSAChannelQQSentRefTimeoutIsTemporary(t *testing.T) {
	ws := dialQQTestConn(t)
	old := qqAckTimeout
	qqAckTimeout = 40 * time.Millisecond
	t.Cleanup(func() { qqAckTimeout = old })

	ch := startQQSend(model.SAThread{ThreadKey: "qq:private:200", Source: model.SASourceQQ}, model.SAOutbound{Id: 9, Text: "等回执"}, nil)
	frame := readQQAction(t, ws)
	if frame.Echo == "" {
		t.Fatal("missing echo")
	}
	res := waitQQSend(t, ch)
	var pe saPermanentError
	if res.err == nil || errors.As(res.err, &pe) || !strings.Contains(res.err.Error(), "timeout") {
		t.Fatalf("timeout should be temporary, got %v", res.err)
	}
}

func TestSAChannelQQSentRefOfflineIsTemporary(t *testing.T) {
	isolateQQConns(t)
	_, err := (saQQChannel{}).Send(context.Background(), model.SAThread{ThreadKey: "qq:private:200", Source: model.SASourceQQ}, model.SAOutbound{Text: "hi"}, nil)
	var pe saPermanentError
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline should be temporary, got %v", err)
	}
}

func TestSAChannelQQSentRefInvalidKeyIsPermanent(t *testing.T) {
	for _, key := range []string{"web:conv:1", "qq:channel:1", "qq:private:", "not-a-key"} {
		_, err := (saQQChannel{}).Send(context.Background(), model.SAThread{ThreadKey: key, Source: model.SASourceQQ}, model.SAOutbound{Text: "hi"}, nil)
		var pe saPermanentError
		if !errors.As(err, &pe) {
			t.Fatalf("key %q should be permanent, got %v", key, err)
		}
	}
}

func TestSAChannelQQSentRefGroupGoneIsPermanent(t *testing.T) {
	ws := dialQQTestConn(t)
	ch := startQQSend(model.SAThread{ThreadKey: "qq:group:300", Source: model.SASourceQQ}, model.SAOutbound{Id: 10, Text: "hi"}, nil)
	frame := readQQAction(t, ws)
	if err := ws.WriteJSON(map[string]any{
		"status":  "failed",
		"retcode": 100,
		"message": "群不存在",
		"echo":    frame.Echo,
	}); err != nil {
		t.Fatal(err)
	}
	res := waitQQSend(t, ch)
	var pe saPermanentError
	if !errors.As(res.err, &pe) {
		t.Fatalf("left group should be permanent, got %v", res.err)
	}
}
