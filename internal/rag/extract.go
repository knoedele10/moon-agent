package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 事实抽取：用**一次**模型调用把一轮对话压成若干条自包含的原子事实。
//
// 这是从 mem0 v3 的 ADD-only 抽取思路借来的：只做“加”，不比对、不改写已有记忆。
// 好处是一次调用就够、实现简单、记忆只累积；代价是旧的过时记忆不会自动消失，
// 需要靠检索时的相关度排序与后续的清理机制处理。

// LLMCaller 是抽取事实所需的最小能力：给 system + user，拿回一段文本。
//
// 注入而不是直接依赖 llm 包，有两个原因：
//  1. rag 不需要知道模型供应商与鉴权；
//  2. 单测可以塞一个假函数，完全离线。
type LLMCaller func(ctx context.Context, system, user string) (string, error)

// MaxFactsPerTurn 限制单轮抽出的条数：一轮聊出 30 条“事实”多半是噪声。
const MaxFactsPerTurn = 8

const factExtractSystem = `你是一个记忆抽取器。把这一轮对话里值得长期记住的信息，抽成自包含的原子事实。

规则：
1. 每条事实只讲一件事，必须能脱离上下文单独读懂：写清主体，例如「用户的项目代号是蓝鲸」，不要写「它是蓝鲸」。
2. 用户的话、以及 AI 给出的结论/计划/方案都要抽；AI 的客套话、能力说明、泛泛的评价不要抽。
3. 只抽对话里真实出现的信息，绝不编造或推断。
4. 同一件事只抽一条，不要换种说法重复。
5. 最多 8 条；没有值得长期记住的就返回空数组。
6. **不要抽“过程”，只抽“结论”**。以下这些一律不抽：
   - 工具/脚本的执行流水：如「运行了 ppt-master/scripts/xxx.py」「调用了 skill_run」「已保存 id=…」「工具执行成功/失败」；
   - AI 写入了记忆、创建了哪个文件、用了哪个工具的描述——那是系统的动作日志，不是关于用户的知识；
   - 文件名、路径、脚本名本身（除非用户明确说过这个路径是他关心的事）。
   下面的「AI 实际做的事」**只用来帮你理解上下文**，不要把它逐条当成事实。
7. 每条事实给出其中出现的实体（人名、项目名、编号、专有名词、时间窗口），用于检索加权。
8. 给每条事实打一个 scope（陪伴记忆与工作记忆要分开，不要把工程流水塞进陪伴记忆）：
   - "life" —— 关于这个人本身：称呼、喜好、情绪、生活、健康、关系、习惯；
   - "work" —— 关于在做的项目/技术/代码/业务：项目名、技术栈、端口、文件、任务进度；
   - 拿不准就给空串 ""，**不要硬猜**。
9. 给每条事实一个 importance（只能取 0 / 0.5 / 1 三档）：
   - 1   —— 长期身份、核心偏好、重要决定（这类事实衰减得最慢）；
   - 0.5 —— 一般事实、计划、约定（拿不准就用 0.5）；
   - 0   —— 随口一提、价值很低的细节。
10. 给每条事实一个 emotion（0~1，没有情绪信息就写 0）：用户明确表达了强烈情绪时给高分，平静陈述给 0。

只输出 JSON，不要任何解释、不要 Markdown 代码块：
{"facts":[{"text":"...","entities":["..."],"scope":"life|work|","importance":0.5,"emotion":0}]}`

// ExtractFacts 抽取原子事实。抽不出来或模型输出不合法时返回 error，
// 由调用方决定降级策略（blog-my 的降级是「存整轮原文」，保证记忆不丢）。
func ExtractFacts(ctx context.Context, call LLMCaller, userMsg, assistantMsg, toolSummary string) ([]Fact, error) {
	if call == nil {
		return nil, fmt.Errorf("rag: 抽取器为空")
	}
	userMsg = strings.TrimSpace(userMsg)
	if userMsg == "" && strings.TrimSpace(assistantMsg) == "" && strings.TrimSpace(toolSummary) == "" {
		return nil, fmt.Errorf("没有可抽取的内容")
	}

	var b strings.Builder
	b.WriteString("【用户说】\n")
	b.WriteString(clipForPrompt(userMsg, 2000))
	if s := strings.TrimSpace(toolSummary); s != "" {
		b.WriteString("\n\n【AI 实际做的事】\n")
		b.WriteString(clipForPrompt(s, 1200))
	}
	if a := strings.TrimSpace(assistantMsg); a != "" {
		b.WriteString("\n\n【AI 的回复】\n")
		b.WriteString(clipForPrompt(a, 2000))
	}

	raw, err := call(ctx, factExtractSystem, b.String())
	if err != nil {
		return nil, err
	}
	facts, err := parseFacts(raw)
	if err != nil {
		return nil, err
	}
	if len(facts) > MaxFactsPerTurn {
		facts = facts[:MaxFactsPerTurn]
	}
	return facts, nil
}

// parseFacts 宽进严出：模型可能包代码围栏、可能前后带一句话，
// 所以先剥围栏再取第一个平衡的花括号对象；解析不出来就报错（交给上层降级）。
func parseFacts(raw string) ([]Fact, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("模型没有返回 JSON 对象")
	}
	body := s[start : end+1]

	// 先确认 facts 字段真的存在：模型返回合法 JSON 但没带 facts，属于答非所问，
	// 应当报错让上层降级，而不是当成“没有值得记的东西”静默丢掉一轮记忆。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return nil, fmt.Errorf("解析事实 JSON 失败: %w", err)
	}
	if _, ok := probe["facts"]; !ok {
		return nil, fmt.Errorf("模型返回的 JSON 里没有 facts 字段")
	}

	// 用指针区分「模型没给这个字段」（nil → 用默认值）与「明确给了 0」。
	// 旧版模型/旧测试只返回 text+entities，缺省必须仍然解析成功且落到兼容默认值：
	// scope=""、importance=0.5、emotion=0。
	var parsed struct {
		Facts []struct {
			Text       string   `json:"text"`
			Entities   []string `json:"entities"`
			Scope      string   `json:"scope"`
			Importance *float64 `json:"importance"`
			Emotion    *float64 `json:"emotion"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("解析事实 JSON 失败: %w", err)
	}

	out := make([]Fact, 0, len(parsed.Facts))
	for _, f := range parsed.Facts {
		text := strings.TrimSpace(f.Text)
		if text == "" {
			continue
		}
		ents := make([]string, 0, len(f.Entities))
		for _, e := range f.Entities {
			if e = strings.TrimSpace(e); e != "" {
				ents = append(ents, e)
			}
		}
		fact := Fact{Text: text, Entities: ents, Scope: normalizeScope(f.Scope), Importance: 0.5}
		if f.Importance != nil {
			fact.Importance = snapImportance(*f.Importance)
		}
		if f.Emotion != nil {
			fact.Emotion = clamp01(*f.Emotion)
		}
		out = append(out, fact)
	}
	return out, nil
}

func clipForPrompt(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
