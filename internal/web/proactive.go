package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 本文件是主动聊天的服务层：状态持久化、候选生成、模型调用、后台调度与 4 个 handler。
// 策略判定全部在 web/proactive_policy.go（纯函数），这里只负责把真实世界的数据喂进去。
//
// 路由由主 agent 在 web.go 里接：
//
//	GET  /api/proactive/status    -> proactiveStatus
//	PUT  /api/proactive/settings  -> proactiveSettings
//	POST /api/proactive/run-now   -> proactiveRunNow
//	GET  /api/proactive/messages  -> proactiveMessages

const (
	// proactiveConversationTitle 是主动消息专用会话的固定标题。
	// 每个用户只有一条这样的会话；前端 proactive.js 也用它来在侧栏里定位这条会话。
	proactiveConversationTitle = "主动消息"

	// proactiveMemoryMinAgeDays：只有「至少 7 天前」记下的事才值得回访。
	// 太近的事用户自己还记得，回访价值低，还容易显得助手在复读。
	proactiveMemoryMinAgeDays = 7

	// proactiveSceneCooldownMs：同一个话题 24 小时内不重复提。
	proactiveSceneCooldownMs int64 = 24 * 60 * 60 * 1000

	// proactiveCandidateLimit：每轮最多取这么多条候选来算分。
	proactiveCandidateLimit = 5

	// proactiveMessageMaxRunes：落库前的长度上限保护（提示词要求 20~40 字，这里留足余量）。
	proactiveMessageMaxRunes = 120

	// proactiveMessagesAPILimit：/api/proactive/messages 返回的最近条数。
	proactiveMessagesAPILimit = 20
)

// ---------------------------------------------------------------------------
// 环境开关
// ---------------------------------------------------------------------------

// proactiveIntervalMinutes 读 AGENT_PROACTIVE_INTERVAL_MINUTES，默认 5（分钟）。
// <=0 表示关掉整个功能：调度根本不启动。
func proactiveIntervalMinutes() int {
	raw := strings.TrimSpace(os.Getenv("AGENT_PROACTIVE_INTERVAL_MINUTES"))
	if raw == "" {
		return 5
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 5
	}
	return n
}

// proactiveForcedModel 读 AGENT_PROACTIVE_MODEL：指定主动消息用哪个模型。
// 不设时自动挑「第一个有 key 的端点」的第一个模型。
func proactiveForcedModel() string {
	return strings.TrimSpace(os.Getenv("AGENT_PROACTIVE_MODEL"))
}

// ---------------------------------------------------------------------------
// 忙闲信号（内存态；拿不到就按 false）
// ---------------------------------------------------------------------------

type proactiveBusyFlags struct {
	conversation bool
	generation   bool
	screenLocked bool
}

var (
	proactiveBusyMu sync.Mutex
	proactiveBusy   = map[string]proactiveBusyFlags{}
)

// SetProactiveBusy 供桌面壳 / 对话主循环上报「正在对话 / 正在生成 / 锁屏」。
//
// 目前没有调用方：web/chat.go 与 web/agent.go 属于别的 worker 的改动范围，
// 主动聊天不去改它们。策略层允许「拿不到就当 false」，所以默认全是 false，
// 功能照常；这个 setter 留着是为了以后接线时不必再动策略层。
func SetProactiveBusy(userID string, conversationBusy, generationBusy, screenLocked bool) {
	if strings.TrimSpace(userID) == "" {
		return
	}
	proactiveBusyMu.Lock()
	defer proactiveBusyMu.Unlock()
	if !conversationBusy && !generationBusy && !screenLocked {
		delete(proactiveBusy, userID)
		return
	}
	proactiveBusy[userID] = proactiveBusyFlags{conversationBusy, generationBusy, screenLocked}
}

func proactiveBusyFor(userID string) proactiveBusyFlags {
	proactiveBusyMu.Lock()
	defer proactiveBusyMu.Unlock()
	return proactiveBusy[userID]
}

// ---------------------------------------------------------------------------
// 状态读写
// ---------------------------------------------------------------------------

