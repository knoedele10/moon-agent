package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"blog-my/internal/llm"
	"blog-my/internal/model"
	"blog-my/internal/rag"

	"github.com/gin-gonic/gin"
)

// 技能的**自动检索**：理解任务 → 混合召回 top20 → 大模型重排 top5 → 注入本轮上下文。
//
// 为什么不是「把技能清单塞满提示词」：技能库上百条，简介全塞进去就是几万 token，
// 而且大部分跟当前这句话毫无关系。这里只把**这一轮真正相关**的几条正文放进去，
// 既省上下文，又比让模型自己猜该读哪个技能更准。
//
// 三级降级，任何一级坏了都不阻断对话：
//  1. 混合召回（词面 + 实体 + 语义）：Ollama 没开就自动少一路语义信号；
//  2. 大模型重排：模型不可用就退回混合检索的顺序；
//  3. 全部不可用：不注入任何技能，对话照常。
const (
	// skillRecallTopK 是喂给重排模型的候选数。
	skillRecallTopK = 20
	// skillRerankTopN 是最终注入的技能数。
	skillRerankTopN = 5
	// skillInjectRunes 是单个技能注入正文的上限：5 条 × 3000 字，上下文还可控。
	skillInjectRunes = 3000
	// skillEmbedTimeout 是查询向量化的超时。刻意很短：本地 Ollama 没开时，
	// 连接会被立刻拒绝；真在跑但很慢时，也不能让“检索技能”把整轮对话卡住。
	skillEmbedTimeout = 3 * time.Second
)

