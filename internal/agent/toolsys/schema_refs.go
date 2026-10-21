package toolsys

import (
	"net/url"
	"strings"
)

// inlineSchemaRefs 将文档内 JSON Pointer 引用展开，保留引用旁的描述等覆盖项。
// 只遍历 schema 位置，enum/default 中与 $ref 同名的业务字段不受影响。
func inlineSchemaRefs(value any, root map[string]any, active map[string]bool, depth int) any {
	node, ok := value.(map[string]any)
	if !ok {
		return deepCopyValue(value)
	}
	if depth > 32 {
		return map[string]any{"description": "嵌套结构过深，请遵循工具的字段说明。"}
	}
	out := make(map[string]any, len(node))
	if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#") {
		if !active[ref] {
			if target, ok := schemaPointer(root, ref); ok {
				active[ref] = true
				if expanded, ok := inlineSchemaRefs(target, root, active, depth+1).(map[string]any); ok {
					for key, val := range expanded {
						out[key] = val
					}
				}
				delete(active, ref)
			}
		}
		// 循环引用到此结束；不向后端发送已被移除定义表的悬空引用。
	}
	for key, val := range node {
		if key == "$defs" || key == "definitions" {
			continue
		}
		if key == "$ref" {
			if ref, ok := val.(string); ok && strings.HasPrefix(ref, "#") {
				continue
			}
		}
		switch key {
		case "properties", "patternProperties", "dependentSchemas":
			if props, ok := val.(map[string]any); ok {
				expanded := make(map[string]any, len(props))
				for name, prop := range props {
					expanded[name] = inlineSchemaRefs(prop, root, active, depth+1)
				}
				out[key] = expanded
				continue
			}
		case "items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames":
			out[key] = inlineSchemaRefs(val, root, active, depth+1)
			continue
		case "allOf", "anyOf", "oneOf", "prefixItems":
			if items, ok := val.([]any); ok {
				expanded := make([]any, len(items))
				for i, item := range items {
					expanded[i] = inlineSchemaRefs(item, root, active, depth+1)
				}
				out[key] = expanded
				continue
			}
		}
		out[key] = deepCopyValue(val)
	}
	return out
}

func schemaPointer(root map[string]any, ref string) (any, bool) {
	fragment, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
	if err != nil {
		return nil, false
	}
	if fragment == "" {
		return root, true
	}
	if !strings.HasPrefix(fragment, "/") {
		return nil, false
	}
	var node any = root
	for _, part := range strings.Split(strings.TrimPrefix(fragment, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return node, true
}