// loadProactiveState 取用户状态；没有这一行时返回「默认开启」的空状态。
func loadProactiveState(userID string) (model.AgentProactiveState, error) {
	if model.DB == nil {
		return model.AgentProactiveState{}, errors.New("数据库未初始化")
	}
	var row model.AgentProactiveState
	err := model.DB.Where("user_id = ?", userID).First(&row).Error
	if err == nil {
		return row, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 没有行 = 从没设置过 = 默认开启。
		return model.AgentProactiveState{UserID: userID, Enabled: true}, nil
	}
	return model.AgentProactiveState{}, err
}

// saveProactiveState 写回状态行（不存在就插入）。
func saveProactiveState(row *model.AgentProactiveState) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	row.UpdatedAt = time.Now()
	var n int64
	if err := model.DB.Model(&model.AgentProactiveState{}).
		Where("user_id = ?", row.UserID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return model.DB.Create(row).Error
	}
	return model.DB.Save(row).Error
}

// policyStateFrom / applyPolicyState 在「持久化形态」与「策略纯数据」之间转换。
func policyStateFrom(row model.AgentProactiveState) ProactiveState {
	ps := ProactiveState{
		Epoch:                    row.Epoch,
		UnansweredCount:          row.UnansweredCount,
		LastProactiveAt:          row.LastProactiveAt,
		LastProactiveScene:       row.LastProactiveScene,
		LastNormalConversationAt: row.LastNormalConversationAt,
	}
	if strings.TrimSpace(row.LastFiredJSON) != "" {
		var m map[string]time.Time
		if err := json.Unmarshal([]byte(row.LastFiredJSON), &m); err == nil && len(m) > 0 {
			ps.LastFiredAt = m
		}
	}
	return ps
}

func applyPolicyState(row *model.AgentProactiveState, ps ProactiveState) {
	row.Epoch = ps.Epoch
	row.UnansweredCount = ps.UnansweredCount
	row.LastProactiveAt = ps.LastProactiveAt
	row.LastProactiveScene = ps.LastProactiveScene
	row.LastNormalConversationAt = ps.LastNormalConversationAt
	if len(ps.LastFiredAt) == 0 {
		row.LastFiredJSON = ""
		return
	}
	if b, err := json.Marshal(ps.LastFiredAt); err == nil {
		row.LastFiredJSON = string(b)
	}
}

// lastMessageAt 查用户最后一条消息的时间（role 为空表示不限角色）。
// 用 MAX 让数据库算，不把消息拉回内存；个人库量级下这条聚合很快。
func lastMessageAt(userID, role string) *time.Time {
	if model.DB == nil || userID == "" {
		return nil
	}
	var row struct {
		T *time.Time `gorm:"column:t"`
	}
	q := model.DB.Model(&model.Message{}).
		Joins("JOIN conversation ON conversation.id = message.conversation_id").
		Where("conversation.user_id = ?", userID)
	if role != "" {
		q = q.Where("message.role = ?", role)
	}
	if err := q.Select("MAX(message.created_at) AS t").Scan(&row).Error; err != nil {
		return nil
	}
	return row.T
}

// reconcileProactiveState 用数据库里真实的用户消息时间补上「用户活动」信号。
//
// 为什么不在 web/chat.go 里直接调 Mark*：那个文件属于另一个 worker 的改动范围，
// 主动聊天不该去改它。改为每轮调度前查一次「这个用户最后一条 user 消息的时间」，
// 有变化就等价于一次 MarkNormalConversationEnded（epoch++、记录时间），
// 于是 normal_quiet_period 与 epoch 失效检查都能照常工作。
// 生成前后各调一次，就能捕捉到「生成期间用户又说话了」并判 stale_epoch。
func reconcileProactiveState(row *model.AgentProactiveState) {
	lastUser := lastMessageAt(row.UserID, "user")
	if lastUser == nil {
		return
	}
	ps := policyStateFrom(*row)
	if ps.LastNormalConversationAt != nil && !lastUser.After(*ps.LastNormalConversationAt) {
		return // 没有新的用户消息
	}
	MarkNormalConversationEnded(&ps, *lastUser)
	// 用户在上一条主动消息之后说话了 → 视为「已回复」，未回复计数清零。
	if ps.LastProactiveAt != nil && lastUser.After(*ps.LastProactiveAt) {
		MarkUserActivity(&ps)
	}
	applyPolicyState(row, ps)
}