// skillHit 是一条召回到的技能。Body 不出 JSON（正文只在服务端用于拼上下文）。
type skillHit struct {
	ID          uint    `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
	Lexical     float64 `json:"lexical"`
	Semantic    float64 `json:"semantic"`
	Entity      float64 `json:"entity"`
	NameHit     bool    `json:"name_hit"`
	Body        string  `json:"-"`
}

// skillEnvBool 读一个开关型环境变量，默认值由调用方给。
func skillEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return fallback
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// ---------- 混合召回 ----------

// loadEnabledSkills 取某个用户所有已启用技能。
//
// 一次全取（而不是先做 SQL LIKE 预筛）：技能是几百条量级，全取一遍再在内存里打分，
// 比让数据库做中文子串匹配靠谱得多（MySQL 的 LIKE 对中文二元组召回很弱）。
func loadEnabledSkills(userID string) ([]model.AgentSkill, error) {
	if model.DB == nil {
		return nil, fmt.Errorf("数据库未初始化")
	}
	var rows []model.AgentSkill
	err := model.DB.Where("user_id = ? AND enabled = ?", userID, true).
		Order("name asc").Find(&rows).Error
	return rows, err
}

// fuseSkillScores 融合三路信号。
//
// 与 rag.FuseScores 同思路（自适应分母，避免缺一路信号就把分数整体压低），
// 但**不要求语义分**：技能检索必须在没开 Ollama 的机器上照常工作。
func fuseSkillScores(lexical, semantic, entity float64, hasSemantic, hasEntity bool) float64 {
	maxPossible := 1.0 // 词面这一路永远在
	if hasSemantic {
		maxPossible += 1.0
	}
	if hasEntity {
		maxPossible += 0.5
	}
	return (lexical + semantic + entity) / maxPossible
}

// skillNameHit 判断技能名是不是被用户直接点名了（“用 caveman 模式”“打开 officecli-pptx”）。
//
// 直接点名的技能必须排最前：模型的偏好不该覆盖用户的明确指令。
func skillNameHit(query, name string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	n := strings.ToLower(strings.TrimSpace(name))
	if q == "" || n == "" {
		return false
	}
	return strings.Contains(q, n)
}

// rankSkills 是纯函数：给定技能行、查询与语义分表，返回融合排序后的前 topK。
// 抽出来是为了能不起数据库、不起 Ollama 直接对排序逻辑写单测。
func rankSkills(rows []model.AgentSkill, query string, sem map[uint]float64, topK int) []skillHit {
	if len(rows) == 0 {
		return nil
	}
	qEntities := rag.QueryEntities(query)
	hasSemantic := len(sem) > 0
	hasEntity := false

	hits := make([]skillHit, 0, len(rows))
	for _, r := range rows {
		// 词面分分两路：名字+简介是“这条技能是什么”的正脸，正文只是补充。
		// 单看正文覆盖率会让“顺带提过这个词”的长技能压过真正对口的短技能
		// （有个技能正文 9 万字，随便什么查询都能在它里面找到词），
		// 所以正文一路只按 0.6 折算，且不单独构成高分。
		lexDesc := rag.LexicalScore(query, r.Name+" "+r.Description)
		lexBody := rag.LexicalScore(query, r.Body) * 0.6
		lexical := lexDesc
		if lexBody > lexical {
			lexical = lexBody
		}
		// 实体从名字与简介里抽：正文里的实体太杂，会把噪声当成“精确命中”。
		entity := rag.EntityBoost(qEntities, rag.QueryEntities(r.Name+" "+r.Description))
		if entity > 0 {
			hasEntity = true
		}
		semantic := 0.0
		if hasSemantic {
			semantic = sem[r.Id]
		}

		score := fuseSkillScores(lexical, semantic, entity, hasSemantic, hasEntity)
		nameHit := skillNameHit(query, r.Name)
		if nameHit {
			score = math.Min(1, score+0.5)
		}
		hits = append(hits, skillHit{
			ID: r.Id, Name: r.Name, Description: r.Description, Body: r.Body,
			Score: score, Lexical: lexical, Semantic: semantic, Entity: entity, NameHit: nameHit,
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Name < hits[j].Name
	})
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// skillCosine 余弦相似度。维度不一致直接返回 0：
// 换了嵌入模型时新旧向量会混在库里，不能让它们互相算出一个假分数。
func skillCosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------- 语义一路（可选） ----------

// skillEmbedderConfig 从环境读 Ollama 配置，与知识库共用同一个嵌入模型。
func skillEmbedderConfig() rag.OllamaConfig {
	return rag.OllamaConfig{
		BaseURL:    strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")),
		EmbedModel: strings.TrimSpace(os.Getenv("KB_EMBED_MODEL")),
		Timeout:    skillEmbedTimeout,
	}
}

// skillEmbedText 是技能的嵌入文本：名字 + 简介。
// 正文不进向量——正文动辄几万字，嵌入既慢，又会把简介的信号稀释掉。
func skillEmbedText(r model.AgentSkill) string {
	return strings.TrimSpace(r.Name + "：" + clipRunes(strings.TrimSpace(r.Description), 500))
}

// skillSemanticScores 尽力给出查询与各技能的语义相似度。
//
// 第二个返回值非空表示“这一路没用上”，内容是给人看的原因（前端会显示成降级提示）。
// 这**不是错误**：技能检索不该因为本地模型没开就整个不可用。
//
// 顺带做惰性补算：谁还没有向量就一起嵌入并落库，下次召回就能用上语义。
// 这样导入技能不需要依赖 Ollama（没装也能导），语义能力什么时候有就什么时候生效。
func skillSemanticScores(ctx context.Context, rows []model.AgentSkill, query string) (map[uint]float64, string) {
	if len(rows) == 0 || strings.TrimSpace(query) == "" {
		return nil, ""
	}
	vecs := make(map[uint][]float32, len(rows))
	missing := make([]int, 0, len(rows))
	texts := []string{query}
	for i, r := range rows {
		if v := model.DecodeEmbedding(r.Embedding); len(v) > 0 {
			vecs[r.Id] = v
			continue
		}
		missing = append(missing, i)
		texts = append(texts, skillEmbedText(r))
	}
	if len(vecs) == 0 && len(missing) == 0 {
		return nil, ""
	}

	ectx, cancel := context.WithTimeout(ctx, skillEmbedTimeout)
	defer cancel()
	out, err := rag.NewOllamaEmbedder(skillEmbedderConfig()).Embed(ectx, texts)
	if err != nil || len(out) != len(texts) {
		reason := "本地嵌入模型不可用（Ollama 没启动？），本轮只用词面+实体召回"
		if err != nil {
			reason += "：" + err.Error()
		}
		return nil, reason
	}
	queryVec := out[0]
	if len(queryVec) == 0 {
		return nil, "查询向量为空，跳过语义召回"
	}
	// 补算的向量落库。失败只忽略：这是优化，不是必需路径。
	for k, idx := range missing {
		if k+1 >= len(out) {
			break
		}
		vecs[rows[idx].Id] = out[k+1]
		_ = model.SaveSkillEmbedding(rows[idx].Id, out[k+1])
	}
	scores := make(map[uint]float64, len(vecs))
	for id, v := range vecs {
		scores[id] = skillCosine(queryVec, v)
	}
	return scores, ""
}

// recallSkills 混合召回：词面 + 实体 +（可用时）语义，返回前 topK 条。
func recallSkills(ctx context.Context, userID, query string, topK int) ([]skillHit, string, error) {
	rows, err := loadEnabledSkills(userID)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", nil
	}
	sem, degrade := skillSemanticScores(ctx, rows, query)
	return rankSkills(rows, query, sem, topK), degrade, nil
}

// ---------- 理解任务 ----------

// skillUnderstandSystem 让模型把一句话摊成检索关键词。
//
// 关键在“中英混合”：技能名和简介大多是英文，用户却常用中文提问（“解释一下这段报错”）。
// 没有本地嵌入模型时词面召回跨不了语言，这句话在库里几乎什么都命中不了；
// 先把它摊成 debug / diagnose / error 这类词，召回才有的放矢。
// 这就是链路的第一步“理解任务”。
const skillUnderstandSystem = "你是技能检索的查询改写器。用户给你一句任务描述，" +
	"你输出 3~8 个用于在技能库里做关键词检索的词，中英混合，覆盖动作、对象和产物类型。" +
	"只输出一行，词之间用空格分隔，不要编号、不要引号、不要解释。"

// skillUnderstandQuery 返回“原句 + 关键词”的检索串。
//
// 失败一律退回原句：这一步只影响召回质量，绝不能影响能不能聊天。
func skillUnderstandQuery(ctx context.Context, query, modelID, key string) (string, error) {
	if registry == nil {
		return query, fmt.Errorf("模型注册表未初始化")
	}
	out, err := registry.Generate(ctx, modelID, key, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: skillUnderstandSystem},
			{Role: llm.RoleUser, Content: clipRunes(query, 1000)},
		},
		Temperature: 0,
		MaxTokens:   80,
	})
	if err != nil {
		return query, err
	}
	words := sanitizeKeywords(out)
	if words == "" {
		return query, fmt.Errorf("改写模型没有给出关键词")
	}
	return query + " " + words, nil
}

// sanitizeKeywords 只留第一行关键词，并去掉模型爱加的编号、引号、项目符号和字段名。
func sanitizeKeywords(raw string) string {
	line := strings.TrimSpace(raw)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	repl := strings.NewReplacer(
		"\"", " ", "'", " ", "`", " ", "，", " ", "、", " ", "：", " ", ":", " ",
		"；", " ", ";", " ", "*", " ", "#", " ", "-", " ", "(", " ", ")", " ",
	)
	// 模型有时会把“关键词：”这个标签本身也当成词回给我们。
	drop := map[string]bool{"关键词": true, "检索词": true, "词": true, "keyword": true, "keywords": true}
	fields := strings.Fields(repl.Replace(line))
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if drop[strings.ToLower(f)] {
			continue
		}
		kept = append(kept, f)
		if len(kept) >= 12 {
			break
		}
	}
	return strings.Join(kept, " ")
}

