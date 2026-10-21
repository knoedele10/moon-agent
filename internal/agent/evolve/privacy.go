package evolve

import (
	"encoding/json"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var credentialTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})`),
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9_+/=.-]{8,}`),
	regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|token|cookie)\b["']?\s*[:=]\s*["']?[^\s,"';&}]+`),
}

// Redact 在旁路存储前移除已识别的凭证；错误摘要和运行备注也走同一条路径。
func Redact(text string) string {
	for _, pattern := range credentialTextPatterns {
		text = pattern.ReplaceAllString(text, redacted)
	}
	return text
}

// RedactArguments 保留合法 JSON 结构，以便后续提炼真实工具参数；敏感键无论值的
// 长短和类型都移除，避免仅靠密钥前缀漏掉私有服务的短密码。
func RedactArguments(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return Redact(raw)
	}
	value = redactValue(value)
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func redactValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			if strings.Contains(compact, "password") || strings.Contains(compact, "passwd") ||
				strings.Contains(compact, "secret") || strings.Contains(compact, "token") ||
				strings.Contains(compact, "apikey") || strings.Contains(compact, "authorization") ||
				strings.Contains(compact, "cookie") || strings.Contains(compact, "privatekey") || compact == "credential" {
				item[key] = redacted
			} else {
				item[key] = redactValue(child)
			}
		}
	case []any:
		for i := range item {
			item[i] = redactValue(item[i])
		}
	case string:
		return Redact(item)
	}
	return value
}
