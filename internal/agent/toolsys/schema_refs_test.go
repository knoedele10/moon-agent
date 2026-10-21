package toolsys

import (
	"encoding/json"
	"strings"
	"testing"
)

// MCP 常用 $defs 引用嵌套对象；模型看到的定义和执行时纠偏必须使用同一结构。
func TestSchemaLocalReferencesExpandBeforeCoercion(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"Count/Value": map[string]any{"type": "integer"},
			"Envelope": map[string]any{"type": "object", "properties": map[string]any{
				"count": map[string]any{"$ref": "#/$defs/Count~1Value"},
			}, "required": []string{"count"}},
		},
		"properties": map[string]any{"data": map[string]any{"$ref": "#/$defs/Envelope", "description": "输入对象"}},
		"required":   []string{"data"},
	}
	inner := &fakeTool{name: "nested", params: schema}
	wrapped := Decorate(inner)
	params, _ := json.Marshal(wrapped.Parameters())
	if strings.Contains(string(params), `"$ref"`) || strings.Contains(string(params), `"$defs"`) {
		t.Fatalf("不应保留悬空引用：%s", params)
	}
	result := runTool(t, wrapped, map[string]any{"data": map[string]any{"count": "42"}})
	if result.IsError {
		t.Fatalf("引用字段应正常纠偏：%+v", result)
	}
	data := inner.lastArgs(t)["data"].(map[string]any)
	if data["count"] != float64(42) {
		t.Fatalf("引用中的整数未纠偏：%+v", data)
	}
	result = runTool(t, wrapped, map[string]any{"data": map[string]any{}})
	if !result.IsError {
		t.Fatal("引用中的必填字段应继续校验")
	}
	if _, ok := schema["$defs"]; !ok {
		t.Fatal("不得修改工具原始 schema")
	}
}

func TestSchemaRecursiveReferencesAreBounded(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"$defs": map[string]any{"Node": map[string]any{
			"type": "object", "properties": map[string]any{"next": map[string]any{"$ref": "#/$defs/Node"}},
		}},
		"properties": map[string]any{"root": map[string]any{"$ref": "#/$defs/Node"}},
	}
	data, err := json.Marshal(sanitizeSchema(schema))
	if err != nil || len(data) > 1000 || strings.Contains(string(data), `"$ref"`) {
		t.Fatalf("递归引用应有界且没有悬空引用：%s %v", data, err)
	}
}