// ---------- 大模型重排 ----------

// skillRerankSystem 要求“只输出 JSON”。重排要的是**判断**，不是文采；
// 让模型自由发挥只会得到一段解释，反而要写更脆的解析。
const skillRerankSystem = "你是技能路由器。用户给你一个任务和候选技能清单（技能名：简介），" +
	"你挑出最多 N 个真正相关的技能，按相关度从高到低排列。" +
	"只输出 JSON，形如 {\"picked\":[\"技能名1\",\"技能名2\"]}，不要输出任何解释，" +
	"不要编造清单以外的名字。都不相关就输出 {\"picked\":[]}。"

// rerankSkills 让大模型从召回结果里挑出最相关的 topN 个。
//
// 返回 error 时调用方应退回混合检索顺序——重排是**加分项**，不是必经环节。
func rerankSkills(ctx context.Context, modelID, key, query string, cands []skillHit, topN int) ([]skillHit, error) {
	if len(cands) <= topN {
		return cands, nil
	}
	if registry == nil {
		return nil, fmt.Errorf("模型注册表未初始化")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "任务：%s\n\n候选技能（共 %d 个）：\n", clipRunes(query, 1000), len(cands))
	for i, c := range cands {
		fmt.Fprintf(&b, "%d. %s：%s\n", i+1, c.Name, clipRunes(strings.TrimSpace(c.Description), 200))
	}
	fmt.Fprintf(&b, "\n最多选 %d 个，只输出 JSON。", topN)

	out, err := registry.Generate(ctx, modelID, key, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: skillRerankSystem},
			{Role: llm.RoleUser, Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   300,
	})
	if err != nil {
		return nil, err
	}
	names := parsePickedNames(out)
	if len(names) == 0 {
		return nil, fmt.Errorf("重排模型没有给出技能名")
	}
	byName := make(map[string]skillHit, len(cands))
	for _, c := range cands {
		byName[strings.ToLower(c.Name)] = c
	}
	picked := make([]skillHit, 0, topN)
	seen := make(map[string]bool, topN)
	for _, n := range names {
		k := strings.ToLower(strings.TrimSpace(n))
		if k == "" || seen[k] {
			continue
		}
		c, ok := byName[k]
		if !ok {
			continue // 模型编出来的名字一律忽略，不让它凭空造技能
		}
		seen[k] = true
		picked = append(picked, c)
		if len(picked) >= topN {
			break
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("重排结果里没有清单内的技能名")
	}
	return picked, nil
}

