package toolsys

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// TestSanitizeParameters 契约 §3.1：净化掉非标准键、补齐对象形态、折叠可空联合类型。
func TestSanitizeParameters(t *testing.T) {
	original := map[string]any{
		"$schema":   "https://json-schema.org/draft/2020-12/schema",
		"$id":       "tool://x",
		"$defs":     map[string]any{"X": map[string]any{"type": "string"}},
		"invariant": true,
		"type":      "object",
		"allOf":     []any{map[string]any{"required": []string{"query"}}},
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "$comment": "别管我"},
			"mode":  map[string]any{"enum": []string{"pretty", "minify"}},
			"tags":  map[string]any{"type": []any{"string", "null"}},
			"level": map[string]any{"properties": map[string]any{
				"x": map[string]any{"type": "integer"},
			}},
		},
		"required": []any{"query", "", 42},
	}
	params := Sanitize(&fakeTool{name: "x", params: original}).Parameters()

	if params["type"] != "object" {
		t.Fatalf("顶层 type=%v，期望 object", params["type"])
	}
	for _, key := range []string{"$schema", "$id", "$defs", "invariant", "allOf"} {
		if _, exists := params[key]; exists {
			t.Fatalf("非标准键 %q 应被净化掉", key)
		}
	}
	if got, want := params["required"], []string{"query"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required=%#v，期望 %#v", got, want)
	}
	props := params["properties"].(map[string]any)
	query := props["query"].(map[string]any)
	if _, exists := query["$comment"]; exists {
		t.Fatal("属性上的 $comment 应被净化掉")
	}
	tags := props["tags"].(map[string]any)
	if tags["type"] != "string" || tags["nullable"] != true {
		t.Fatalf("可空联合类型应折叠成 type=string + nullable=true，实际 %#v", tags)
	}
	if mode := props["mode"].(map[string]any); mode["type"] != "string" {
		t.Fatalf("enum-only 属性应推断出 type=string，实际 %#v", mode)
	}
	if level := props["level"].(map[string]any); level["type"] != "object" {
		t.Fatalf("含 properties 的属性应推断出 type=object，实际 %#v", level)
	}

	// 净化不能改动工具自己持有的原始 schema（深拷贝）。
	if _, exists := original["$schema"]; !exists {
		t.Fatal("Sanitize 不应修改原 schema")
	}
	// 每次返回新副本，调用方改动不会污染下一次。
	params["properties"].(map[string]any)["query"].(map[string]any)["type"] = "integer"
	again := Sanitize(&fakeTool{name: "x", params: original}).Parameters()
	if again["properties"].(map[string]any)["query"].(map[string]any)["type"] != "string" {
		t.Fatal("Parameters 每次应返回一份独立副本")
	}
}

// TestSanitizeNilParameters nil schema 补成最小合法对象。
func TestSanitizeNilParameters(t *testing.T) {
	tool := &fakeTool{name: "noop"}
	tool.params = nil
	params := Sanitize(tool).Parameters()
	if params["type"] != "object" {
		t.Fatalf("nil schema 应补 type=object，实际 %#v", params)
	}
	if props, ok := params["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Fatalf("nil schema 应补空 properties，实际 %#v", params["properties"])
	}
}

