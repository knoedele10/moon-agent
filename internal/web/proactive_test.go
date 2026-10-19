package web

import (
	"net/http"
	"testing"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// baseSnap / baseState / baseCandidate 给出一个「没有任何拦截」的基线，
// 每个用例只改动它关心的那一个字段，这样断言才能精确定位到具体分支。
func baseSnap() ProactiveRuntimeSnapshot {
	return ProactiveRuntimeSnapshot{
		Now:         time.Date(2026, 1, 15, 12, 0, 0, 0, time.Local),
		LocalHour:   12,
		IdleSeconds: 600,
		Enabled:     true,
	}
}

func baseCandidate() ProactiveCandidate {
	return ProactiveCandidate{SceneID: "memory:1", Score: 90, SceneCooldownMs: 60 * 60 * 1000}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestCanStartProactiveBlockReasons 逐个覆盖策略层的每一个拦截原因。
func TestCanStartProactiveBlockReasons(t *testing.T) {
	snap := baseSnap()
	now := snap.Now

	cases := []struct {
		name      string
		snap      ProactiveRuntimeSnapshot
		state     ProactiveState
		candidate ProactiveCandidate
		want      string
	}{
		{
			name: "disabled",
			snap: func() ProactiveRuntimeSnapshot { s := snap; s.Enabled = false; return s }(),
			want: "disabled",
		},
		{
			name: "screen_locked",
			snap: func() ProactiveRuntimeSnapshot { s := snap; s.ScreenLocked = true; return s }(),
			want: "screen_locked",
		},
		{
			name: "conversation_busy",
			snap: func() ProactiveRuntimeSnapshot { s := snap; s.ConversationBusy = true; return s }(),
			want: "conversation_busy",
		},
		{
			name: "generation_busy",
			snap: func() ProactiveRuntimeSnapshot { s := snap; s.GenerationBusy = true; return s }(),
			want: "generation_busy",
		},
		{
			name: "night_inactive",
			snap: func() ProactiveRuntimeSnapshot { s := snap; s.LocalHour = 23; s.IdleSeconds = 10; return s }(),
			want: "night_inactive",
		},
		{
			name:  "normal_quiet_period",
			state: ProactiveState{LastNormalConversationAt: ptrTime(now.Add(-10 * time.Minute))},
			want:  "normal_quiet_period",
		},
		{
			name:  "global_cooldown",
			state: ProactiveState{LastProactiveAt: ptrTime(now.Add(-1 * time.Hour))},
			want:  "global_cooldown",
		},
		{
			name: "scene_cooldown",
			state: ProactiveState{LastFiredAt: map[string]time.Time{
				"memory:1": now.Add(-10 * time.Minute),
			}},
			candidate: baseCandidate(),
			want:      "scene_cooldown",
		},
		{
			name:      "unanswered_limit",
			state:     ProactiveState{UnansweredCount: 2, LastProactiveAt: ptrTime(now.Add(-7 * time.Hour))},
			candidate: baseCandidate(),
			want:      "unanswered_limit",
		},
		{
			name:      "followup_cooldown",
			state:     ProactiveState{UnansweredCount: 1, LastProactiveAt: ptrTime(now.Add(-3 * time.Hour))},
			candidate: baseCandidate(),
			want:      "followup_cooldown",
		},
		{
			name: "followup_same_scene",
			state: ProactiveState{
				UnansweredCount:    1,
				LastProactiveAt:    ptrTime(now.Add(-7 * time.Hour)),
				LastProactiveScene: "memory:1",
			},
			candidate: baseCandidate(),
			want:      "followup_same_scene",
		},
		{
			name: "followup_score_too_low",
			state: ProactiveState{
				UnansweredCount:    1,
				LastProactiveAt:    ptrTime(now.Add(-7 * time.Hour)),
				LastProactiveScene: "memory:2",
			},
			candidate: ProactiveCandidate{SceneID: "memory:1", Score: 50, SceneCooldownMs: 60 * 60 * 1000},
			want:      "followup_score_too_low",
		},
		{
			name: "allowed",
			state: ProactiveState{
				UnansweredCount:    1,
				LastProactiveAt:    ptrTime(now.Add(-7 * time.Hour)),
				LastProactiveScene: "memory:2",
			},
			candidate: baseCandidate(),
			want:      "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cand := tc.candidate
			if cand.SceneID == "" {
				cand = baseCandidate()
			}
			// 没显式给快照的用例一律用基线（零值快照 Enabled=false 会一律 disabled）。
			snap := tc.snap
			if snap.Now.IsZero() {
				snap = baseSnap()
			}
			ok, reason := CanStartProactive(snap, tc.state, cand)
			if tc.want == "" {
				if !ok || reason != "" {
					t.Fatalf("应当允许: ok=%v reason=%q", ok, reason)
				}
				return
			}
			if ok {
				t.Fatalf("应当被拦下（%s），却返回允许", tc.want)
			}
			if reason != tc.want {
				t.Fatalf("拦截原因应为 %q，实际 %q", tc.want, reason)
			}
		})
	}
}

// 夜间窗口是 22:00–08:00；边界要精确，否则晚上 22 点整 / 早上 8 点整会判错。
func TestIsNightHourBoundaries(t *testing.T) {
	night := []int{22, 23, 0, 3, 7}
	for _, h := range night {
		if !isNightHour(h) {
			t.Fatalf("%d 点应算夜间", h)
		}
	}
	day := []int{8, 9, 12, 18, 21}
	for _, h := range day {
		if isNightHour(h) {
			t.Fatalf("%d 点不该算夜间", h)
		}
	}
}

// 夜间只有空闲够久（≥60s）才允许主动；空闲不足时报 night_inactive。
func TestNightActiveIdleLimit(t *testing.T) {
	snap := baseSnap()
	snap.LocalHour = 23

	snap.IdleSeconds = nightActiveIdleLimitSec - 1
	if ok, reason := CanStartProactive(snap, ProactiveState{}, baseCandidate()); ok || reason != "night_inactive" {
		t.Fatalf("空闲不足时应当 night_inactive: ok=%v reason=%q", ok, reason)
	}

	snap.IdleSeconds = nightActiveIdleLimitSec
	if ok, reason := CanStartProactive(snap, ProactiveState{}, baseCandidate()); !ok {
		t.Fatalf("空闲达到下限应当放行: reason=%q", reason)
	}
}

// 未回复计数到 2 后，无论等多久都不再发（除非用户回复把计数清零）。
func TestUnansweredLimitStopsAfterTwo(t *testing.T) {
	snap := baseSnap()
	state := ProactiveState{UnansweredCount: 2, LastProactiveAt: ptrTime(snap.Now.Add(-48 * time.Hour))}
	if ok, reason := CanStartProactive(snap, state, baseCandidate()); ok || reason != "unanswered_limit" {
		t.Fatalf("连发两条未回后应当停下: ok=%v reason=%q", ok, reason)
	}
	// 用户回复后计数清零，就能重新发。
	MarkUserActivity(&state)
	if state.UnansweredCount != 0 {
		t.Fatalf("用户活动应当把未回复计数清零: %d", state.UnansweredCount)
	}
	if ok, reason := CanStartProactive(snap, state, baseCandidate()); !ok {
		t.Fatalf("清零后应当放行: reason=%q", reason)
	}
}

// epoch 失效：生成期间用户有活动 / 开始了普通对话，提交时必须判 stale_epoch。
func TestEpochInvalidation(t *testing.T) {
	state := ProactiveState{Epoch: 5}
	generationEpoch := state.Epoch

	// 用户说话了。
	MarkUserActivity(&state)
	if ok, reason := CanCommitProactive(generationEpoch, state); ok || reason != "stale_epoch" {
		t.Fatalf("用户活动后提交应当 stale_epoch: ok=%v reason=%q", ok, reason)
	}

	// 普通对话开始同样使代次失效。
	state2 := ProactiveState{Epoch: 5}
	MarkNormalConversationStarted(&state2)
	if ok, reason := CanCommitProactive(5, state2); ok || reason != "stale_epoch" {
		t.Fatalf("对话开始后提交应当 stale_epoch: ok=%v reason=%q", ok, reason)
	}

	// 没有任何活动时，代次不变，可以提交。
	state3 := ProactiveState{Epoch: 5}
	if ok, reason := CanCommitProactive(5, state3); !ok || reason != "" {
		t.Fatalf("代次不变应当可提交: ok=%v reason=%q", ok, reason)
	}
}

// MarkNormalConversationEnded 要自增代次并记录结束时间。
func TestMarkNormalConversationEnded(t *testing.T) {
	state := ProactiveState{Epoch: 3}
	now := time.Date(2026, 2, 1, 9, 0, 0, 0, time.Local)
	MarkNormalConversationEnded(&state, now)
	if state.Epoch != 4 {
		t.Fatalf("epoch 应当自增: %d", state.Epoch)
	}
	if state.LastNormalConversationAt == nil || !state.LastNormalConversationAt.Equal(now) {
		t.Fatalf("应当记录对话结束时间: %v", state.LastNormalConversationAt)
	}
}

// MarkProactiveCommitted 记录触发时间 / 话题 / 场景冷却，并把未回复计数 +1（封顶 2）。
func TestMarkProactiveCommitted(t *testing.T) {
	state := ProactiveState{}
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.Local)
	cand := ProactiveCandidate{SceneID: "memory:9", Score: 90}
	MarkProactiveCommitted(&state, cand, now)

	if state.LastProactiveAt == nil || !state.LastProactiveAt.Equal(now) {
		t.Fatalf("应当记录触发时间")
	}
	if state.LastProactiveScene != "memory:9" {
		t.Fatalf("应当记录话题: %q", state.LastProactiveScene)
	}
	if state.LastFiredAt["memory:9"].IsZero() {
		t.Fatalf("应当写入场景冷却表")
	}
	if state.UnansweredCount != 1 {
		t.Fatalf("第一次提交后未回复应为 1: %d", state.UnansweredCount)
	}

	MarkProactiveCommitted(&state, ProactiveCandidate{SceneID: "memory:10"}, now.Add(time.Hour))
	if state.UnansweredCount != 2 {
		t.Fatalf("第二次提交后未回复应为 2: %d", state.UnansweredCount)
	}
	MarkProactiveCommitted(&state, ProactiveCandidate{SceneID: "memory:11"}, now.Add(2*time.Hour))
	if state.UnansweredCount != 2 {
		t.Fatalf("未回复计数应封顶 2: %d", state.UnansweredCount)
	}
}

