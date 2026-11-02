package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-my/internal/model"
)

// qqAckTimeout 是等待 OneBot echo 回执的时间。超时是临时失败，发件箱会重试。
// 测试可以把它调短；生产保持 15s。
var qqAckTimeout = 15 * time.Second

type saQQChannel struct{}

func (saQQChannel) Send(ctx context.Context, thread model.SAThread, out model.SAOutbound, reply *model.SATrigger) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	action, baseParams, err := qqThreadTarget(thread.ThreadKey)
	if err != nil {
		return "", err
	}
	conn := qqAnyConn()
	if conn == nil {
		// 没有在线连接是临时失败：OneBot 重连后发件箱再试。
		return "", errors.New("qq connection is offline")
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
		ref, err := qqWaitAck(ctx, echo, waiter)
		if err != nil {
			return "", err
		}
		if firstRef == "" {
			firstRef = ref
		}
	}
	return firstRef, nil
}

// qqThreadTarget 把 ThreadKey 译成 OneBot 动作。键不合法是永久失败。
func qqThreadTarget(key string) (action string, params map[string]any, err error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "qq" || strings.TrimSpace(parts[2]) == "" {
		return "", nil, saPermanentError{errors.New("invalid qq thread key")}
	}
	switch parts[1] {
	case "private":
		return "send_private_msg", map[string]any{"user_id": qqNumber(qqID(parts[2]))}, nil
	case "group":
		return "send_group_msg", map[string]any{"group_id": qqNumber(qqID(parts[2]))}, nil
	default:
		return "", nil, saPermanentError{errors.New("invalid qq thread key")}
	}
}

func qqAnyConn() *qqConn {
	qqConnsMu.Lock()
	defer qqConnsMu.Unlock()
	for _, c := range qqConns {
		if c != nil {
			return c
		}
	}
	return nil
}

func qqWaitAck(ctx context.Context, echo string, waiter <-chan qqActionResponse) (string, error) {
	timer := time.NewTimer(qqAckTimeout)
	defer timer.Stop()
	select {
	case result := <-waiter:
		if result.err != nil {
			return "", qqClassifySendError(result.err)
		}
		if result.messageID == "" {
			return "", errors.New("onebot action acknowledgement has no message_id")
		}
		return result.messageID, nil
	case <-timer.C:
		qqWaitRemove(echo)
		return "", errors.New("onebot action acknowledgement timeout")
	case <-ctx.Done():
		qqWaitRemove(echo)
		return "", ctx.Err()
	}
}

// qqClassifySendError 把「群已经不在」一类错误标成永久失败。
// 超时、断线和含糊的 retcode 保持临时失败，交给发件箱重试。
func qqClassifySendError(err error) error {
	if err == nil {
		return nil
	}
	var pe saPermanentError
	if errors.As(err, &pe) {
		return err
	}
	msg := strings.ToLower(err.Error())
	for _, hint := range []string{
		"群不存在",
		"不在该群",
		"不在群",
		"已退群",
		"已退出群",
		"群已退出",
		"group not found",
		"not in group",
		"bot was kicked",
	} {
		if strings.Contains(msg, strings.ToLower(hint)) {
			return saPermanentError{err}
		}
	}
	return err
}

func init() { saChannels[model.SASourceQQ] = saQQChannel{} }