// TestCoerceRepairsArguments 契约 §7.4 的纠偏成功路径。
func TestCoerceRepairsArguments(t *testing.T) {
	schema := objectSchema(map[string]any{
		"filters": map[string]any{"type": "object", "properties": map[string]any{
			"level": map[string]any{"type": "string"},
		}},
		"limit":   map[string]any{"type": "integer"},
		"enabled": map[string]any{"type": "boolean"},
		"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"note":    map[string]any{"type": "string"},
	})
	tool := &fakeTool{name: "probe", params: schema}
	coerced := Coerce(tool)

	cases := []struct {
		name    string
		raw     string
		want    func(t *testing.T, args map[string]any)
		wantRaw string // 非空时断言工具收到的原始参数被原样放行
	}{
		{
			name: "字符串化 JSON 对象还原成对象",
			raw:  `{"filters":"{\"level\":\"high\"}"}`,
			want: func(t *testing.T, args map[string]any) {
				filters, ok := args["filters"].(map[string]any)
				if !ok || filters["level"] != "high" {
					t.Fatalf("filters=%#v，期望对象 {level:high}", args["filters"])
				}
			},
		},
		{
			name: "数字字符串转数字",
			raw:  `{"limit":"42"}`,
			want: func(t *testing.T, args map[string]any) {
				if args["limit"] != float64(42) {
					t.Fatalf("limit=%#v，期望 42", args["limit"])
				}
			},
		},
		{
			name: "布尔字符串转布尔",
			raw:  `{"enabled":"true"}`,
			want: func(t *testing.T, args map[string]any) {
				if args["enabled"] != true {
					t.Fatalf("enabled=%#v，期望 true", args["enabled"])
				}
			},
		},
		{
			name: "JSON 数组字符串还原成数组",
			raw:  `{"tags":"[\"a\",\"b\"]"}`,
			want: func(t *testing.T, args map[string]any) {
				tags, ok := args["tags"].([]any)
				if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
					t.Fatalf("tags=%#v，期望 [a b]", args["tags"])
				}
			},
		},
		{
			name: "裸字符串补成单元素数组",
			raw:  `{"tags":"alpha"}`,
			want: func(t *testing.T, args map[string]any) {
				tags, ok := args["tags"].([]any)
				if !ok || len(tags) != 1 || tags[0] != "alpha" {
					t.Fatalf("tags=%#v，期望 [alpha]", args["tags"])
				}
			},
		},
		{
			name: "类型匹配的字符串保持原样",
			raw:  `{"note":"42"}`,
			want: func(t *testing.T, args map[string]any) {
				if args["note"] != "42" {
					t.Fatalf("note=%#v，期望字符串 42（schema 是 string，不能转数字）", args["note"])
				}
			},
		},
		{
			name: "无法解析的 JSON 数组字符串补成单元素数组",
			raw:  `{"tags":"[oops"}`,
			want: func(t *testing.T, args map[string]any) {
				// 与 hermes 一致：看起来像数组但解析不了时退化为单元素列表，而不是丢参数。
				tags, ok := args["tags"].([]any)
				if !ok || len(tags) != 1 || tags[0] != "[oops" {
					t.Fatalf("tags=%#v，期望单元素数组 [\"[oops\"]", args["tags"])
				}
			},
		},
		{
			name:    "非法 JSON 原样放行给 Validate 报错",
			raw:     `{oops`,
			wantRaw: `{oops`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool.calls = nil
			res, err := coerced.Execute(context.Background(), json.RawMessage(tc.raw), &agent.Exec{})
			if err != nil {
				t.Fatalf("Coerce 不应返回 error: %v", err)
			}
			if res.IsError {
				t.Fatalf("Coerce 包装后不应失败: %+v", res)
			}
			if tc.wantRaw != "" {
				if got := tool.lastRaw(t); got != tc.wantRaw {
					t.Fatalf("原始参数应原样放行，收到 %q", got)
				}
				return
			}
			tc.want(t, tool.lastArgs(t))
		})
	}
}

