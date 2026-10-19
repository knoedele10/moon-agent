// Package skill 负责解析 SKILL.md 的 frontmatter。
//
// 单独成包是为了让 web 层与导入脚本共用同一份解析逻辑——两边各写一份必然会漂移。
package skill

import "strings"

// ParseMarkdown 从 SKILL.md 文本里抽出 name / description / 正文。
//
// 只认最简 frontmatter（`---` 包起来的 name: / description:），不引 YAML 解析器：
// 技能文件实际只用到这两个字段。但要正确处理 **YAML 块标量**——
// gstack 那批技能的写法是：
//
//	description: >
//	  MUST USE when the user asks to ...
//	  第二行说明
//
// 早期版本把 `>` 当成了描述内容（界面上显示成一个竖线），这里按缩进把后续行拼起来。
func ParseMarkdown(raw string) (name, desc, body string) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "---") {
		rest := trimmed[3:]
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			front := rest[:idx]
			body = strings.TrimSpace(rest[idx+4:])
			name, desc = parseFrontmatter(front)
		}
	}
	if name == "" {
		name = fallbackName(trimmed)
	}
	if body == "" {
		body = trimmed
	}
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	if len([]rune(desc)) > 1000 {
		desc = string([]rune(desc)[:1000])
	}
	return name, strings.TrimSpace(desc), body
}

// fallbackName 在没有 name 字段时挑一个像人写的名字。
//
// 顺序：第一个 markdown 标题 → 第一个既不是 frontmatter 分隔符、也不像
// `key: value` 的非空行。**必须跳过 `---`**：早期版本直接取首个非空行，
// 结果没有 name 字段的技能全叫 "---"（技能名是模型找技能的钥匙，不能是它）。
func fallbackName(s string) string {
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			if v := strings.TrimSpace(strings.TrimLeft(t, "#")); v != "" {
				return v
			}
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || t == "---" || strings.HasPrefix(t, "<!--") {
			continue
		}
		if i := strings.Index(t, ":"); i > 0 && !strings.ContainsAny(t[:i], " ：") {
			continue // 像 frontmatter 的 key: value
		}
		return t
	}
	return ""
}

// parseFrontmatter 解析 key: value 行，支持 `>` / `|` 块标量。
func parseFrontmatter(front string) (name, desc string) {
	lines := strings.Split(front, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		var key string
		switch {
		case strings.HasPrefix(lower, "name:"):
			key = "name"
		case strings.HasPrefix(lower, "description:"):
			key = "desc"
		default:
			continue
		}

		val := strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:])
		// 块标量：把后续缩进更深的行拼起来，直到遇到新的顶层 key。
		if val == ">" || val == "|" || val == ">-" || val == "|-" || val == ">+" || val == "|+" {
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if strings.TrimSpace(next) == "" {
					parts = append(parts, "")
					continue
				}
				// 顶层 key 没有前导空格 → 块结束。
				if next == strings.TrimLeft(next, " \t") {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
				i = j
			}
			val = strings.TrimSpace(strings.Join(parts, " "))
		} else {
			val = strings.Trim(val, `"'`)
		}

		switch key {
		case "name":
			name = val
		case "desc":
			desc = val
		}
	}
	return name, desc
}
