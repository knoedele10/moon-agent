package toolkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	// 内嵌时区库：Windows 上系统不一定有 IANA tzdata，
	// 没有它 LoadLocation("Asia/Shanghai") 会直接失败。
	_ "time/tzdata"

	"blog-my/internal/agent"
)

// DateTimeTool 取当前时间、做日期加减与两个时间点的差值。
type DateTimeTool struct{}

func (DateTimeTool) Name() string { return "datetime" }

func (DateTimeTool) Description() string {
	return "时间日期工具：action=now 取当前时间；action=add 在某个时间上加减（amount + unit）；" +
		"action=diff 算两个时间的间隔。unit 支持 seconds/minutes/hours/days/weeks/months/years；" +
		"timezone 用 IANA 名称（如 Asia/Shanghai），缺省用本机时区。需要“今天几号”“三天后是几号”时用它，不要凭记忆推断。"
}

func (DateTimeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"now", "add", "diff"}, "description": "要做的操作，缺省 now"},
			"datetime": map[string]any{"type": "string", "description": "基准时间；支持 RFC3339、2006-01-02 15:04:05、2006-01-02；缺省为现在"},
			"to":       map[string]any{"type": "string", "description": "action=diff 时的第二个时间"},
			"amount":   map[string]any{"type": "number", "description": "action=add 时的数量，可为负数"},
			"unit":     map[string]any{"type": "string", "description": "action=add/diff 的单位"},
			"timezone": map[string]any{"type": "string", "description": "IANA 时区名，如 Asia/Shanghai；缺省本机时区"},
		},
	}
}

type datetimeArgs struct {
	Action   string  `json:"action"`
	Datetime string  `json:"datetime"`
	To       string  `json:"to"`
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit"`
	Timezone string  `json:"timezone"`
}

func (DateTimeTool) Execute(ctx context.Context, raw json.RawMessage, ex *agent.Exec) (agent.Result, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		default:
		}
	}
	var a datetimeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return agent.Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
	}

	loc := time.Local
	if tz := strings.TrimSpace(a.Timezone); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return agent.Result{}, fmt.Errorf("未知的时区 %q（用 IANA 名称，例如 Asia/Shanghai）", tz)
		}
		loc = l
	}

	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		action = "now"
	}

	base, err := parseTimeFlexible(a.Datetime, loc)
	if err != nil {
		return agent.Result{}, err
	}

	switch action {
	case "now":
		return agent.Result{
			Content: describeTime("当前时间", base, loc),
			Display: map[string]any{
				"iso": base.Format(time.RFC3339), "unix": base.Unix(),
				"weekday": base.Weekday().String(), "timezone": loc.String(),
			},
		}, nil

	case "add":
		unit := strings.ToLower(strings.TrimSpace(a.Unit))
		if unit == "" {
			return agent.Result{}, fmt.Errorf("action=add 需要 unit（seconds/minutes/hours/days/weeks/months/years）")
		}
		res, err := addUnit(base, a.Amount, unit)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{
			Content: describeTime("基准时间", base, loc) + "\n" + describeTime(fmt.Sprintf("加 %g %s 之后", a.Amount, unit), res, loc),
			Display: map[string]any{
				"base": base.Format(time.RFC3339), "result": res.Format(time.RFC3339),
				"unix": res.Unix(), "timezone": loc.String(),
			},
		}, nil

	case "diff":
		if strings.TrimSpace(a.To) == "" {
			return agent.Result{}, fmt.Errorf("action=diff 需要 to（第二个时间）")
		}
		other, err := parseTimeFlexible(a.To, loc)
		if err != nil {
			return agent.Result{}, err
		}
		d := other.Sub(base)
		content := fmt.Sprintf("%s → %s 相差 %s\n总秒数：%d",
			base.Format("2006-01-02 15:04:05"), other.Format("2006-01-02 15:04:05"),
			humanDuration(d), int64(d.Seconds()))
		return agent.Result{
			Content: content,
			Display: map[string]any{"seconds": int64(d.Seconds()), "hours": d.Hours(), "days": d.Hours() / 24},
		}, nil
	}

	return agent.Result{}, fmt.Errorf("未知的 action %q（支持 now/add/diff）", a.Action)
}

// parseTimeFlexible 接受几种常见写法；空串表示“现在”。
func parseTimeFlexible(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().In(loc), nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q（试试 2026-10-12 或 2026-10-12T19:30:00+08:00）", s)
}

func addUnit(t time.Time, amount float64, unit string) (time.Time, error) {
	switch unit {
	case "second", "seconds", "s":
		return t.Add(time.Duration(amount * float64(time.Second))), nil
	case "minute", "minutes", "m":
		return t.Add(time.Duration(amount * float64(time.Minute))), nil
	case "hour", "hours", "h":
		return t.Add(time.Duration(amount * float64(time.Hour))), nil
	case "day", "days", "d":
		return t.AddDate(0, 0, int(amount)), nil
	case "week", "weeks", "w":
		return t.AddDate(0, 0, int(amount)*7), nil
	case "month", "months":
		return t.AddDate(0, int(amount), 0), nil
	case "year", "years", "y":
		return t.AddDate(int(amount), 0, 0), nil
	}
	return time.Time{}, fmt.Errorf("未知的 unit %q", unit)
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		return "-" + humanDuration(-d)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", mins))
	}
	if secs > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d 秒", secs))
	}
	return strings.Join(parts, " ")
}

func describeTime(label string, t time.Time, loc *time.Location) string {
	return fmt.Sprintf("%s（%s）：\n%s\nRFC3339: %s\nUnix: %d\n星期：%s",
		label, loc.String(),
		t.Format("2006-01-02 15:04:05 MST"),
		t.Format(time.RFC3339),
		t.Unix(),
		weekdayCN(t.Weekday()))
}

func weekdayCN(d time.Weekday) string {
	switch d {
	case time.Monday:
		return "星期一"
	case time.Tuesday:
		return "星期二"
	case time.Wednesday:
		return "星期三"
	case time.Thursday:
		return "星期四"
	case time.Friday:
		return "星期五"
	case time.Saturday:
		return "星期六"
	default:
		return "星期日"
	}
}
