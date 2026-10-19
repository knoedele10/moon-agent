package web

import "time"

// 本文件把 Cyrene 的「主动聊天」策略状态机翻成 Go，**全部是纯函数**：
// 不碰数据库、不碰时钟（时间从快照传入），所以每个拦截分支都能直接单测。
//
// 主动聊天不是定时器，而是一整套「拦截条件 + 代次防错」。下面这些常量
// 照抄 Cyrene 的默认值，理由写在各自注释里；改动它们之前先想清楚为什么要偏离。

const (
	// normalQuietMs 是「刚聊完不打扰」的静默期。
	// 照抄 Cyrene 的 30 分钟：用户刚说完话，助手马上又来一句会显得很吵。
	normalQuietMs int64 = 30 * 60 * 1000

	// globalProactiveIntervalMs 是全局冷却：两次主动消息之间至少隔 2 小时。
	// 照抄 Cyrene：即使话题完全不同，也不该在两小时内反复找用户。
	globalProactiveIntervalMs int64 = 2 * 60 * 60 * 1000

	// followupIntervalMs 是「上一次没回时的追加间隔」。
	// 照抄 Cyrene 的 6 小时：用户没回说明可能在忙，追加要等得更久。
	followupIntervalMs int64 = 6 * 60 * 60 * 1000

	// nightActiveIdleLimitSec 是夜间判定「空闲」的下限。
	// 照抄 Cyrene 的 60 秒：夜间只有用户已经空闲（不在打字）才允许主动，
	// 避免打断正在用电脑的人。
	nightActiveIdleLimitSec = 60

	// followupMinScore 是追加消息的分数门槛。
	// 照抄 Cyrene 的 85：用户上次没回，追问必须更确定「这事值得提」才发。
	followupMinScore = 85
)

// ProactiveRuntimeSnapshot 是「这一轮运行时的外部状态」：时间、空闲、各种忙闲开关。
//
// 字段全部由调用方（web/proactive.go）组装，策略层只读不写，保证可单测。
type ProactiveRuntimeSnapshot struct {
	Now       time.Time
	LocalHour int
	// IdleSeconds 是距用户上一次任何消息的秒数（近似「空闲时长」）。
	IdleSeconds int
	Enabled     bool
	// ConversationBusy / GenerationBusy 表示用户当前正在对话 / 正在生成回复。
	ConversationBusy bool
	GenerationBusy   bool
	// ScreenLocked 表示屏幕已锁定。
	//
	// Cyrene 有 screen_locked 这条拦截；后端本身拿不到锁屏信号，所以默认 false，
	// 由桌面壳通过 SetProactiveBusy 上报。保留这个字段是为了让该分支可测、语义不丢。
	ScreenLocked bool
}

// ProactiveCandidate 是一条候选主动消息（一个可追问的话题）。
type ProactiveCandidate struct {
	SceneID         string
	Score           int
	SceneCooldownMs int64
}

// ProactiveState 是持久化状态的纯数据形态（与 model.AgentProactiveState 一一对应）。
type ProactiveState struct {
	Epoch                    int64
	UnansweredCount          int
	LastProactiveAt          *time.Time
	LastProactiveScene       string
	LastNormalConversationAt *time.Time
	// LastFiredAt 是 sceneID → 最近一次触发时间；nil 表示还没有任何触发记录。
	LastFiredAt map[string]time.Time
}

// isNightHour 判定是否处于夜间窗口（22:00–08:00，含 22 点、不含 8 点）。
func isNightHour(hour int) bool {
	return hour >= 22 || hour < 8
}

// withinMs 判断 t（可能为 nil）距离 now 是否还没超过 ms 毫秒。
// t 为 nil 表示「从没发生过」，一律不算在冷却期内。
func withinMs(now time.Time, t *time.Time, ms int64) bool {
	if t == nil {
		return false
	}
	return now.Sub(*t) < time.Duration(ms)*time.Millisecond
}