// 打分是纯函数：越久 / 实体越多 / 正文越长，分数越高，且始终落在 [0,100]。
func TestScoreProactiveMemory(t *testing.T) {
	low := scoreProactiveMemory(7, 0, 0)
	high := scoreProactiveMemory(40, 3, 240)
	if !(high > low) {
		t.Fatalf("更强的候选分数应当更高: low=%d high=%d", low, high)
	}
	for _, s := range []int{
		scoreProactiveMemory(-5, -1, -1),
		scoreProactiveMemory(1000, 99, 99999),
	} {
		if s < 0 || s > 100 {
			t.Fatalf("分数应当落在 [0,100]: %d", s)
		}
	}
}

// 环境变量解析：默认 5，非法回落 5，<=0 原样返回（由调度层关掉功能）。
func TestProactiveIntervalMinutes(t *testing.T) {
	t.Setenv("AGENT_PROACTIVE_INTERVAL_MINUTES", "")
	if got := proactiveIntervalMinutes(); got != 5 {
		t.Fatalf("默认应为 5: %d", got)
	}
	t.Setenv("AGENT_PROACTIVE_INTERVAL_MINUTES", "abc")
	if got := proactiveIntervalMinutes(); got != 5 {
		t.Fatalf("非法值应回落 5: %d", got)
	}
	t.Setenv("AGENT_PROACTIVE_INTERVAL_MINUTES", "15")
	if got := proactiveIntervalMinutes(); got != 15 {
		t.Fatalf("应读到 15: %d", got)
	}
	t.Setenv("AGENT_PROACTIVE_INTERVAL_MINUTES", "0")
	if got := proactiveIntervalMinutes(); got != 0 {
		t.Fatalf("0 应原样返回（关闭功能）: %d", got)
	}
}

