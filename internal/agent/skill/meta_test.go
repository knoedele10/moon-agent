package skill

import "testing"

func TestParseMetaExtraFields(t *testing.T) {
	raw := "---\nname: demo\ndescription: >\n  用来整理周报\nversion: 1.2.0\nsource: claude\nallowed-tools: Read, Write\n---\n\n正文\n"
	meta := ParseMeta(raw)
	if meta.Name != "demo" || meta.Description != "用来整理周报" {
		t.Fatalf("name/description = %#v", meta)
	}
	if meta.Version != "1.2.0" || meta.Source != "claude" {
		t.Fatalf("version/source = %#v", meta)
	}
	if len(meta.AllowedTools) != 2 || meta.AllowedTools[0] != "Read" || meta.AllowedTools[1] != "Write" {
		t.Fatalf("allowed-tools = %#v", meta.AllowedTools)
	}
	name, desc, body := ParseMarkdown(raw)
	if name != "demo" || desc != "用来整理周报" || body != "正文" {
		t.Fatalf("ParseMarkdown 被额外字段影响：%q %q %q", name, desc, body)
	}
}
