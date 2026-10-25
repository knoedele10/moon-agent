package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"

	"gorm.io/gorm"
)

// 三个工具：定唤醒 / 看唤醒 / 取消唤醒。
//
// 只依赖 exec.SessionID（就是会话 id）和构造时注入的 userID；
// 这样它们在 HTTP 轮次、子 agent、后台唤醒轮次里都能用同一套权限与归属校验。

// wakeupConversationID 从执行上下文取会话 id；不在会话里时给一句人话。
func wakeupConversationID(ex *agent.Exec) (uint, error) {
	if ex == nil {
		return 0, errors.New("缺少执行上下文")
	}
	id, err := strconv.ParseUint(strings.TrimSpace(ex.SessionID), 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("当前不在一个会话里，无法设置定时唤醒")
	}
	return uint(id), nil
}

// loadOwnedConversation 确认会话属于当前用户（跨用户当不存在）。
func loadOwnedConversation(userID string, convID uint) (model.Conversation, error) {
	var conv model.Conversation
	if model.DB == nil {
		return conv, errors.New("数据库未初始化")
	}
	err := model.DB.Where("id = ? AND user_id = ?", convID, userID).First(&conv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return conv, errors.New("会话不存在或不属于当前用户")
	}
	return conv, err
}

// ---------------------------------------------------------------------------
// schedule_wakeup
// ---------------------------------------------------------------------------

type scheduleWakeupTool struct{ userID string }

func (scheduleWakeupTool) Name() string { return "schedule_wakeup" }

func (scheduleWakeupTool) Description() string {
	return "给自己安排一次定时唤醒：到点后在这个会话里自动跑一轮，按 prompt 继续做事或提醒用户。" +
		"run_at 用 RFC3339 并带时区偏移（例如 2026-10-25T09:00:00+08:00）；不确定现在几点就先调 datetime 工具，" +
		"不要凭记忆推算。适合「明早 9 点提醒我」「今晚 11 点检查构建结果」这类明确时间点的一次性安排；" +
		"周期任务不要用它，用提醒里说明的常规流程。"
}

func (scheduleWakeupTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"run_at": map[string]any{"type": "string", "description": "唤醒时间，RFC3339（带时区），例如 2026-10-25T09:00:00+08:00"},
			"prompt": map[string]any{"type": "string", "description": "到点后交给自己的说明：要做什么、做到什么程度、需不需要问用户"},
			"title":  map[string]any{"type": "string", "description": "一句话标题，列表里显示；留空取 prompt 前 40 字"},
		},
		"required": []string{"run_at", "prompt"},
	}
}

type scheduleWakeupArgs struct {
	RunAt  string `json:"run_at"`
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
}

func (t scheduleWakeupTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var a scheduleWakeupArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	convID, err := wakeupConversationID(ex)
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	conv, err := loadOwnedConversation(t.userID, convID)
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	now := time.Now()
	runAt, err := parseWakeupRunAt(a.RunAt, now)
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	prompt, title, err := cleanWakeupText(a.Prompt, a.Title)
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	if n, err := countPendingWakeups(conv.Id); err == nil && n >= wakeupMaxPendingPerConversation {
		return agent.Result{
			IsError: true,
			Content: fmt.Sprintf("这个会话已经挂了 %d 条待唤醒（上限 %d），先取消几条再安排",
				n, wakeupMaxPendingPerConversation),
		}, nil
	}
	w, err := createWakeup(t.userID, conv.Id, title, prompt, runAt, "assistant")
	if err != nil {
		return agent.Result{IsError: true, Content: "保存唤醒失败：" + err.Error()}, nil
	}
	content := fmt.Sprintf("已安排定时唤醒 #%d：%s（本地时间 %s）。到点后我会在这个会话里继续；需要取消就说一声。",
		w.Id, w.Title, w.RunAt.Local().Format("2006-01-02 15:04"))
	return agent.Result{
		Content: content,
		Display: map[string]any{
			"code": "WAKEUP_SCHEDULED", "id": w.Id,
			"run_at": w.RunAt.Format(time.RFC3339), "title": w.Title,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// list_wakeups
// ---------------------------------------------------------------------------

type listWakeupsTool struct{ userID string }

func (listWakeupsTool) Name() string { return "list_wakeups" }

func (listWakeupsTool) Description() string {
	return "查看自己安排过的定时唤醒（默认只看待唤醒）。status 可选 pending/done/failed/canceled；" +
		"想确认「有没有定过」「定在几点」时用它，不要凭记忆回答。"
}

func (listWakeupsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"pending", "done", "failed", "canceled", ""}, "description": "缺省 pending"},
			"limit":  map[string]any{"type": "number", "description": "最多返回几条，默认 10，上限 50"},
		},
	}
}

type listWakeupsArgs struct {
	Status string `json:"status"`
	Limit  int    `json:"limit"`
}

func (t listWakeupsTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a listWakeupsArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	status := strings.TrimSpace(a.Status)
	if status == "" {
		status = model.WakeupPending
	}
	limit := a.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := listUserWakeups(t.userID, 0, status, limit)
	if err != nil {
		return agent.Result{IsError: true, Content: "读取唤醒失败：" + err.Error()}, nil
	}
	if len(rows) == 0 {
		return agent.Result{Content: "没有" + wakeupStatusLabel(status) + "的定时唤醒。"}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s的定时唤醒（%d 条）：", wakeupStatusLabel(status), len(rows))
	for _, w := range rows {
		fmt.Fprintf(&b, "\n- #%d %s · %s（会话 #%d）", w.Id,
			w.RunAt.Local().Format("2006-01-02 15:04"), w.Title, w.ConversationID)
		if w.Status == model.WakeupFailed && w.Error != "" {
			b.WriteString(" · 失败：" + w.Error)
		}
	}
	return agent.Result{Content: b.String()}, nil
}

func wakeupStatusLabel(status string) string {
	switch status {
	case model.WakeupPending:
		return "待执行"
	case model.WakeupDone:
		return "已执行"
	case model.WakeupFailed:
		return "失败"
	case model.WakeupCanceled:
		return "已取消"
	}
	return status
}

// ---------------------------------------------------------------------------
// cancel_wakeup
// ---------------------------------------------------------------------------

type cancelWakeupTool struct{ userID string }

func (cancelWakeupTool) Name() string { return "cancel_wakeup" }

func (cancelWakeupTool) Description() string {
	return "取消一条还没执行的定时唤醒（用 list_wakeups 查到 id）。已经执行完或已取消的不能再取消。"
}

func (cancelWakeupTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "number", "description": "唤醒 id，来自 schedule_wakeup 或 list_wakeups"},
		},
		"required": []string{"id"},
	}
}

type cancelWakeupArgs struct {
	Id uint `json:"id"`
}

func (t cancelWakeupTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	var a cancelWakeupArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if a.Id == 0 {
		return agent.Result{IsError: true, Content: "缺少 id"}, nil
	}
	w, err := cancelUserWakeup(t.userID, a.Id)
	if errors.Is(err, errWakeupNotFound) {
		return agent.Result{IsError: true, Content: "没有这条唤醒（或不属于当前用户）"}, nil
	}
	if err != nil {
		return agent.Result{IsError: true, Content: err.Error()}, nil
	}
	return agent.Result{
		Content: fmt.Sprintf("已取消定时唤醒 #%d：%s", w.Id, w.Title),
		Display: map[string]any{"code": "WAKEUP_CANCELED", "id": w.Id},
	}, nil
}
