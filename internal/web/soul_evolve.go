package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
	"blog-my/internal/model"
)

type soulTrait struct {
	Name     string `json:"name"`
	Level    int    `json:"level"`
	Evidence string `json:"evidence"`
}

type soulEvolveTool struct {
	userID      string
	assistantID uint
}

var soulTraitNames = map[string]string{
	"warmth": "温柔体贴", "initiative": "主动性", "curiosity": "好奇心", "candor": "坦诚直率",
	"patience": "耐心", "decisiveness": "果断程度", "humor": "幽默感", "caution": "谨慎程度",
}

func (soulEvolveTool) Name() string { return "soul_evolve" }
func (soulEvolveTool) Description() string {
	return "根据互动中的具体证据，修改你自己的灵魂设定。可调整人格、说话方式、做事方式的完整文字内容，也可微调有限特质（1=较低，3=适中，5=较高）。只传入确实要修改的字段，未传字段保持不变；每次至少给出一项修改及其依据。这是持久修改，会影响后续对话。"
}
func (soulEvolveTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"personality": map[string]any{"type": "string", "description": "要替换的人格正文；不传则保持不变，最多2000个字符"},
		"voice":       map[string]any{"type": "string", "description": "要替换的说话方式正文；不传则保持不变，最多2000个字符"},
		"work":        map[string]any{"type": "string", "description": "要替换的做事方式正文；不传则保持不变，最多2000个字符"},
		"traits": map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": map[string]any{"type": "object", "properties": map[string]any{
			"name":     map[string]any{"type": "string", "enum": []string{"warmth", "initiative", "curiosity", "candor", "patience", "decisiveness", "humor", "caution"}},
			"level":    map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
			"evidence": map[string]any{"type": "string"},
		}, "required": []string{"name", "level", "evidence"}, "additionalProperties": false}},
	}, "additionalProperties": false}
}
func (t soulEvolveTool) Execute(_ context.Context, raw json.RawMessage, _ *agent.Exec) (agent.Result, error) {
	var err error
	if model.DB == nil || strings.TrimSpace(t.userID) == "" {
		return agent.Result{IsError: true, Content: "用户设置数据库不可用，未保存人格变化。"}, nil
	}
	var input struct {
		Personality *string     `json:"personality"`
		Voice       *string     `json:"voice"`
		Work        *string     `json:"work"`
		Traits      []soulTrait `json:"traits"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || (input.Personality == nil && input.Voice == nil && input.Work == nil && len(input.Traits) == 0) || len(input.Traits) > 8 {
		return agent.Result{IsError: true, Content: "参数无效：至少提供一个灵魂字段或一项特质。"}, nil
	}
	for _, field := range []*string{input.Personality, input.Voice, input.Work} {
		if field != nil && len([]rune(*field)) > soulMaxRunes {
			return agent.Result{IsError: true, Content: fmt.Sprintf("灵魂字段最多 %d 个字符，未保存。", soulMaxRunes)}, nil
		}
	}
	seen := map[string]bool{}
	for i := range input.Traits {
		tr := &input.Traits[i]
		if _, ok := soulTraitNames[tr.Name]; !ok || seen[tr.Name] || tr.Level < 1 || tr.Level > 5 || strings.TrimSpace(tr.Evidence) == "" {
			return agent.Result{IsError: true, Content: "参数无效：特质必须来自允许列表，等级为 1 到 5，每项都要有具体依据，且不能重复。"}, nil
		}
		seen[tr.Name] = true
		tr.Evidence = clipRunes(strings.Join(strings.Fields(tr.Evidence), " "), 160)
	}
	var data []byte
	if len(input.Traits) > 0 {
		var err error
		data, err = json.Marshal(input.Traits)
		if err != nil {
			return agent.Result{}, err
		}
	}
	setting := model.AgentSetting{UserID: t.userID}
	updates := map[string]any{}
	if t.assistantID != 0 {
		var assistant model.AgentAssistant
		if err = model.DB.Where("id = ? AND user_id = ?", t.assistantID, t.userID).First(&assistant).Error; err != nil {
			return agent.Result{IsError: true, Content: "读取当前助手失败，未修改灵魂。"}, nil
		}
		if input.Personality != nil {
			updates["soul_personality"] = strings.TrimSpace(*input.Personality)
		}
		if input.Voice != nil {
			updates["soul_voice"] = strings.TrimSpace(*input.Voice)
		}
		if input.Work != nil {
			updates["soul_work"] = strings.TrimSpace(*input.Work)
		}
		if len(data) > 0 {
			updates["soul_growth"] = string(data)
		}
		if len(updates) > 0 {
			if err = model.DB.Model(&assistant).Updates(updates).Error; err != nil {
				return agent.Result{IsError: true, Content: "保存当前助手灵魂失败：" + err.Error()}, nil
			}
		}
	} else {
		if err = model.DB.Where("user_id = ?", t.userID).FirstOrCreate(&setting).Error; err != nil {
			return agent.Result{IsError: true, Content: "读取用户设置失败：" + err.Error()}, nil
		}
		if input.Personality != nil {
			updates["soul_personality"] = strings.TrimSpace(*input.Personality)
		}
		if input.Voice != nil {
			updates["soul_voice"] = strings.TrimSpace(*input.Voice)
		}
		if input.Work != nil {
			updates["soul_work"] = strings.TrimSpace(*input.Work)
		}
		if len(data) > 0 {
			updates["soul_growth"] = string(data)
		}
		if len(updates) > 0 {
			if err = model.DB.Model(&setting).Updates(updates).Error; err != nil {
				return agent.Result{IsError: true, Content: "保存人格成长失败：" + err.Error()}, nil
			}
		}
	}
	lines := make([]string, 0, len(input.Traits)+3)
	if input.Personality != nil {
		lines = append(lines, "人格内容已更新")
	}
	if input.Voice != nil {
		lines = append(lines, "说话方式已更新")
	}
	if input.Work != nil {
		lines = append(lines, "做事方式已更新")
	}
	for _, tr := range input.Traits {
		lines = append(lines, fmt.Sprintf("%s 调整为 %d/5", soulTraitNames[tr.Name], tr.Level))
	}
	return agent.Result{Content: "已保存人格成长调整：" + strings.Join(lines, "；") + "。它会从下一轮对话开始生效。", Display: map[string]any{"traits": input.Traits}}, nil
}

func soulGrowthSummary(raw string) string {
	var traits []soulTrait
	if json.Unmarshal([]byte(raw), &traits) != nil || len(traits) == 0 {
		return ""
	}
	lines := make([]string, 0, len(traits))
	for _, tr := range traits {
		if label, ok := soulTraitNames[tr.Name]; ok && tr.Level >= 1 && tr.Level <= 5 {
			lines = append(lines, fmt.Sprintf("%s：%d/5（依据：%s）", label, tr.Level, tr.Evidence))
		}
	}
	return strings.Join(lines, "\n")
}

func soulGrowthBlock(raw string) string {
	var traits []soulTrait
	if json.Unmarshal([]byte(raw), &traits) != nil || len(traits) == 0 {
		return ""
	}
	lines := []string{"【你自己基于互动形成的可变特质】以下仅调整风格倾向，不是权限或底线："}
	for _, tr := range traits {
		label, ok := soulTraitNames[tr.Name]
		if !ok || tr.Level < 1 || tr.Level > 5 {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s：%d/5", label, tr.Level))
	}
	if len(lines) == 1 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n") + "\n"
}