// DB 未初始化时 4 个 handler 都必须返回 500 而不是 panic。
func TestProactiveHandlersWithoutDB(t *testing.T) {
	old := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = old })

	cases := []struct {
		name   string
		call   func(*gin.Context)
		method string
	}{
		{"status", proactiveStatus, http.MethodGet},
		{"settings", proactiveSettings, http.MethodPut},
		{"run-now", proactiveRunNow, http.MethodPost},
		{"messages", proactiveMessages, http.MethodGet},
	}
	for _, tc := range cases {
		c, w := newTestContext(tc.method, "/api/proactive/status", "")
		tc.call(c) // 必须不 panic
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: DB 未初始化应返回 500，实际 %d", tc.name, w.Code)
		}
	}
}

// nextAllowedAt 取静默期与冷却期的较晚者；没有历史时就是「现在」。
func TestNextAllowedAt(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.Local)

	if got := nextAllowedAt(now, ProactiveState{}); !got.Equal(now) {
		t.Fatalf("没有任何历史时应当是现在: %v", got)
	}

	// 静默期还有 20 分钟，比全局冷却（2 小时前触发 → 已过）更晚。
	ps := ProactiveState{
		LastNormalConversationAt: ptrTime(now.Add(-10 * time.Minute)),
		LastProactiveAt:          ptrTime(now.Add(-3 * time.Hour)),
	}
	want := now.Add(20 * time.Minute)
	if got := nextAllowedAt(now, ps); !got.Equal(want) {
		t.Fatalf("应当取静默期结束: got=%v want=%v", got, want)
	}

	// 未回复时冷却按 6 小时算。
	ps2 := ProactiveState{LastProactiveAt: ptrTime(now.Add(-1 * time.Hour)), UnansweredCount: 1}
	want2 := now.Add(5 * time.Hour)
	if got := nextAllowedAt(now, ps2); !got.Equal(want2) {
		t.Fatalf("未回复时应当取 6 小时冷却: got=%v want=%v", got, want2)
	}
}

