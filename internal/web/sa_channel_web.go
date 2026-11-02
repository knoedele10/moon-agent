package web

import (
	"blog-my/internal/model"
	"context"
	"errors"
	"strconv"
)

type saWebChannel struct{}

func (saWebChannel) Send(_ context.Context, thread model.SAThread, out model.SAOutbound, _ *model.SATrigger) (string, error) {
	if thread.ConversationID == 0 {
		return "", errors.New("web thread has no conversation")
	}
	var m = model.Message{ConversationID: thread.ConversationID, Role: "assistant", Content: out.Text}
	if err := model.DB.Create(&m).Error; err != nil {
		return "", err
	}
	return fmtUint(m.Id), nil
}
func fmtUint(v uint) string { return strconv.FormatUint(uint64(v), 10) }
func init()                 { saChannels[model.SASourceWeb] = saWebChannel{} }
