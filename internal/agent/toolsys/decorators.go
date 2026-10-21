package toolsys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"blog-my/internal/agent"
)

// Decorate 给工具套上契约 §3.1 的三层装饰器（净化 → 纠偏 → 预校验）。
//
// 执行顺序：先按净化后的 schema 纠偏参数，再按同一份 schema 预校验，
// 最后才交给工具本体；失败文案只讲「哪个字段、期望什么、收到什么、怎么改」。
// Catalog 在登记工具时会自动调用它，接线层无需重复包装（重复包装也无副作用）。
func Decorate(t agent.Tool) agent.Tool {
	return Sanitize(Coerce(Validate(t)))
}

// Sanitize 返回一个 Parameters 已净化的工具包装。
//
// 净化只做「让各家后端都收得下」的兼容性修正，不改业务语义：
//   - 去掉 $schema/$id/$defs/definitions/invariant 等非标准键（有的后端直接 400）；
//   - 顶层强制 type=object、properties 存在、required 是字符串数组；
//   - 递归清理属性 schema，把 `type: ["string","null"]` 折叠成 `type: "string", nullable: true`；
//   - 去掉顶层 allOf/anyOf/oneOf/if/then/else 组合器（本包预校验只按 properties/required 走）。
func Sanitize(t agent.Tool) agent.Tool {
	if t == nil {
		return nil
	}
	return sanitizedTool{inner: t}
}

// Coerce 返回一个执行前做参数纠偏的工具包装。
//
// 纠偏是「保守修复」：只在 schema 明确期望某种类型、且转换结果无歧义时才改；
// 改不动就原样放行，让 Validate 或工具本体给出更具体的报错。
// 覆盖契约要求的 JSON 字符串→对象、数字字符串→数字、布尔字符串→布尔，
// 另外把「期望数组却收到裸标量」补成单元素数组。
func Coerce(t agent.Tool) agent.Tool {
	if t == nil {
		return nil
	}
	return coercedTool{inner: t}
}

// Validate 返回一个执行前按 schema 预校验的工具包装。
//
// 校验 required/type/enum（含一层嵌套对象与数组元素），失败返回 IsError=true 的
// 可修复文案，**不**以 error 返回——与主循环「失败也回灌成工具结果」的语义一致。
func Validate(t agent.Tool) agent.Tool {
	if t == nil {
		return nil
	}
	return validatedTool{inner: t}
}

// sanitizedTool 是 Sanitize 的包装类型。
type sanitizedTool struct{ inner agent.Tool }

func (s sanitizedTool) Name() string               { return s.inner.Name() }
func (s sanitizedTool) Description() string        { return s.inner.Description() }
func (s sanitizedTool) Parameters() map[string]any { return sanitizeSchema(s.inner.Parameters()) }

func (s sanitizedTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	return s.inner.Execute(ctx, raw, ex)
}

// coercedTool 是 Coerce 的包装类型。
type coercedTool struct{ inner agent.Tool }

func (c coercedTool) Name() string               { return c.inner.Name() }
func (c coercedTool) Description() string        { return c.inner.Description() }
func (c coercedTool) Parameters() map[string]any { return c.inner.Parameters() }

func (c coercedTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	// 用净化后的 schema 纠偏：与本层拿给模型的 schema 保持一致。
	return c.inner.Execute(ctx, coerceArgs(raw, sanitizeSchema(c.inner.Parameters())), ex)
}

// validatedTool 是 Validate 的包装类型。
type validatedTool struct{ inner agent.Tool }

func (v validatedTool) Name() string               { return v.inner.Name() }
func (v validatedTool) Description() string        { return v.inner.Description() }
func (v validatedTool) Parameters() map[string]any { return v.inner.Parameters() }

func (v validatedTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if issue := validateArgs(raw, sanitizeSchema(v.inner.Parameters())); issue != nil {
		display := map[string]any{"code": issue.code, "tool": v.inner.Name(), "summary": "参数需要修正"}
		if issue.field != "" {
			display["field"] = issue.field
		}
		return agent.Result{
			IsError: true,
			Content: fmt.Sprintf("工具 %s 的参数不合法：%s", v.inner.Name(), issue.text),
			Display: display,
		}, nil
	}
	return v.inner.Execute(ctx, raw, ex)
}

// ---------- schema 净化 ----------

// schemaNoiseKeys 是各家 JSON Schema 实现容易拒收的非标准键（契约点名 + 相邻项）。
var schemaNoiseKeys = []string{
	"$schema", "$id", "$defs", "definitions", "$comment", "$anchor",
	"$dynamicAnchor", "$dynamicRef", "$vocabulary", "invariant",
}