// 场景冷却只对「有 scene id」的候选生效；空 scene 不该误伤。
func TestSceneCooldownIgnoresEmptyScene(t *testing.T) {
	snap := baseSnap()
	state := ProactiveState{LastFiredAt: map[string]time.Time{"": snap.Now.Add(-time.Minute)}}
	if ok, reason := CanStartProactive(snap, state, ProactiveCandidate{SceneID: "", Score: 100}); !ok {
		t.Fatalf("空 scene 不该触发 scene_cooldown: reason=%q", reason)
	}
}

// 只读转换：policyStateFrom / applyPolicyState 往返一致（LastFiredAt 用 JSON 存）。
func TestProactiveStateRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 1, 8, 0, 0, 0, time.Local)
	row := model.AgentProactiveState{
		UserID:                   "u1",
		Epoch:                    7,
		UnansweredCount:          1,
		LastProactiveAt:          ptrTime(now),
		LastNormalConversationAt: ptrTime(now.Add(-time.Hour)),
		LastProactiveScene:       "memory:3",
		Enabled:                  true,
	}
	applyPolicyState(&row, ProactiveState{
		Epoch:                    8,
		UnansweredCount:          2,
		LastProactiveAt:          ptrTime(now),
		LastProactiveScene:       "memory:4",
		LastNormalConversationAt: ptrTime(now),
		LastFiredAt:              map[string]time.Time{"memory:4": now},
	})
	back := policyStateFrom(row)
	if back.Epoch != 8 || back.UnansweredCount != 2 || back.LastProactiveScene != "memory:4" {
		t.Fatalf("往返后字段不一致: %+v", back)
	}
	if len(back.LastFiredAt) != 1 || back.LastFiredAt["memory:4"].IsZero() {
		t.Fatalf("LastFiredAt 应往返保留: %+v", back.LastFiredAt)
	}
}