// buildProactiveSnapshot 组装策略层要的运行时快照。
func buildProactiveSnapshot(userID string, row model.AgentProactiveState, now time.Time) ProactiveRuntimeSnapshot {
	// 空闲用「距上次任何消息的时间」近似。从没有任何消息时给一个很大的值，
	// 表示「一直空闲」，不会误触发夜间拦截。
	idle := int64(1 << 30)
	if t := lastMessageAt(userID, ""); t != nil {
		if d := int64(now.Sub(*t).Seconds()); d >= 0 {
			idle = d
		}
	} else if row.LastNormalConversationAt != nil {
		if d := int64(now.Sub(*row.LastNormalConversationAt).Seconds()); d >= 0 {
			idle = d
		}
	}
	busy := proactiveBusyFor(userID)
	return ProactiveRuntimeSnapshot{
		Now:              now,
		LocalHour:        now.Hour(),
		IdleSeconds:      int(idle),
		Enabled:          row.Enabled,
		ConversationBusy: busy.conversation,
		GenerationBusy:   busy.generation,
		ScreenLocked:     busy.screenLocked,
	}
}

// ---------------------------------------------------------------------------
// 候选生成
// ---------------------------------------------------------------------------

// scoreProactiveMemory 给一条旧记忆算「值不值得回访」的分数（0~100）。
//
// 纯函数，便于单测；现在是三个简单信号，后续可以整体换成更好的模型打分。
//
//	时间：7 天前刚够格（0 分），越久越值得回访（30 天以上给满 25 分）；
//	重要性：抽出的实体越多越像一件具体的事（每个 6 分，最多 18 分）；
//	信息量：正文越长越可能是一段值得跟进的事（最多 12 分）。
func scoreProactiveMemory(ageDays, entityCount, textBytes int) int {
	score := 55

	age := ageDays
	if age < proactiveMemoryMinAgeDays {
		age = proactiveMemoryMinAgeDays
	}
	if age > proactiveMemoryMinAgeDays+30 {
		age = proactiveMemoryMinAgeDays + 30
	}
	score += (age - proactiveMemoryMinAgeDays) * 25 / 30

	if entityCount < 0 {
		entityCount = 0
	}
	if entityCount > 3 {
		entityCount = 3
	}
	score += entityCount * 6

	if textBytes < 0 {
		textBytes = 0
	}
	if textBytes > 240 {
		textBytes = 240
	}
	score += textBytes * 12 / 240

	if score > 100 {
		score = 100
	}
	return score
}

// proactivePick 是一条被选中的候选，附带生成时要用到的记忆正文。
// ProactiveCandidate 本身只有 scene/score/cooldown，正文单独带，避免把长文本塞进策略层。
type proactivePick struct {
	candidate  ProactiveCandidate
	memoryText string
}

// proactiveMemoryCandidates 从用户的长期记忆里挑出「值得追问」的候选。
//
// 只查库、不做嵌入：主动聊天是后台任务，不该依赖 Ollama 是否在跑；
// 条件是「来源是自动记忆、未被取代、至少 7 天前」。最近提过的话题在这里就过滤掉，
// 免得选到一条必然被 scene_cooldown 拦下的候选，把机会让给别的。
func proactiveMemoryCandidates(userID string, now time.Time, state ProactiveState) []ProactiveCandidate {
	if model.DB == nil || userID == "" {
		return nil
	}
	cutoff := now.AddDate(0, 0, -proactiveMemoryMinAgeDays)
	var docs []model.KBDocument
	if err := model.DB.
		Select("id, title, bytes, meta, created_at").
		Where("user_id = ? AND source = ? AND stale = ? AND created_at <= ?", userID, "memory", false, cutoff).
		Order("created_at asc").
		Limit(proactiveCandidateLimit * 4).
		Find(&docs).Error; err != nil {
		log.Printf("[proactive] 读取记忆候选失败：%v", err)
		return nil
	}

	out := make([]ProactiveCandidate, 0, len(docs))
	for _, d := range docs {
		scene := fmt.Sprintf("memory:%d", d.Id)
		if last, ok := state.LastFiredAt[scene]; ok {
			if now.Sub(last) < time.Duration(proactiveSceneCooldownMs)*time.Millisecond {
				continue // 最近提过，换个话题
			}
		}
		var meta struct {
			Entities []string `json:"entities"`
		}
		_ = json.Unmarshal([]byte(d.Meta), &meta)
		ageDays := int(now.Sub(d.CreatedAt).Hours() / 24)
		out = append(out, ProactiveCandidate{
			SceneID:         scene,
			Score:           scoreProactiveMemory(ageDays, len(meta.Entities), int(d.Bytes)),
			SceneCooldownMs: proactiveSceneCooldownMs,
		})
	}

	// 分数高的排前面；同分时用 scene id 保证顺序稳定（便于测试与复现）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].SceneID < out[j].SceneID
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > proactiveCandidateLimit {
		out = out[:proactiveCandidateLimit]
	}
	return out
}

