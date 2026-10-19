// Package toolkit 放与文件系统无关的通用工具：计算器、时间日期、联网搜索。
//
// 与 agent/fs 分开的理由：这些工具不碰磁盘、不需要工作区，任何会话都可以给；
// 而 fs 工具必须先有沙箱根目录才注册。
package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"blog-my/internal/agent"
)

// CalculatorTool 计算数学表达式，避免模型心算出错。
type CalculatorTool struct{}

func (CalculatorTool) Name() string { return "calculator" }

func (CalculatorTool) Description() string {
	return "计算一个数学表达式并返回结果（支持 + - * / % ^、括号、sqrt/abs/round/floor/ceil/" +
		"log/log10/exp/sin/cos/tan/pow/min/max 以及常量 pi、e）。需要算数时用它，不要心算。"
}

func (CalculatorTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{"type": "string", "description": "要计算的表达式，例如 (1+2)*3、2^10、sqrt(2)"},
		},
		"required": []string{"expression"},
	}
}

func (CalculatorTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var a struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(a.Expression) == "" {
		return agent.Result{}, fmt.Errorf("expression 不能为空")
	}

	v, err := evalExpression(a.Expression)
	if err != nil {
		return agent.Result{}, fmt.Errorf("计算 %q 失败: %w", a.Expression, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return agent.Result{}, fmt.Errorf("计算 %q 的结果不是有限数", a.Expression)
	}

	text := formatNumber(v)
	return agent.Result{
		Content: fmt.Sprintf("%s = %s", strings.TrimSpace(a.Expression), text),
		Display: map[string]any{"expression": a.Expression, "value": text},
	}, nil
}

// formatNumber 整数就不带小数点，其余用最短往返表示。
func formatNumber(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ---------- 表达式求值（递归下降，无第三方依赖）----------

type calcParser struct {
	s   string
	pos int
}

func evalExpression(expr string) (float64, error) {
	p := &calcParser{s: expr}
	v, err := p.parseSum()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("表达式里有多余内容: %q", strings.TrimSpace(p.s[p.pos:]))
	}
	return v, nil
}

func (p *calcParser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *calcParser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *calcParser) accept(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *calcParser) parseSum() (float64, error) {
	v, err := p.parseProduct()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.parseProduct()
			if err != nil {
				return 0, err
			}
			v += r
		case '-':
			p.pos++
			r, err := p.parseProduct()
			if err != nil {
				return 0, err
			}
			v -= r
		default:
			return v, nil
		}
	}
}

func (p *calcParser) parseProduct() (float64, error) {
	v, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '*':
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			v *= r
		case '/':
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("除数不能为 0")
			}
			v /= r
		case '%':
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("取模的除数不能为 0")
			}
			v = math.Mod(v, r)
		default:
			return v, nil
		}
	}
}

func (p *calcParser) parseUnary() (float64, error) {
	switch p.peek() {
	case '+':
		p.pos++
		return p.parseUnary()
	case '-':
		p.pos++
		v, err := p.parseUnary()
		return -v, err
	}
	return p.parsePower()
}

// parsePower 右结合：2^3^2 == 2^(3^2)。
func (p *calcParser) parsePower() (float64, error) {
	base, err := p.parseAtom()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
		exp, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *calcParser) parseAtom() (float64, error) {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("表达式意外结束")
	}

	// 括号
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseSum()
		if err != nil {
			return 0, err
		}
		if !p.accept(')') {
			return 0, fmt.Errorf("缺少右括号")
		}
		return v, nil
	}

	// 数字
	if c := p.s[p.pos]; c >= '0' && c <= '9' || c == '.' {
		start := p.pos
		for p.pos < len(p.s) {
			c := p.s[p.pos]
			if (c >= '0' && c <= '9') || c == '.' || c == '_' {
				p.pos++
				continue
			}
			// 科学计数法：1e3 / 2.5e-2
			if c == 'e' || c == 'E' {
				p.pos++
				if p.pos < len(p.s) && (p.s[p.pos] == '+' || p.s[p.pos] == '-') {
					p.pos++
				}
				continue
			}
			break
		}
		lit := strings.ReplaceAll(p.s[start:p.pos], "_", "")
		v, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return 0, fmt.Errorf("无法解析数字 %q", lit)
		}
		return v, nil
	}

	// 标识符：常量或函数
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.pos++
			continue
		}
		break
	}
	name := strings.ToLower(p.s[start:p.pos])
	if name == "" {
		return 0, fmt.Errorf("无法识别的字符 %q", string(p.s[p.pos]))
	}

	if p.peek() == '(' {
		p.pos++
		var args []float64
		if p.peek() != ')' {
			for {
				v, err := p.parseSum()
				if err != nil {
					return 0, err
				}
				args = append(args, v)
				if !p.accept(',') {
					break
				}
			}
		}
		if !p.accept(')') {
			return 0, fmt.Errorf("函数 %s 缺少右括号", name)
		}
		return callFunc(name, args)
	}

	switch name {
	case "pi":
		return math.Pi, nil
	case "e":
		return math.E, nil
	}
	return 0, fmt.Errorf("未知的标识符 %q", name)
}

func callFunc(name string, args []float64) (float64, error) {
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s 需要 %d 个参数，收到 %d 个", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "sqrt":
		if err := need(1); err != nil {
			return 0, err
		}
		if args[0] < 0 {
			return 0, fmt.Errorf("sqrt 的参数不能为负数")
		}
		return math.Sqrt(args[0]), nil
	case "abs":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Abs(args[0]), nil
	case "round":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Round(args[0]), nil
	case "floor":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Floor(args[0]), nil
	case "ceil":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Ceil(args[0]), nil
	case "log":
		if err := need(1); err != nil {
			return 0, err
		}
		if args[0] <= 0 {
			return 0, fmt.Errorf("log 的参数必须为正数")
		}
		return math.Log(args[0]), nil
	case "log10":
		if err := need(1); err != nil {
			return 0, err
		}
		if args[0] <= 0 {
			return 0, fmt.Errorf("log10 的参数必须为正数")
		}
		return math.Log10(args[0]), nil
	case "exp":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Exp(args[0]), nil
	case "sin":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Sin(args[0]), nil
	case "cos":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Cos(args[0]), nil
	case "tan":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Tan(args[0]), nil
	case "pow":
		if err := need(2); err != nil {
			return 0, err
		}
		return math.Pow(args[0], args[1]), nil
	case "min":
		if len(args) == 0 {
			return 0, fmt.Errorf("min 至少需要 1 个参数")
		}
		m := args[0]
		for _, v := range args[1:] {
			m = math.Min(m, v)
		}
		return m, nil
	case "max":
		if len(args) == 0 {
			return 0, fmt.Errorf("max 至少需要 1 个参数")
		}
		m := args[0]
		for _, v := range args[1:] {
			m = math.Max(m, v)
		}
		return m, nil
	}
	return 0, fmt.Errorf("未知的函数 %q", name)
}