// schemaCombinatorKeys 是顶层会被严格后端拒收的组合器。本包的预校验不依赖它们，
// 删掉不会降低可校验性（危险参数仍会被工具本体拒绝）。
var schemaCombinatorKeys = []string{
	"allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependentRequired", "dependentSchemas",
}

// sanitizeSchema 深拷贝并净化一份参数 schema；nil 返回最小的合法 object schema。
func sanitizeSchema(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	copied, _ := deepCopyValue(params).(map[string]any)
	if copied == nil {
		copied = map[string]any{}
	}
	// 先展开本地引用，再移除定义表，避免 MCP/Pydantic schema 留下悬空 $ref。
	expanded := inlineSchemaRefs(copied, copied, make(map[string]bool), 0)
	out, _ := sanitizeNode(expanded).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	out["type"] = "object"
	props, _ := out["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	for key, value := range props {
		props[key] = sanitizeNode(value)
	}
	out["properties"] = props
	out["required"] = normalizeRequired(out["required"])
	for _, key := range schemaCombinatorKeys {
		delete(out, key)
	}
	return out
}

// sanitizeNode 递归净化一个 schema 节点。
func sanitizeNode(value any) any {
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, val := range node {
			out[key] = val
		}
		for _, key := range schemaNoiseKeys {
			delete(out, key)
		}
		if props, ok := out["properties"].(map[string]any); ok {
			clean := make(map[string]any, len(props))
			for key, val := range props {
				clean[key] = sanitizeNode(val)
			}
			out["properties"] = clean
		}
		if items, ok := out["items"]; ok {
			out["items"] = sanitizeNode(items)
		}
		if required, ok := out["required"]; ok {
			out["required"] = normalizeRequired(required)
		}
		normalizeSchemaType(out)
		return out
	case []any:
		out := make([]any, len(node))
		for i := range node {
			out[i] = sanitizeNode(node[i])
		}
		return out
	case []string:
		return append([]string(nil), node...)
	default:
		return value
	}
}

// normalizeSchemaType 把 type 收拾成单值；可空联合类型折叠成 nullable 提示。
func normalizeSchemaType(node map[string]any) {
	raw, ok := node["type"]
	if !ok {
		if inferred := inferSchemaType(node); inferred != "" {
			node["type"] = inferred
		}
		return
	}
	names, hadNull := schemaTypeList(raw)
	if len(names) == 0 {
		if hadNull {
			node["type"] = "null"
			return
		}
		delete(node, "type")
		return
	}
	if len(names) == 1 {
		node["type"] = names[0]
		if hadNull {
			node["nullable"] = true
		}
		return
	}
	// 多类型联合保持数组形态；JSON 里 []string 与 []any 都能序列化。
	if hadNull {
		names = append(names, "null")
	}
	union := make([]any, len(names))
	for i, name := range names {
		union[i] = name
	}
	node["type"] = union
}

// schemaTypeList 把 type 解析成类型名列表，并报告是否出现过 "null"。
func schemaTypeList(raw any) (names []string, hadNull bool) {
	appendType := func(value any) {
		name, ok := value.(string)
		if !ok {
			return
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if name == "null" {
			hadNull = true
			return
		}
		names = append(names, name)
	}
	switch typed := raw.(type) {
	case string:
		appendType(typed)
	case []string:
		for _, item := range typed {
			appendType(item)
		}
	case []any:
		for _, item := range typed {
			appendType(item)
		}
	}
	return names, hadNull
}

// inferSchemaType 在 schema 没写 type 时按结构猜一个（只做最稳妥的推断）。
func inferSchemaType(node map[string]any) string {
	if _, ok := node["properties"]; ok {
		return "object"
	}
	if _, ok := node["additionalProperties"]; ok {
		return "object"
	}
	if _, ok := node["items"]; ok {
		return "array"
	}
	if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
		switch enum[0].(type) {
		case string:
			return "string"
		case float64, int, int64:
			return "number"
		case bool:
			return "boolean"
		}
	}
	if enum, ok := node["enum"].([]string); ok && len(enum) > 0 {
		return "string"
	}
	return ""
}

