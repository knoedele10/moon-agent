package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/model"
)

type saQQChannel struct{}

func (saQQChannel) Send(_ context.Context, thread model.SAThread, out model.SAOutbound, reply *model.SATrigger) (string, error) {
	parts := strings.SplitN(thread.ThreadKey, ":", 3)
	if len(parts) < 3 {
		return "", saPermanentError{errors.New("invalid qq thread key")}
	}
	var conn *qqConn
	qqConnsMu.Lock()
	for _, c := range qqConns {
		conn = c
		break
	}
	qqConnsMu.Unlock()
	if conn == nil {
		return "", errors.New("qq connection is offline")
	}
	action := "send_private_msg"
	baseParams := map[string]any{"user_id": qqNumber(qqID(parts[2]))}
	if parts[1] == "group" {
		action = "send_group_msg"
		baseParams = map[string]any{"group_id": qqNumber(qqID(parts[2]))}
	}
	firstRef := ""
	for i, chunk := range qqSplit(out.Text, qqMaxChunk) {
		msg := []map[string]any{}
		if i == 0 && reply != nil && reply.SourceMsgID != "" {
			msg = append(msg, map[string]any{"type": "reply", "data": map[string]any{"id": reply.SourceMsgID}})
		}
		msg = append(msg, map[string]any{"type": "text", "data": map[string]any{"text": chunk}})
		echo := fmt.Sprintf("sa-%d-%d", out.Id, qqEchoSeq.Add(1))
		waiter := qqWaitRegister(echo)
		params := make(map[string]any, len(baseParams)+1)
		for k, v := range baseParams {
			params[k] = v
		}
		params["message"] = msg
		if err := conn.writeJSON(map[string]any{"action": action, "params": params, "echo": echo}); err != nil {
			qqWaitRemove(echo)
			return "", err
		}
		select {
		case result := <-waiter:
			if result.err != nil {
				return "", result.err
			}
			if result.messageID == "" {
				return "", errors.New("onebot action acknowledgement has no message_id")
			}
			if firstRef == "" {
				firstRef = result.messageID
			}
		case <-time.After(15 * time.Second):
			qqWaitRemove(echo)
			return "", errors.New("onebot action acknowledgement timeout")
		}
	}
	return firstRef, nil
}
func init() { saChannels[model.SASourceQQ] = saQQChannel{} }