// parsePickedNames 从模型输出里抠出技能名列表。
//
// 容忍三种常见写法：{"picked":[...]}、{"skills":[...]}、直接一个 JSON 数组；
// 围栏与前后解释也一并剥掉——要求模型 100% 只吐 JSON 是不现实的。
func parsePickedNames(out string) []string {
	s := strings.TrimSpace(out)
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```JSON", "")
	s = strings.ReplaceAll(s, "```", "")
	s = strings.TrimSpace(s)

	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		var obj struct {
			Picked []string `json:"picked"`
			Skills []string `json:"skills"`
		}
		if err := json.Unmarshal([]byte(s[i:j+1]), &obj); err == nil {
			if len(obj.Picked) > 0 {
				return obj.Picked
			}
			if len(obj.Skills) > 0 {
				return obj.Skills
			}
		}
	}
	if i, j := strings.Index(s, "["), strings.LastIndex(s, "]"); i >= 0 && j > i {
		var arr []string
		if err := json.Unmarshal([]byte(s[i:j+1]), &arr); err == nil {
			return arr
		}
	}
	return nil
}

// ---------- 注入与入口 ----------

// buildSkillBlock 把选中的技能拼成注入系统提示词的一段。
//
// 每条都截断到 skillInjectRunes：5 条技能里有 1 条超长（有的技能几万字），
// 不截断就会把整轮上下文挤爆，而用户要的是“会做这件事”，不是把说明书背下来。
func buildSkillBlock(hits []skillHit) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n【本轮自动匹配到的技能】按相关度排序，请优先按它们的说明执行；不够用时再用 skill_search 找更多。\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "\n--- 技能 %d：%s ---\n", i+1, h.Name)
		if d := strings.TrimSpace(h.Description); d != "" {
			fmt.Fprintf(&b, "简介：%s\n", clipRunes(d, 300))
		}
		body := strings.TrimSpace(h.Body)
		if body == "" {
			body = "（这条技能没有正文，只有简介）"
		}
		if len([]rune(body)) > skillInjectRunes {
			body = string([]rune(body)[:skillInjectRunes]) + "\n…（正文已截断，要完整说明用 skill_read）"
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String()
}

