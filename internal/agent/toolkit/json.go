package toolkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"blog-my/internal/agent"
)

// JSONTool 校验 / 美化 / 压缩 JSON。
//
// 为什么值得单独做个工具：模型手写 JSON 很容易出现多余逗号、引号不配对这类问题，
// 而下游（比如调 API）需要的是严格合法的 JSON。用工具校验比让模型自己数括号可靠。
type JSONTool struct{}

func (JSONTool) Name() string { return "json_format" }

func (JSONTool) Description() string {
	return "校验并整理 JSON：mode=pretty 美化缩进，mode=minify 压缩成一行，mode=validate 只校验。" +
		"JSON 非法时会指出出错位置。需要产出严格合法的 JSON 时用它，不要手写后直接交付。"
}

func (JSONTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"json": map[string]any{"type": "string", "description": "要处理的 JSON 文本"},
			"mode": map[string]any{"type": "string", "enum": []string{"pretty", "minify", "validate"}, "description": "缺省 pretty"},
		},
		"required": []string{"json"},
	}
}

func (JSONTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var a struct {
		JSON string `json:"json"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	src := strings.TrimSpace(a.JSON)
	if src == "" {
		return agent.Result{}, fmt.Errorf("json 不能为空")
	}

	var v any
	dec := json.NewDecoder(bytes.NewReader([]byte(src)))
	dec.UseNumber() // 保留大整数的原样，避免 float64 精度损失
	if err := dec.Decode(&v); err != nil {
		return agent.Result{}, fmt.Errorf("JSON 非法：%v", err)
	}
	// 只允许单个 JSON 值：后面还有内容说明是拼接了多段。
	if dec.More() {
		return agent.Result{}, fmt.Errorf("JSON 后面还有多余内容，只接受单个 JSON 值")
	}

	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	if mode == "" {
		mode = "pretty"
	}

	var out string
	switch mode {
	case "validate":
		out = "JSON 合法。"
	case "minify":
		b, err := json.Marshal(v)
		if err != nil {
			return agent.Result{}, fmt.Errorf("压缩失败: %w", err)
		}
		out = string(b)
	case "pretty":
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return agent.Result{}, fmt.Errorf("美化失败: %w", err)
		}
		out = string(b)
	default:
		return agent.Result{}, fmt.Errorf("未知的 mode %q（支持 pretty/minify/validate）", a.Mode)
	}

	return agent.Result{
		Content: out,
		Display: map[string]any{"mode": mode, "valid": true},
	}, nil
}