// TestValidateRepairsMissingRequired 契约 §7.4：缺必填得到可修复报错，且工具不被执行。
func TestValidateRepairsMissingRequired(t *testing.T) {
	schema := objectSchema(map[string]any{
		"query": map[string]any{"type": "string"},
		"limit": map[string]any{"type": "integer"},
	}, "query")
	tool := &fakeTool{name: "search_probe", params: schema}
	validated := Validate(tool)

	for _, raw := range []string{"", "{}", `{"limit":5}`} {
		res, err := validated.Execute(context.Background(), json.RawMessage(raw), &agent.Exec{})
		if err != nil {
			t.Fatalf("Validate 不应返回 error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("raw=%q 应判失败: %+v", raw, res)
		}
		if code := displayCode(res); code != "TOOL_ARG_MISSING" {
			t.Fatalf("raw=%q code=%q，期望 TOOL_ARG_MISSING", raw, code)
		}
		for _, want := range []string{"query", "必填", "请补上"} {
			if !strings.Contains(res.Content, want) {
				t.Fatalf("raw=%q 报错缺 %q：%s", raw, want, res.Content)
			}
		}
	}
	if tool.callCount() != 0 {
		t.Fatalf("参数不合法时工具不应被执行，实际调用 %d 次", tool.callCount())
	}
}

// TestValidateRepairsWrongTypeAndEnum 契约 §7.4：类型/枚举错误要讲清期望、实际与改法。
func TestValidateRepairsWrongTypeAndEnum(t *testing.T) {
	schema := objectSchema(map[string]any{
		"limit": map[string]any{"type": "integer"},
		"mode":  map[string]any{"type": "string", "enum": []string{"pretty", "minify", "validate"}},
		"deep": map[string]any{"type": "object", "properties": map[string]any{
			"level": map[string]any{"type": "integer"},
		}, "required": []string{"level"}},
	})
	tool := &fakeTool{name: "probe", params: schema}
	validated := Validate(tool)

	cases := []struct {
		name     string
		raw      string
		wantCode string
		wants    []string
	}{
		{name: "类型错误", raw: `{"limit":"abc"}`, wantCode: "TOOL_ARG_TYPE", wants: []string{"limit", "integer", "字符串", "请把"}},
		{name: "枚举错误", raw: `{"mode":"fancy"}`, wantCode: "TOOL_ARG_ENUM", wants: []string{"mode", "pretty", "允许"}},
		{name: "嵌套必填", raw: `{"deep":{}}`, wantCode: "TOOL_ARG_MISSING", wants: []string{"deep", "level", "必填"}},
		{name: "嵌套类型", raw: `{"deep":{"level":"x"}}`, wantCode: "TOOL_ARG_TYPE", wants: []string{"deep.level", "整数"}},
		{name: "非法 JSON", raw: `{oops`, wantCode: "TOOL_ARG_JSON", wants: []string{"JSON"}},
		{name: "非对象", raw: `null`, wantCode: "TOOL_ARG_OBJECT", wants: []string{"对象"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := validated.Execute(context.Background(), json.RawMessage(tc.raw), &agent.Exec{})
			if err != nil {
				t.Fatalf("Validate 不应返回 error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("raw=%q 应判失败: %+v", tc.raw, res)
			}
			if code := displayCode(res); code != tc.wantCode {
				t.Fatalf("raw=%q code=%q，期望 %q（文案：%s）", tc.raw, code, tc.wantCode, res.Content)
			}
			for _, want := range tc.wants {
				if !strings.Contains(res.Content, want) {
					t.Fatalf("raw=%q 报错缺 %q：%s", tc.raw, want, res.Content)
				}
			}
		})
	}
	if tool.callCount() != 0 {
		t.Fatalf("参数不合法时工具不应被执行，实际 %d 次", tool.callCount())
	}
}

// TestValidateAcceptsValidArgs 合法参数（含空字符串、可选字段缺失）必须放行。
func TestValidateAcceptsValidArgs(t *testing.T) {
	schema := objectSchema(map[string]any{
		"path":    map[string]any{"type": "string"},
		"content": map[string]any{"type": "string"},
		"force":   map[string]any{"type": "boolean"},
	}, "path", "content")
	tool := &fakeTool{name: "write_probe", params: schema}
	res, err := Validate(tool).Execute(context.Background(),
		json.RawMessage(`{"path":"a.md","content":""}`), &agent.Exec{})
	if err != nil || res.IsError {
		t.Fatalf("合法参数应放行：res=%+v err=%v", res, err)
	}
	if tool.callCount() != 1 {
		t.Fatalf("合法参数应执行一次，实际 %d", tool.callCount())
	}
}

// TestDecoratePipeline Full pipeline：先纠偏再校验，最后才执行。
func TestDecoratePipeline(t *testing.T) {
	schema := objectSchema(map[string]any{
		"query": map[string]any{"type": "string"},
		"limit": map[string]any{"type": "integer"},
	}, "query")
	tool := &fakeTool{name: "probe", params: schema}
	decorated := Decorate(tool)

	res, err := decorated.Execute(context.Background(), json.RawMessage(`{"query":"x","limit":"7"}`), &agent.Exec{})
	if err != nil || res.IsError {
		t.Fatalf("纠偏后应成功：res=%+v err=%v", res, err)
	}
	if args := tool.lastArgs(t); args["limit"] != float64(7) {
		t.Fatalf("工具收到的 limit=%#v，期望数字 7", args["limit"])
	}

	// 纠偏修不了的类型错误，由 Validate 给出可修复文案，且工具不执行。
	tool.calls = nil
	res, err = decorated.Execute(context.Background(), json.RawMessage(`{"query":"x","limit":"abc"}`), &agent.Exec{})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.IsError || displayCode(res) != "TOOL_ARG_TYPE" {
		t.Fatalf("应报 TOOL_ARG_TYPE: %+v", res)
	}
	if tool.callCount() != 0 {
		t.Fatalf("校验失败时工具不应执行，实际 %d", tool.callCount())
	}
}

// TestDecoratorNilAndPassThrough nil 工具安全，正常结果/错误原样透传。
func TestDecoratorNilAndPassThrough(t *testing.T) {
	if Sanitize(nil) != nil || Coerce(nil) != nil || Validate(nil) != nil || Decorate(nil) != nil {
		t.Fatal("nil 工具应返回 nil")
	}
	inner := &fakeTool{name: "boom", exec: func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
		return agent.Result{IsError: true, Content: "业务失败", Display: map[string]any{"code": "BOOM"}}, nil
	}}
	res, err := Validate(inner).Execute(context.Background(), json.RawMessage(`{}`), &agent.Exec{})
	if err != nil || !res.IsError || res.Content != "业务失败" {
		t.Fatalf("工具自身的 is_error 结果应原样透传：res=%+v err=%v", res, err)
	}
}
