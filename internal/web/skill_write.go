package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"blog-my/internal/agent"
	"blog-my/internal/agent/skill"
	"blog-my/internal/model"
)

// 技能创作：让模型把「刚刚跑通的一套流程」沉淀成一条可复用的技能。
// 对齐 Open WebUI 的 /skills:create——技能是写给**未来自己**看的说明书，
// 不是给用户看的文档，所以要写成「下次照这个做」，而不是「本次我做了这些」。
//
// 为什么让模型自己写：真正值得沉淀的流程，往往是在一次具体任务里跑出来的
// （踩过坑、试对过参数）。让用户事后手写一遍，多半就没人写了。

const (
	// skillNameMaxLen 与数据库列宽（128）留足余量：名字是提示词里要列出来的，必须短。
	skillNameMaxLen = 64
	// skillBodyMaxBytes 限制正文大小，避免一条技能把 longtext 撑爆、也避免写进一堆无意义内容。
	skillBodyMaxBytes = 64 * 1024
)

// skillBodyStandard 是正文的章节规范（对齐 Open WebUI 的技能创作标准）。
// 放进工具描述里，模型写的时候就能照着排，不用我们事后人工整理。
const skillBodyStandard = "按固定章节写，方便以后扫读：" +
	"`# 标题` 一句话说清它做什么、**不**做什么；" +
	"`## 何时使用` 列出触发场景（用户会怎么说）；" +
	"`## 前置条件` 需要的环境、凭证、安装步骤（没有就写「无」）；" +
	"`## 怎么做` 可照抄的编号步骤；" +
	"`## 速查` 命令、路径、接口；" +
	"`## 坑` 已知限制与失败模式；" +
	"`## 验证` 一条能证明它跑通的检查。"

type skillWriteTool struct{ userID string }

func (t skillWriteTool) Name() string { return "skill_write" }

func (t skillWriteTool) Description() string {
	return "新建或覆盖一条技能。技能是「以后遇到同类事情该怎么做」的说明书。" +
		"当用户明确要求「把这个流程记成技能」，或你们刚跑通一套值得复用的步骤时用它。" +
		"同名技能会被**整条覆盖**，所以更新已有技能前先 skill_read 看现状、别把有用的内容写没了。" +
		"写命令和路径时要照实写（能直接抄），不要写「运行相应命令」这种空话。" +
		skillBodyStandard
}

func (t skillWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "技能名：小写字母/数字/连字符，不要空格，不超过 64 字符（例如 build-windows-installer）。",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "一句话说明这个技能能做什么（不是怎么实现），不超过 200 字，句末加句号。",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "技能正文（Markdown）。" + skillBodyStandard,
			},
		},
		"required": []string{"name", "description", "body"},
	}
}

func (t skillWriteTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	if model.DB == nil {
		return agent.Result{}, fmt.Errorf("数据库未初始化，无法保存技能")
	}
	if strings.TrimSpace(t.userID) == "" {
		return agent.Result{}, fmt.Errorf("缺少用户身份，无法保存技能")
	}

	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	name := normalizeSkillName(a.Name)
	if name == "" {
		return agent.Result{}, fmt.Errorf("name 不能为空（用小写字母、数字、连字符）")
	}
	body := strings.TrimSpace(a.Body)
	if body == "" {
		return agent.Result{}, fmt.Errorf("body 不能为空")
	}
	if len(body) > skillBodyMaxBytes {
		return agent.Result{}, fmt.Errorf("body 太大（%d 字节，上限 %d）", len(body), skillBodyMaxBytes)
	}
	desc := clipRunes(strings.TrimSpace(a.Description), 200)

	// 同名覆盖：与导入器共用一套 upsert，避免两处语义漂移。
	// enabled 传 nil：新建默认启用，已存在的保留用户手工设置的开关状态。
	created, err := model.UpsertSkill(t.userID, skill.Item{
		Name: name, Description: desc, Body: body, Source: "agent",
	}, nil)
	if err != nil {
		return agent.Result{}, err
	}

	verb := "已更新技能"
	if created {
		verb = "已新建技能"
	}
	summary := fmt.Sprintf("%s %s（%d 字）", verb, name, len([]rune(body)))
	return agent.Result{
		Content: summary,
		Display: map[string]any{"name": name, "created": created, "runes": len([]rune(body))},
	}, nil
}

// normalizeSkillName 把模型给的名字规整成「小写 + 连字符」的 slug。
//
// 为什么强制：技能名会出现在系统提示词的清单里，也会被 skill_search 当关键词匹配。
// 大小写混用、带空格的名字会让匹配和去重都变脆（"Build Installer" 与
// "build-installer" 会被当成两条技能）。
func normalizeSkillName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.' || r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// 其余字符（含中文标点、引号）一律丢弃。
		}
	}
	out := strings.Trim(b.String(), "-")
	if len([]rune(out)) > skillNameMaxLen {
		out = string([]rune(out)[:skillNameMaxLen])
		out = strings.Trim(out, "-")
	}
	return out
}