// pickProactiveCandidate 选一条候选并取回它的正文。
// 没有合适候选时返回 ok=false——宁可安静，也不发一条没内容可说的消息。
func pickProactiveCandidate(userID string, now time.Time, state ProactiveState) (proactivePick, bool) {
	cands := proactiveMemoryCandidates(userID, now, state)
	if len(cands) == 0 {
		return proactivePick{}, false
	}
	best := cands[0]
	id, err := strconv.ParseUint(strings.TrimPrefix(best.SceneID, "memory:"), 10, 64)
	if err != nil {
		return proactivePick{}, false
	}
	var doc model.KBDocument
	if err := model.DB.Select("content, title").Where("id = ?", uint(id)).First(&doc).Error; err != nil {
		return proactivePick{}, false
	}
	text := strings.TrimSpace(string(doc.Content))
	if text == "" {
		return proactivePick{}, false
	}
	return proactivePick{candidate: best, memoryText: text}, true
}

// ---------------------------------------------------------------------------
// 模型生成
// ---------------------------------------------------------------------------

// proactiveSystemPrompt 要求「一句话、自然、不客套、不暴露记忆机制」。
// 这些约束直接来自需求：20~40 字、不要重复问候、不要提「根据记忆」。
const proactiveSystemPrompt = "你是用户的长期助手。现在请你以助手的身份，主动对用户说一句话。" +
	"要求：\n" +
	"1. 长度 20~40 个字，只输出这一句话，不要加引号、不要用列表或 Markdown；\n" +
	"2. 自然、口语化，像随口想起一件事，不要客套（不要“你好”“在吗”“最近怎么样”）；\n" +
	"3. 不要重复以前说过的问候，不要提“根据记忆”“我记得”这类元信息；\n" +
	"4. 可以就下面这件事问一句、或顺着它给一个小建议。"

// proactiveModel 挑一个可用的模型：优先 AGENT_PROACTIVE_MODEL，否则第一个有 key 的端点。
// 没有可用模型时返回 ok=false——此时不发消息（不降级造假）。
func proactiveModel(userID string) (modelID, key string, ok bool) {
	if registry == nil {
		return "", "", false
	}
	if forced := proactiveForcedModel(); forced != "" {
		ep, found := registry.FindByModel(forced)
		if !found {
			return "", "", false
		}
		return forced, resolveKey(userID, ep), true
	}
	for _, ep := range registry.Endpoints() {
		if len(ep.Models) == 0 {
			continue
		}
		k := resolveKey(userID, ep)
		// EnvKey 为空的端点（ollama / mock）本来就不需要 key，不能因为 key=="" 被跳过。
		if k == "" && ep.EnvKey != "" {
			continue
		}
		return ep.Models[0], k, true
	}
	return "", "", false
}