func joinReason(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a + "；" + b
}

// autoSkillContext 是整条链路的入口：召回 → 重排 → 拼成可注入的提示词块。
// skillChitchatRe 匹配“纯寒暄/确认”的整句：这类消息不需要任何技能。
var skillChitchatRe = regexp.MustCompile(`^(在吗|你好|您好|hi|hello|hey|谢谢|多谢|感谢|好的|好滴|行|嗯|哦|哦哦|ok|okay|收到|明白|知道了|了解|哈哈+|呵呵|早上好|中午好|晚上好|晚安|再见|拜拜|没事)[!！。.~～?？、\s]*$`)

// skillTaskKeywords 是“这件事可能需要专门技能”的信号词。
// 刻意包含中英文与常见产出物名称（ppt/excel/文档…）：模型说英文、
// 或者用户直接点明要什么文件，都该触发检索。
var skillTaskKeywords = []string{
	// 中文：动作意图
	"帮我", "写一份", "写个", "写一篇", "做一个", "做个", "生成", "创建", "制作", "设计", "排版",
	"整理", "总结", "汇总", "润色", "改写", "翻译", "调研", "分析", "对比", "评估", "优化", "重构",
	"报告", "方案", "计划", "提纲", "大纲", "文案", "邮件", "简历",
	// 中文：产出物
	"文档", "表格", "幻灯片", "演示", "海报", "图片", "视频", "动画", "图表", "脑图", "流程图", "代码", "脚本",
	// 英文与格式名
	"create", "make", "generate", "design", "write", "draft", "summar", "translat", "analyz", "research",
	"report", "slides", "slide", "deck", "presentation", "spreadsheet", "worksheet", "document", "poster", "diagram",
	"ppt", "pptx", "docx", "xlsx", "excel", "word", "pdf", "markdown",
}

// needSkills 判断这句话值不值得去检索技能。目标是“宁漏不错”：
// 漏了最多是模型自己用 skill_search 找（它有这个工具），
// 错了则是每句闲聊都多一次模型调用 + 一堆无关技能塞进上下文。
func needSkills(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	if skillChitchatRe.MatchString(t) {
		return false
	}
	runes := len([]rune(t))
	// 很短的句子（<6 字）几乎不可能是需要专门技能的任务。
	if runes < 6 {
		return false
	}
	for _, kw := range skillTaskKeywords {
		if strings.Contains(t, kw) {
			return true
		}
	}
	// 没命中信号词，但句子足够长（≥30 字）也算有信息量的请求，值得找一下。
	return runes >= 30
}