// CanStartProactive 是策略主入口：返回 (是否允许, 拦截原因)。
//
// 拦截原因按 Cyrene 的语义**原样保留**，便于排查：
//
//	disabled / screen_locked / conversation_busy / generation_busy / night_inactive /
//	normal_quiet_period / global_cooldown / scene_cooldown / unanswered_limit /
//	followup_cooldown / followup_same_scene / followup_score_too_low
//
// （stale_epoch 不在这里判——它发生在生成结束提交时，见 CanCommitProactive。）
//
// 判定顺序就是上面列出的顺序，且**有意如此**：比如全局 2 小时冷却排在未回复上限之前，
// 于是「两小时内」永远报 global_cooldown，而 followup_cooldown（6 小时）只在
// 2 小时到 6 小时之间、且上一次没回时才可能出现。
func CanStartProactive(snap ProactiveRuntimeSnapshot, state ProactiveState, candidate ProactiveCandidate) (bool, string) {
	if !snap.Enabled {
		return false, "disabled"
	}
	if snap.ScreenLocked {
		return false, "screen_locked"
	}
	if snap.ConversationBusy {
		return false, "conversation_busy"
	}
	if snap.GenerationBusy {
		return false, "generation_busy"
	}

	// 夜间：只有用户已经空闲够久才允许，否则不打扰。
	if isNightHour(snap.LocalHour) && snap.IdleSeconds < nightActiveIdleLimitSec {
		return false, "night_inactive"
	}

	// 刚聊完的静默期。
	if withinMs(snap.Now, state.LastNormalConversationAt, normalQuietMs) {
		return false, "normal_quiet_period"
	}

	// 全局冷却：两次主动消息之间至少 2 小时。
	if withinMs(snap.Now, state.LastProactiveAt, globalProactiveIntervalMs) {
		return false, "global_cooldown"
	}

	// 场景冷却：同一个话题在它自己的冷却期内不重复提。
	if candidate.SceneID != "" {
		if last, ok := state.LastFiredAt[candidate.SceneID]; ok {
			if snap.Now.Sub(last) < time.Duration(candidate.SceneCooldownMs)*time.Millisecond {
				return false, "scene_cooldown"
			}
		}
	}

	// 未回复上限：连发两条都没回就停下来，别刷屏。
	if state.UnansweredCount >= 2 {
		return false, "unanswered_limit"
	}

	// 上一次没回时的追加规则：间隔更长、必须换话题、分数要更高。
	if state.UnansweredCount >= 1 {
		if withinMs(snap.Now, state.LastProactiveAt, followupIntervalMs) {
			return false, "followup_cooldown"
		}
		if candidate.SceneID != "" && candidate.SceneID == state.LastProactiveScene {
			return false, "followup_same_scene"
		}
		if candidate.Score < followupMinScore {
			return false, "followup_score_too_low"
		}
	}

	return true, ""
}

// MarkUserActivity 记录「用户有活动」：代次自增、未回复清零。
//
// 用户一说话，之前发出去的主动消息就算「被回应」了，未回复计数没有继续累积的理由；
// epoch++ 则让任何还在生成中的主动消息在提交时被判 stale_epoch 丢掉。
func MarkUserActivity(state *ProactiveState) {
	state.Epoch++
	state.UnansweredCount = 0
}

// MarkNormalConversationStarted 记录「一次普通对话开始」。
// 只自增代次：开始生成时用户显然在线，任何在途的主动消息都过期了。
func MarkNormalConversationStarted(state *ProactiveState) {
	state.Epoch++
}

// MarkNormalConversationEnded 记录「一次普通对话结束」：代次自增并记下结束时间。
// 结束时间是 normal_quiet_period（30 分钟静默）的基准。
func MarkNormalConversationEnded(state *ProactiveState, now time.Time) {
	state.Epoch++
	t := now
	state.LastNormalConversationAt = &t
}

// MarkProactiveCommitted 在一条主动消息成功落库后更新状态。
// 记录触发时间与话题、写入场景冷却表，并把未回复计数 +1（封顶 2）。
func MarkProactiveCommitted(state *ProactiveState, c ProactiveCandidate, now time.Time) {
	t := now
	state.LastProactiveAt = &t
	state.LastProactiveScene = c.SceneID
	if state.LastFiredAt == nil {
		state.LastFiredAt = map[string]time.Time{}
	}
	if c.SceneID != "" {
		state.LastFiredAt[c.SceneID] = now
	}
	if state.UnansweredCount < 2 {
		state.UnansweredCount++
	}
}

// CanCommitProactive 是生成结束、准备落库前的代次校验：返回 (是否可提交, 原因)。
// generationEpoch 是**开始生成前**记下的 epoch；与当前 state.Epoch 不一致，
// 说明生成期间用户有过活动 / 开始了别的对话，这条消息已经过期，必须丢弃。
func CanCommitProactive(generationEpoch int64, state ProactiveState) (bool, string) {
	if generationEpoch != state.Epoch {
		return false, "stale_epoch"
	}
	return true, ""
}