// generateProactive 调一次模型生成主动消息。任何失败都返回 error，由调用方决定「不发」。
func generateProactive(ctx context.Context, modelID, key string, pick proactivePick) (string, error) {
	if registry == nil {
		return "", errors.New("模型注册表未初始化")
	}
	if strings.TrimSpace(modelID) == "" {
		return "", errors.New("没有可用模型")
	}
	userMsg := "用户之前提过这样一件事：\n" + clipRunes(pick.memoryText, 300) +
		"\n\n请据此主动说一句自然的话。"
	out, err := registry.Generate(ctx, modelID, key, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: proactiveSystemPrompt},
			{Role: llm.RoleUser, Content: userMsg},
		},
		Temperature: 0.8,
		MaxTokens:   200,
	})
	if err != nil {
		return "", err
	}
	msg := strings.TrimSpace(out)
	msg = strings.Trim(msg, "“”\"'` \n\t")
	if msg == "" {
		return "", errors.New("模型没有返回内容")
	}
	return clipRunes(msg, proactiveMessageMaxRunes), nil
}

// ---------------------------------------------------------------------------
// 运行一次
// ---------------------------------------------------------------------------

// proactiveRunResult 是一次「尝试发主动消息」的结果，直接作为 run-now 的响应体。
type proactiveRunResult struct {
	Fired          bool     `json:"fired"`
	BlockReason    string   `json:"block_reason"`
	Bypassed       []string `json:"bypassed"`
	ConversationID uint     `json:"conversation_id"`
	MessageID      uint     `json:"message_id"`
	Content        string   `json:"content"`
	Detail         string   `json:"detail,omitempty"`
}

// bypassableReasons 是 run-now 允许绕过的「冷却类」原因。
// 只放冷却：disabled / 忙 / 夜间 / 未回复上限这些语义不该被调试入口绕过。
var bypassableReasons = map[string]bool{
	"normal_quiet_period": true,
	"global_cooldown":     true,
	"scene_cooldown":      true,
	"followup_cooldown":   true,
}

// runProactiveForUser 跑一次完整流程：组装快照 → 选候选 → 过策略 → 生成 → 代次校验 → 落库。
// bypassCooldowns=true 时跳过冷却类拦截，并在结果的 bypassed 里如实列出绕过了哪些。
func runProactiveForUser(ctx context.Context, userID string, bypassCooldowns bool) proactiveRunResult {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return proactiveRunResult{BlockReason: "disabled", Detail: "数据库未初始化"}
	}

	row, err := loadProactiveState(userID)
	if err != nil {
		return proactiveRunResult{BlockReason: "error", Detail: err.Error()}
	}
	reconcileProactiveState(&row)
	if err := saveProactiveState(&row); err != nil {
		log.Printf("[proactive] 保存状态失败（继续本轮）：%v", err)
	}

	now := time.Now()
	snap := buildProactiveSnapshot(userID, row, now)
	ps := policyStateFrom(row)

	pick, ok := pickProactiveCandidate(userID, now, ps)
	if !ok {
		return proactiveRunResult{BlockReason: "no_candidate"}
	}

	allowed, reason := CanStartProactive(snap, ps, pick.candidate)
	var bypassed []string
	if !allowed && bypassCooldowns && bypassableReasons[reason] {
		bypassed = append(bypassed, reason)
		allowed = true
	}
	if !allowed {
		return proactiveRunResult{BlockReason: reason}
	}

	// 开始生成前记下代次：生成期间用户一说话，提交时就会被判 stale_epoch。
	generationEpoch := ps.Epoch

	modelID, key, ok := proactiveModel(userID)
	if !ok {
		return proactiveRunResult{BlockReason: "no_model", Bypassed: bypassed}
	}
	content, err := generateProactive(ctx, modelID, key, pick)
	if err != nil {
		// 失败就不发：绝不发一条降级的假消息。
		log.Printf("[proactive] 生成失败（本轮不发）：%v", err)
		return proactiveRunResult{BlockReason: "generation_failed", Bypassed: bypassed, Detail: err.Error()}
	}

	// 代次校验：生成期间用户有过活动就丢弃这条过期消息。
	reconcileProactiveState(&row)
	ps = policyStateFrom(row)
	if ok, reason := CanCommitProactive(generationEpoch, ps); !ok {
		_ = saveProactiveState(&row)
		return proactiveRunResult{BlockReason: reason, Bypassed: bypassed}
	}

	convID, err := ensureProactiveConversation(userID)
	if err != nil {
		return proactiveRunResult{BlockReason: "error", Bypassed: bypassed, Detail: err.Error()}
	}
	msg := model.Message{ConversationID: convID, Role: "assistant", Content: content}
	if err := model.DB.Create(&msg).Error; err != nil {
		return proactiveRunResult{BlockReason: "error", Bypassed: bypassed, Detail: err.Error()}
	}
	// 旁注表写失败不回滚消息：消息本身已经落库，标记丢了只是前端少一个徽标，
	// 比「消息发了但状态没更新导致重复发送」要轻。
	if err := model.DB.Create(&model.AgentProactiveMessage{
		UserID:         userID,
		MessageID:      msg.Id,
		ConversationID: convID,
		SceneID:        pick.candidate.SceneID,
	}).Error; err != nil {
		log.Printf("[proactive] 写主动消息标记失败（消息已落库）：%v", err)
	}

	MarkProactiveCommitted(&ps, pick.candidate, now)
	applyPolicyState(&row, ps)
	if err := saveProactiveState(&row); err != nil {
		log.Printf("[proactive] 保存状态失败（可能影响冷却）：%v", err)
	}
	touchConversation(convID)

	return proactiveRunResult{
		Fired:          true,
		Bypassed:       bypassed,
		ConversationID: convID,
		MessageID:      msg.Id,
		Content:        content,
	}
}

