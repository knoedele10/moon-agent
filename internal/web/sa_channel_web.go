package web

import (
	"context"
	"errors"
	"strconv"

	"blog-my/internal/model"
)

type saWebChannel struct{}

// Send 把一条外发写进该 Thread 的前台会话，sent_ref 是 assistant 消息的 id。
// 没有前台会话时重试也不会成功，按永久失败交给发件箱丢弃。
func (saWebChannel) Send(ctx context.Context, thread model.SAThread, out model.SAOutbound, _ *model.SATrigger) (string, error) {
	if thread.ConversationID == 0 {
		return "", saPermanentError{errors.New("web thread has no conversation")}
	}
	if model.DB == nil {
		return "", errors.New("database is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m := model.Message{ConversationID: thread.ConversationID, Role: "assistant", Content: out.Text}
	if err := model.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return "", err
	}
	touchConversation(thread.ConversationID)
	return strconv.FormatUint(uint64(m.Id), 10), nil
}

func init() { saChannels[model.SASourceWeb] = saWebChannel{} }