// normalizeRequired 把 required 统一成去重、去空、保序的 []string。
func normalizeRequired(raw any) []string {
	var out []string
	seen := make(map[string]bool)
	appendName := func(value any) {
		name, ok := value.(string)
		if !ok {
			return
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	switch typed := raw.(type) {
	case []string:
		for _, item := range typed {
			appendName(item)
		}
	case []any:
		for _, item := range typed {
			appendName(item)
		}
	case string:
		appendName(typed)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// deepCopyValue 递归深拷贝 schema 里会用到的容器类型，避免净化时改到调用方的数据。
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = deepCopyValue(typed[i])
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []int:
		return append([]int(nil), typed...)
	default:
		return value
	}
}

// ---------- 参数纠偏 ----------

// coerceArgs 按 schema 纠偏一份原始参数；解析失败或没有改动时原样返回。
func coerceArgs(raw json.RawMessage, schema map[string]any) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	var args map[string]any
	if err := json.Unmarshal(trimmed, &args); err != nil || args == nil {
		return raw
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return raw
	}
	changed := false
	for key, value := range args {
		prop, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		if fixed, did := coerceValue(value, prop); did {
			args[key] = fixed
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return out
}

// coerceValue 按一个属性 schema 纠偏值，返回纠偏结果与「是否改动」。
func coerceValue(value any, schema map[string]any) (any, bool) {
	if schema == nil || value == nil {
		return value, false
	}
	types := schemaTypes(schema)
	nullable := schemaAllowsNull(schema)
	if text, ok := value.(string); ok {
		return coerceString(text, types, schema, nullable)
	}
	switch typed := value.(type) {
	case []any:
		if len(types) > 0 && !containsAny(types, "array") {
			return value, false // 类型不符交给 Validate 报错
		}
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return value, false
		}
		out := make([]any, len(typed))
		changed := false
		for i, item := range typed {
			out[i] = item
			if fixed, did := coerceValue(item, items); did {
				out[i] = fixed
				changed = true
			}
		}
		if changed {
			return out, true
		}
		return value, false
	case map[string]any:
		if len(types) > 0 && !containsAny(types, "object") {
			return value, false
		}
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return value, false
		}
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = val
		}
		changed := false
		for key, val := range typed {
			prop, ok := props[key].(map[string]any)
			if !ok {
				continue
			}
			if fixed, did := coerceValue(val, prop); did {
				out[key] = fixed
				changed = true
			}
		}
		if changed {
			return out, true
		}
		return value, false
	default:
		// 数字/布尔等裸标量：期望数组时补成单元素数组（模型常把列表写成单值）。
		if containsAny(types, "array") {
			return []any{value}, true
		}
		return value, false
	}
}

// coerceString 尝试把字符串纠偏成 schema 期望的标量/容器；无歧义才改。
func coerceString(text string, types []string, schema map[string]any, nullable bool) (any, bool) {
	trimmed := strings.TrimSpace(text)
	if nullable && strings.EqualFold(trimmed, "null") {
		return nil, true
	}
	for _, want := range types {
		switch want {
		case "integer":
			if n, ok := parseIntString(trimmed); ok {
				return n, true
			}
		case "number":
			if f, ok := parseNumberString(trimmed); ok {
				return f, true
			}
		case "boolean":
			if b, ok := parseBoolString(trimmed); ok {
				return b, true
			}
		case "array":
			if parsed, ok := parseJSONArray(trimmed); ok {
				return coerceNested(parsed, schema), true
			}
			// 不是 JSON 数组：按单元素列表补齐（空串不补，交给工具判断）。
			if trimmed != "" {
				return []any{text}, true
			}
		case "object":
			if parsed, ok := parseJSONObject(trimmed); ok {
				return coerceNested(parsed, schema), true
			}
		}
	}
	return text, false
}

// coerceNested 对刚解析出来的容器继续按子 schema 纠偏。
func coerceNested(value any, schema map[string]any) any {
	switch typed := value.(type) {
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return value
		}
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = item
			if fixed, did := coerceValue(item, items); did {
				out[i] = fixed
			}
		}
		return out
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return value
		}
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = val
		}
		for key, val := range typed {
			prop, exists := props[key].(map[string]any)
			if !exists {
				continue
			}
			if fixed, did := coerceValue(val, prop); did {
				out[key] = fixed
			}
		}
		return out
	default:
		return value
	}
}

// parseIntString 把字符串解析成整数；非整数或超范围返回 false。
func parseIntString(text string) (int64, bool) {
	if text == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, true
	}
	// 兼容 "42.0" 这种整数值的小数写法；非整数值不纠偏。
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	if f < math.MinInt64 || f > math.MaxInt64 {
		return 0, false
	}
	return int64(f), true
}

