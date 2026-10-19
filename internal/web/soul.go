package web

import (
	"strings"

	"blog-my/internal/model"
)

// 灵魂：用户**亲手写给**这个助手的「人格 + 说话方式 + 做事方式」。
//
// 与自动汇总的用户画像的区别很关键：
//   - 画像是系统从对话里猜出来的，可能不准、可能过时；
//   - 灵魂是用户明确设定的，是**指令**，所以优先级最高。
//
// 因此它被放在系统提示词的最前面，并在文案里明确写清「冲突时以它为准」，
// 否则模型很容易被后面的默认风格（我们提示词里那套行为准则）盖过去。

// soulMaxRunes 限制灵魂正文长度：它每轮都注入，必须短。
const soulMaxRunes = 2000

// kbSoulBlock 读取灵魂并拼成提示词块；没设置时返回空串（不留任何痕迹）。
func kbSoulBlock(userID string) string {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return ""
	}
	var s model.AgentSetting
	if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err != nil {
		return ""
	}
	return soulBlockFrom(s.SoulPersonality, s.SoulVoice, s.SoulWork)
}

// soulBlockFrom 把一份「人格 + 说话方式 + 做事方式」拼成提示词块。
//
// 单独抽出来是因为灵魂不止一个来源：用户全局设置、或当前选中的助手
// （见 web/assistant_resolve.go）。两份都是**用户亲手写的指令**，优先级一样高，
// 所以拼装规则只写一份。
func soulBlockFrom(rawPersonality, rawVoice, rawWork string) string {
	personality := clipRunes(strings.TrimSpace(rawPersonality), soulMaxRunes)
	voice := clipRunes(strings.TrimSpace(rawVoice), soulMaxRunes)
	work := clipRunes(strings.TrimSpace(rawWork), soulMaxRunes)
	if personality == "" && voice == "" && work == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("【你的灵魂：人格、说话方式与做事方式】\n")
	b.WriteString("这是用户对你的明确偏好，会塑造你的日常人格与表达。\n")
	b.WriteString("用户当下的明确要求优先；诚实、权限和安全边界始终有效。\n")
	if personality != "" {
		b.WriteString("\n## 人格\n")
		b.WriteString(personality)
		b.WriteString("\n")
	}
	if voice != "" {
		b.WriteString("\n## 说话方式\n")
		b.WriteString(voice)
		b.WriteString("\n")
	}
	if work != "" {
		b.WriteString("\n## 做事方式\n")
		b.WriteString(work)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// soulFromSetting 取出灵魂原文（给接口用，不做格式化）。
func soulFromSetting(s model.AgentSetting) (personality, voice, work string) {
	return strings.TrimSpace(s.SoulPersonality), strings.TrimSpace(s.SoulVoice), strings.TrimSpace(s.SoulWork)
}
