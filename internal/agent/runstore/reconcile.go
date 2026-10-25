package runstore

import "strings"

const (
	// StatusRecovered 表示进程重启后确认这条 run 可以留下，不重放任何工具。
	StatusRecovered Status = "recovered"
	// StatusNeedsConfirm 表示有未知副作用，等用户决定，仍然不自动重放。
	StatusNeedsConfirm Status = "needs_confirm"
)

// ToolEffect 是续跑决策用的三类效果。未知副作用不会自动重放。
type ToolEffect string

const (
	EffectRead       ToolEffect = "read"
	EffectIdempotent ToolEffect = "idempotent"
	EffectUnknown    ToolEffect = "unknown"
)

// EffectOf 把工具权限表里的副作用字符串收成三类。
func EffectOf(sideEffect string) ToolEffect {
	switch strings.TrimSpace(sideEffect) {
	case "read_only":
		return EffectRead
	case "idempotent_mutation":
		return EffectIdempotent
	default:
		return EffectUnknown
	}
}

type toolState struct {
	effect   ToolEffect
	finished bool
}

// Classify 看 journal 决定重启后怎么标记。它不执行工具。
//
// 已结束的 run 保持原状态。仍在 running/waiting 时：
// 有未完成或结果未知的非只读/非幂等调用 → needs_confirm；
// 否则标 recovered，调用方只改 journal，不重放。
func Classify(events []Event) Status {
	if len(events) == 0 {
		return StatusAborted
	}
	last := events[len(events)-1].Status
	switch last {
	case StatusDone, StatusFailed, StatusAborted, StatusRecovered, StatusNeedsConfirm:
		return last
	}
	open := map[string]*toolState{}
	order := []string{}
	for _, ev := range events {
		id := toolID(ev)
		switch ev.Type {
		case "tool_started":
			if id == "" {
				id = ev.Tool + "#" + ev.At.Format("20060102150405.000000000")
			}
			if _, ok := open[id]; !ok {
				order = append(order, id)
			}
			open[id] = &toolState{effect: effectOfEvent(ev)}
		case "tool_finished":
			st := open[id]
			if st == nil {
				st = &toolState{effect: effectOfEvent(ev)}
				open[id] = st
				order = append(order, id)
			}
			st.finished = true
			if st.effect == "" {
				st.effect = effectOfEvent(ev)
			}
		}
	}
	for _, id := range order {
		st := open[id]
		if st.effect == EffectRead || st.effect == EffectIdempotent {
			continue
		}
		if !st.finished || st.effect == EffectUnknown {
			return StatusNeedsConfirm
		}
	}
	return StatusRecovered
}

func effectOfEvent(ev Event) ToolEffect {
	if ev.Tool != "" && len(ev.Payload) > 0 {
		// payload 里如果带 effect 字段，优先用它。解析失败就看工具名缺省为 unknown。
	}
	var body struct {
		Effect string `json:"effect"`
	}
	if len(ev.Payload) > 0 {
		_ = jsonUnmarshal(ev.Payload, &body)
		if body.Effect != "" {
			return EffectOf(body.Effect)
		}
	}
	return EffectUnknown
}

func toolID(ev Event) string {
	var body struct {
		ID string `json:"id"`
	}
	if len(ev.Payload) > 0 {
		_ = jsonUnmarshal(ev.Payload, &body)
	}
	if body.ID != "" {
		return body.ID
	}
	return ev.Tool
}

// Reconcile 把仍标着 running/waiting 的 journal 收成 recovered 或 needs_confirm。
// 不重放工具调用。
func (s *Store) Reconcile() ([]Summary, error) {
	open, err := s.Recoverable()
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, summary := range open {
		events, err := s.Read(summary.RunID)
		if err != nil {
			continue
		}
		next := Classify(events)
		if next == summary.Status {
			continue
		}
		if err := s.Append(summary.RunID, summary.SessionID, "run_reconciled", next, map[string]any{
			"previous": summary.Status,
			"replay":   false,
		}); err != nil {
			return out, err
		}
		summary.Status = next
		out = append(out, summary)
	}
	return out, nil
}