// parseNumberString 把字符串解析成有限浮点数。
func parseNumberString(text string) (float64, bool) {
	if text == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// parseBoolString 只认 true/false（大小写不敏感），不猜 "1"/"0"。
func parseBoolString(text string) (bool, bool) {
	switch strings.ToLower(text) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// parseJSONArray 解析一个 JSON 数组字符串。
func parseJSONArray(text string) ([]any, bool) {
	if !strings.HasPrefix(text, "[") {
		return nil, false
	}
	var out []any
	if err := json.Unmarshal([]byte(text), &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

// parseJSONObject 解析一个 JSON 对象字符串。
func parseJSONObject(text string) (map[string]any, bool) {
	if !strings.HasPrefix(text, "{") {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

// schemaTypes 取 schema 声明的类型名（联合类型展开）。
func schemaTypes(schema map[string]any) []string {
	names, _ := schemaTypeList(schema["type"])
	return names
}

// schemaAllowsNull 判断 schema 是否显式允许 null（type/nullable/anyOf/oneOf）。
func schemaAllowsNull(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	if _, hadNull := schemaTypeList(schema["type"]); hadNull {
		return true
	}
	if nullable, ok := schema["nullable"].(bool); ok && nullable {
		return true
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, _ := schema[key].([]any)
		for _, branch := range branches {
			if item, ok := branch.(map[string]any); ok {
				if _, hadNull := schemaTypeList(item["type"]); hadNull {
					return true
				}
			}
		}
	}
	return false
}

// containsAny 判断 items 中是否有 candidate。
func containsAny(items []string, candidate string) bool {
	for _, item := range items {
		if item == candidate {
			return true
		}
	}
	return false
}

// ---------- 参数预校验 ----------

// argIssue 是一次参数校验失败。
type argIssue struct {
	code  string // 英文大写错误码，进 Display["code"]
	field string // 出问题的字段路径（可空）
	text  string // 给模型看的可修复说明
}

// validateArgs 按 schema 校验一份参数；通过返回 nil。
func validateArgs(raw json.RawMessage, schema map[string]any) *argIssue {
	props, _ := schema["properties"].(map[string]any)
	required := normalizeRequired(schema["required"])
	if len(props) == 0 && len(required) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	args := map[string]any{}
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &args); err != nil {
			return &argIssue{
				code: "TOOL_ARG_JSON",
				text: fmt.Sprintf("参数不是合法的 JSON 对象（收到 %s）。请传形如 {\"字段名\": 值} 的 JSON 对象。",
					clipText(string(trimmed), 120)),
			}
		}
		if args == nil {
			return &argIssue{
				code: "TOOL_ARG_OBJECT",
				text: fmt.Sprintf("参数必须是 JSON 对象（形如 {...}），收到的是 %s。请改成对象后重试。", jsonKindName(trimmed)),
			}
		}
	}
	for _, name := range required {
		value, ok := args[name]
		if !ok || value == nil {
			prop, _ := props[name].(map[string]any)
			return &argIssue{
				code:  "TOOL_ARG_MISSING",
				field: name,
				text: fmt.Sprintf("缺少必填参数 %q（期望 %s）。收到参数：%s。请补上 %s 后重试。",
					name, describeSchemaType(prop), compactJSON(args), name),
			}
		}
	}
	for _, name := range sortedKeys(props) {
		value, exists := args[name]
		if !exists || value == nil {
			continue
		}
		prop, _ := props[name].(map[string]any)
		if issue := validateValue(name, value, prop); issue != nil {
			return issue
		}
	}
	return nil
}

// validateValue 校验单个字段的值（含一层嵌套对象/数组元素）。
func validateValue(field string, value any, schema map[string]any) *argIssue {
	if len(schema) == 0 {
		return nil
	}
	types := schemaTypes(schema)
	if len(types) > 0 && !matchesAnyType(value, types) {
		want := describeTypeNames(types)
		return &argIssue{
			code:  "TOOL_ARG_TYPE",
			field: field,
			text: fmt.Sprintf("参数 %q 期望 %s，收到 %s（%s）。请把 %s 改成 %s 后重试。",
				field, want, jsonTypeName(value), clipText(string(compactJSON(value)), 80), field, want),
		}
	}
	if enum := enumValues(schema); len(enum) > 0 && !matchesEnum(value, enum) {
		return &argIssue{
			code:  "TOOL_ARG_ENUM",
			field: field,
			text: fmt.Sprintf("参数 %q 只能取 %s 之一，收到 %s。请改成允许的取值后重试。",
				field, clipText(string(compactJSON(enum)), 160), clipText(string(compactJSON(value)), 80)),
		}
	}
	// 嵌套对象：递归检查 required 与各字段类型。
	if object, ok := value.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		required := normalizeRequired(schema["required"])
		for _, name := range required {
			sub, exists := object[name]
			if !exists || sub == nil {
				prop, _ := props[name].(map[string]any)
				path := field + "." + name
				return &argIssue{
					code:  "TOOL_ARG_MISSING",
					field: path,
					text: fmt.Sprintf("参数 %q 缺少必填字段 %q（期望 %s）。请补上后重试。",
						field, name, describeSchemaType(prop)),
				}
			}
		}
		for _, name := range sortedKeys(props) {
			sub, exists := object[name]
			if !exists || sub == nil {
				continue
			}
			prop, _ := props[name].(map[string]any)
			if issue := validateValue(field+"."+name, sub, prop); issue != nil {
				return issue
			}
		}
	}
	// 数组：递归检查 items。
	if list, ok := value.([]any); ok {
		items, _ := schema["items"].(map[string]any)
		if len(items) == 0 {
			return nil
		}
		for i, item := range list {
			if issue := validateValue(fmt.Sprintf("%s[%d]", field, i), item, items); issue != nil {
				return issue
			}
		}
	}
	return nil
}

// matchesAnyType 判断值是否匹配类型名列表中的任意一个。
func matchesAnyType(value any, types []string) bool {
	for _, want := range types {
		if matchesType(value, want) {
			return true
		}
	}
	return false
}

// matchesType 按 JSON Schema 的类型定义判断单个值。
func matchesType(value any, want string) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return true // 未知类型名不做限制，避免误伤
	}
}

// matchesEnum 判断值是否等于 enum 中的某一项（按 JSON 语义比较）。
func matchesEnum(value any, enum []any) bool {
	encoded := string(compactJSON(value))
	for _, item := range enum {
		if string(compactJSON(item)) == encoded {
			return true
		}
	}
	return false
}

// enumValues 把 schema 的 enum 统一成 []any（工具可能声明成 []string）。
func enumValues(schema map[string]any) []any {
	switch enum := schema["enum"].(type) {
	case []any:
		return enum
	case []string:
		out := make([]any, len(enum))
		for i, item := range enum {
			out[i] = item
		}
		return out
	default:
		return nil
	}
}

// jsonTypeName 返回值的 JSON 类型名（给报错文案用）。
func jsonTypeName(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return "字符串 string"
	case bool:
		return "布尔 boolean"
	case float64:
		if typed == math.Trunc(typed) && !math.IsInf(typed, 0) {
			return "数字 number（整数值）"
		}
		return "数字 number"
	case []any:
		return "数组 array"
	case map[string]any:
		return "对象 object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// jsonKindName 从原始 JSON 文本判断顶层形状（给「收到的是数组」这类文案用）。
func jsonKindName(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "空内容"
	}
	switch trimmed[0] {
	case '[':
		return "数组 array"
	case '"':
		return "字符串 string"
	case 't', 'f':
		return "布尔 boolean"
	case 'n':
		return "null"
	default:
		return "数字 number"
	}
}

// describeSchemaType 给一个属性 schema 生成「期望 …」文案。
func describeSchemaType(schema map[string]any) string {
	types := schemaTypes(schema)
	if len(types) == 0 {
		return "任意值"
	}
	return describeTypeNames(types)
}

// describeTypeNames 把类型名列表拼成中文短语。
func describeTypeNames(types []string) string {
	names := make([]string, 0, len(types))
	for _, name := range types {
		switch name {
		case "string":
			names = append(names, "字符串 string")
		case "number":
			names = append(names, "数字 number")
		case "integer":
			names = append(names, "整数 integer")
		case "boolean":
			names = append(names, "布尔 boolean")
		case "array":
			names = append(names, "数组 array")
		case "object":
			names = append(names, "对象 object")
		default:
			names = append(names, name)
		}
	}
	return strings.Join(names, " 或 ")
}

// compactJSON 把值压成一行 JSON（给报错文案用），失败退回 fmt.Sprint。
func compactJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(fmt.Sprintf("%v", value))
	}
	return encoded
}

// clipText 按 rune 截断串并加省略号。
func clipText(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

// sortedKeys 返回 map 的键并按字典序排序，保证报错顺序确定（不随 map 遍历随机）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
