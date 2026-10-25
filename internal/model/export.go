package model

import "encoding/json"

// UserDump 是当前用户可带走的一份 JSON。只含这个用户自己的会话，不含密钥明文。
type UserDump struct {
	Conversations []Conversation `json:"conversations"`
	Messages      []Message      `json:"messages"`
	Settings      []AgentSetting `json:"settings"`
	Wakeups       []AgentWakeup  `json:"wakeups"`
}

func ExportUser(userID string) ([]byte, error) {
	dump := UserDump{}
	if DB == nil || userID == "" {
		return json.Marshal(dump)
	}
	if err := DB.Where("user_id = ?", userID).Find(&dump.Conversations).Error; err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(dump.Conversations))
	for _, c := range dump.Conversations {
		ids = append(ids, c.Id)
	}
	if len(ids) > 0 {
		if err := DB.Where("conversation_id IN ?", ids).Find(&dump.Messages).Error; err != nil {
			return nil, err
		}
	}
	if err := DB.Where("user_id = ?", userID).Find(&dump.Settings).Error; err != nil {
		return nil, err
	}
	if err := DB.Where("user_id = ?", userID).Find(&dump.Wakeups).Error; err != nil {
		return nil, err
	}
	return json.MarshalIndent(dump, "", "  ")
}

// ImportUser 把导出的会话按新 id 写回当前用户。不覆盖已有行。
func ImportUser(userID string, payload []byte) (int, error) {
	var dump UserDump
	if err := json.Unmarshal(payload, &dump); err != nil {
		return 0, err
	}
	if DB == nil || userID == "" {
		return 0, nil
	}
	idMap := map[uint]uint{}
	created := 0
	for _, conv := range dump.Conversations {
		old := conv.Id
		conv.Id = 0
		conv.UserID = userID
		if err := DB.Create(&conv).Error; err != nil {
			return created, err
		}
		idMap[old] = conv.Id
		created++
	}
	for _, msg := range dump.Messages {
		next, ok := idMap[msg.ConversationID]
		if !ok {
			continue
		}
		msg.Id = 0
		msg.ConversationID = next
		if err := DB.Create(&msg).Error; err != nil {
			return created, err
		}
	}
	return created, nil
}