// source 取 "llm"（大模型重排过）或 "hybrid"（纯检索排序）；
// degrade 非空表示某一路降级了，前端会把它显示出来，而不是让用户以为“技能没用上”。
func autoSkillContext(ctx context.Context, userID, query, modelID, key string) (block string, picked []skillHit, source, degrade string, err error) {
	if !skillEnvBool("AGENT_SKILL_AUTO", true) {
		return "", nil, "", "", nil
	}
	if strings.TrimSpace(query) == "" || strings.TrimSpace(userID) == "" {
		return "", nil, "", "", nil
	}
	// 先过一道**便宜的判断**：这句话到底像不像需要技能的任务？
	//
	// 不加闸门的话，每句“你好”都会触发一次模型调用（任务理解）+ 全库检索 + 重排，
	// 既慢又可能把无关技能塞进上下文。注意：这只影响**自动注入**——
	// 模型仍可自己调 skill_search / skill_read 主动找技能，能力不会被关掉。
	if !needSkills(query) {
		return "", nil, "skipped", "", nil
	}
	// 第一步“理解任务”：把一句话摊成中英混合关键词。失败就用原句检索。
	// degrade 是具名返回值，这里直接累加，不要重新声明。
	recallQuery := query
	if skillEnvBool("AGENT_SKILL_UNDERSTAND", true) && registry != nil &&
		strings.TrimSpace(modelID) != "" && strings.TrimSpace(key) != "" {
		if expanded, uerr := skillUnderstandQuery(ctx, query, modelID, key); uerr != nil {
			degrade = joinReason(degrade, "任务理解失败，直接用原句检索："+uerr.Error())
		} else {
			recallQuery = expanded
		}
	}

	hits, recallDegrade, err := recallSkills(ctx, userID, recallQuery, skillRecallTopK)
	degrade = joinReason(degrade, recallDegrade)
	if err != nil {
		return "", nil, "", degrade, err
	}
	if len(hits) == 0 {
		return "", nil, "", degrade, nil
	}

	picked, source = hits, "hybrid"
	if len(hits) > skillRerankTopN {
		picked = hits[:skillRerankTopN]
	}
	if skillEnvBool("AGENT_SKILL_RERANK", true) && len(hits) > skillRerankTopN {
		switch {
		case registry == nil:
			degrade = joinReason(degrade, "模型注册表未初始化，跳过重排")
		case strings.TrimSpace(modelID) == "" || strings.TrimSpace(key) == "":
			degrade = joinReason(degrade, "本轮没有可用模型或密钥，跳过重排")
		default:
			if rp, rerr := rerankSkills(ctx, modelID, key, query, hits, skillRerankTopN); rerr == nil {
				picked, source = rp, "llm"
			} else {
				degrade = joinReason(degrade, "大模型重排失败，退回混合检索排序："+rerr.Error())
			}
		}
	}
	return buildSkillBlock(picked), picked, source, degrade, nil
}

// recallSkillsAPI 是技能检索的“预演”接口。
//
// 有了它，界面能直接回答“这句话会命中哪些技能”，召回质量也能在不发聊天消息的情况下被验证。
func recallSkillsAPI(c *gin.Context) {
	userID := currentUserID(c)
	var body struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
		TopN  int    `json:"top_n"`
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	q := strings.TrimSpace(body.Query)
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query 不能为空"})
		return
	}
	topK := body.TopK
	if topK <= 0 {
		topK = skillRecallTopK
	}
	if topK > 50 {
		topK = 50
	}
	topN := body.TopN
	if topN <= 0 {
		topN = skillRerankTopN
	}
	if topN > 10 {
		topN = 10
	}

	// 与对话里的自动检索走同一条链路：先理解任务（摊成中英混合关键词），再召回。
	recallQuery, degrade := q, ""
	if modelID := strings.TrimSpace(body.Model); modelID != "" && registry != nil {
		if ep, ok := registry.FindByModel(modelID); ok {
			if key := resolveKey(userID, ep); key != "" {
				if expanded, uerr := skillUnderstandQuery(c.Request.Context(), q, modelID, key); uerr != nil {
					degrade = joinReason(degrade, "任务理解失败，直接用原句检索："+uerr.Error())
				} else {
					recallQuery = expanded
				}
			}
		}
	}
	hits, recallDegrade, err := recallSkills(c.Request.Context(), userID, recallQuery, topK)
	degrade = joinReason(degrade, recallDegrade)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "召回失败：" + err.Error()})
		return
	}
	picked, source := hits, "hybrid"
	if len(picked) > topN {
		picked = picked[:topN]
	}
	if modelID := strings.TrimSpace(body.Model); modelID != "" && len(hits) > topN && registry != nil {
		if ep, ok := registry.FindByModel(modelID); ok {
			if key := resolveKey(userID, ep); key != "" {
				if rp, rerr := rerankSkills(c.Request.Context(), modelID, key, q, hits, topN); rerr == nil {
					picked, source = rp, "llm"
				} else {
					degrade = joinReason(degrade, rerr.Error())
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "query": q, "recall_query": recallQuery,
		"recall": hits, "picked": picked, "source": source, "error": degrade,
	})
}
