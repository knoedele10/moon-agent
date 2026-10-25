package skill

import "strings"

// Meta 是 SKILL.md 里除正文以外、给分发和权限声明用的字段。
// ParseMarkdown 的签名保持不变；需要版本和工具声明时用 ParseMeta。
type Meta struct {
	Name         string
	Description  string
	Version      string
	Source       string
	AllowedTools []string
}

// ParseMeta 读取 name、description，以及单行的 version、source、allowed-tools。
// 缩进行留给块标量，不当成新字段。
func ParseMeta(raw string) Meta {
	name, desc, _ := ParseMarkdown(raw)
	meta := Meta{Name: name, Description: desc}
	front := frontmatterBody(raw)
	for _, line := range strings.Split(front, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitFrontKey(line)
		if !ok || val == "" || val == ">" || val == "|" || strings.HasPrefix(val, ">") || strings.HasPrefix(val, "|") {
			continue
		}
		switch key {
		case "version":
			meta.Version = val
		case "source":
			meta.Source = val
		case "allowed-tools", "allowed_tools":
			meta.AllowedTools = splitTools(val)
		}
	}
	return meta
}

func frontmatterBody(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "---") {
		return ""
	}
	rest := trimmed[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return ""
	}
	return rest[:idx]
}

func splitFrontKey(line string) (key, val string, ok bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	val = strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
	return key, val, key != ""
}

func splitTools(val string) []string {
	val = strings.TrimSpace(val)
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	var out []string
	for _, part := range strings.FieldsFunc(val, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