// ensureProactiveConversation 取「主动消息」专用会话，没有就建一条。
//
// 用固定标题定位：Conversation 表没有「用途」列，而这张表是共享表不能随便加列。
// 每个用户最多一条同名会话，取最早的一条（id asc），避免历史上手工建过同名会话时乱选。
func ensureProactiveConversation(userID string) (uint, error) {
	if model.DB == nil {
		return 0, errors.New("数据库未初始化")
	}
	var conv model.Conversation
	err := model.DB.Where("user_id = ? AND title = ?", userID, proactiveConversationTitle).
		Order("id asc").First(&conv).Error
	if err == nil {
		return conv.Id, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	conv = model.Conversation{UserID: userID, Title: proactiveConversationTitle}
	if err := model.DB.Create(&conv).Error; err != nil {
		return 0, err
	}
	return conv.Id, nil
}

// ---------------------------------------------------------------------------
// 后台调度
// ---------------------------------------------------------------------------

var (
	proactiveSchedMu     sync.Mutex
	proactiveSchedCancel context.CancelFunc
)

// StartProactiveScheduler 启动后台调度；幂等，重复调用无副作用。
// 间隔来自 AGENT_PROACTIVE_INTERVAL_MINUTES（默认 5 分钟，<=0 关闭）。
func StartProactiveScheduler(ctx context.Context) {
	proactiveSchedMu.Lock()
	defer proactiveSchedMu.Unlock()
	if proactiveSchedCancel != nil {
		return // 已经在跑
	}
	runCtx, cancel := context.WithCancel(ctx)
	proactiveSchedCancel = cancel
	go proactiveLoop(runCtx)
}

// StopProactiveScheduler 停掉后台调度（测试 / 优雅退出用）。
func StopProactiveScheduler() {
	proactiveSchedMu.Lock()
	cancel := proactiveSchedCancel
	proactiveSchedCancel = nil
	proactiveSchedMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func proactiveLoop(ctx context.Context) {
	mins := proactiveIntervalMinutes()
	if mins <= 0 {
		log.Printf("[proactive] AGENT_PROACTIVE_INTERVAL_MINUTES=%d，主动聊天已关闭", mins)
		return
	}
	interval := time.Duration(mins) * time.Minute
	log.Printf("[proactive] 调度已启动，每 %s 检查一次", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[proactive] 调度已停止")
			return
		case <-ticker.C:
			runProactiveCycle(ctx)
		}
	}
}

// runProactiveCycle 遍历所有开启了主动聊天的用户，各跑一轮。
func runProactiveCycle(ctx context.Context) {
	if model.DB == nil {
		return
	}
	var users []model.User
	if err := model.DB.Select("id").Find(&users).Error; err != nil {
		log.Printf("[proactive] 读取用户失败：%v", err)
		return
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		row, err := loadProactiveState(u.Id)
		if err != nil || !row.Enabled {
			continue
		}
		res := runProactiveForUser(ctx, u.Id, false)
		if res.Fired {
			log.Printf("[proactive] 已向 user=%s 发主动消息（会话 %d，消息 %d）", u.Id, res.ConversationID, res.MessageID)
		}
	}
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// nextAllowedAt 估算「最早什么时候可能允许下一次主动」。
// 取静默期结束与冷却期结束的较晚者；返回值可能已经过去（表示现在就没有时间类拦截）。
func nextAllowedAt(now time.Time, ps ProactiveState) time.Time {
	t := now
	if ps.LastNormalConversationAt != nil {
		if v := ps.LastNormalConversationAt.Add(time.Duration(normalQuietMs) * time.Millisecond); v.After(t) {
			t = v
		}
	}
	if ps.LastProactiveAt != nil {
		wait := globalProactiveIntervalMs
		if ps.UnansweredCount >= 1 {
			wait = followupIntervalMs
		}
		if v := ps.LastProactiveAt.Add(time.Duration(wait) * time.Millisecond); v.After(t) {
			t = v
		}
	}
	return t
}

// formatTimePtr 把可空时间格式化成 RFC3339；nil → 空串。
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// proactiveStatus 返回当前开关、上次主动时间、下次允许时间与当前的拦截原因。
func proactiveStatus(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	row, err := loadProactiveState(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取主动聊天状态失败"})
		return
	}
	reconcileProactiveState(&row)

	now := time.Now()
	ps := policyStateFrom(row)
	snap := buildProactiveSnapshot(userID, row, now)

	// 中性候选：状态接口只暴露「环境 / 时间」类拦截，不让候选分数影响 block_reason。
	// 空 scene + 满分保证 scene_cooldown / followup_same_scene / followup_score_too_low 不会误报。
	neutral := ProactiveCandidate{SceneID: "", Score: 100}
	_, reason := CanStartProactive(snap, ps, neutral)

	c.JSON(http.StatusOK, gin.H{
		"enabled":         row.Enabled,
		"last_at":         formatTimePtr(row.LastProactiveAt),
		"next_allowed_at": nextAllowedAt(now, ps).Format(time.RFC3339),
		"block_reason":    reason,
	})
}

// proactiveSettings 保存主动聊天开关。
func proactiveSettings(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误：需要 enabled 字段"})
		return
	}
	userID := currentUserID(c)
	row, err := loadProactiveState(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取主动聊天状态失败"})
		return
	}
	row.Enabled = *body.Enabled
	if err := saveProactiveState(&row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存主动聊天设置失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "enabled": row.Enabled})
}

// proactiveRunNow 手动触发一次（只用于调试 / 自测）。
// 仍走完整策略；body 可带 {"bypass_cooldowns": true} 绕过冷却类拦截，
// 绕过了哪些会在响应的 bypassed 里如实列出。
func proactiveRunNow(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	var body struct {
		BypassCooldowns bool `json:"bypass_cooldowns"`
	}
	// body 允许为空：空 body 会返回 EOF，这不是错误。
	_ = c.ShouldBindJSON(&body)

	res := runProactiveForUser(c.Request.Context(), currentUserID(c), body.BypassCooldowns)
	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"fired":           res.Fired,
		"block_reason":    res.BlockReason,
		"bypassed":        res.Bypassed,
		"conversation_id": res.ConversationID,
		"message_id":      res.MessageID,
		"content":         res.Content,
		"detail":          res.Detail,
	})
}

// proactiveMessages 返回最近的主动消息（按 id 倒序，最新在前）。
func proactiveMessages(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var rows []model.AgentProactiveMessage
	if err := model.DB.Where("user_id = ?", userID).
		Order("id desc").Limit(proactiveMessagesAPILimit).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取主动消息失败"})
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		content := ""
		var m model.Message
		if err := model.DB.Select("content").Where("id = ?", r.MessageID).First(&m).Error; err == nil {
			content = m.Content
		}
		items = append(items, gin.H{
			"id":              r.MessageID,
			"conversation_id": r.ConversationID,
			"scene_id":        r.SceneID,
			"content":         content,
			"created_at":      r.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":                 true,
		"conversation_title": proactiveConversationTitle,
		"messages":           items,
	})
}
