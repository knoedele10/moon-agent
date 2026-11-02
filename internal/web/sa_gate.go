package web

// Superagent 准入（docs/agent/18-superagent.md §3.2）。工作线 B。
//
// gate 是判断的第一层：只看「谁、在哪、有没有对 agent 说话」，确定性地给出强度。
// 它不读消息内容、不调模型；内容判断在前台轮次里做。

import (
	"strings"

	"blog-my/internal/model"
)

// saThreadScope 从 Thread 的键里取出接收范围和群号："qq:group:<gid>" → ("group", gid)。
func saThreadScope(threadKey string) (scope, groupID string) {
	parts := strings.SplitN(threadKey, ":", 3)
	if len(parts) < 2 {
		return "", ""
	}
	scope = parts[1]
	if len(parts) == 3 && scope == "group" {
		groupID = parts[2]
	}
	return scope, groupID
}

// saTriggerThreadKey 取 Trigger 所在 Thread 的键；库不可用时返回空串（按私聊处理）。
var saTriggerThreadKey = func(t model.SATrigger) string {
	if model.DB == nil || t.ThreadID == 0 {
		return ""
	}
	var th model.SAThread
	if err := model.DB.Select("thread_key").Where("id = ?", t.ThreadID).First(&th).Error; err != nil {
		return ""
	}
	return th.ThreadKey
}

func saCellStrength(cell string) string {
	switch cell {
	case saCellExplicit:
		return model.SAStrengthExplicit
	case saCellInferred:
		return model.SAStrengthInferred
	}
	return model.SAStrengthNone
}

// saGateDecide 是 gate 的纯函数部分：threadKey 由调用方给出。
func saGateDecide(p model.SAPolicy, t model.SATrigger, threadKey string, threadHasLiveTicket bool) (strength, reason string) {
	strength, reason = saGateRaw(p, t, threadKey, threadHasLiveTicket)
	if strength == model.SAStrengthInferred && p.AcceptMode == model.SAAcceptOff {
		return model.SAStrengthNone, reason + "；接单模式为 off，推断类不受理"
	}
	return strength, reason
}

func saGateRaw(p model.SAPolicy, t model.SATrigger, threadKey string, hasTicket bool) (string, string) {
	addressed := t.Addressed == model.SAAddressedDirect || t.Addressed == model.SAAddressedMention
	switch t.Source {
	case model.SASourceWeb:
		if t.ActorKind == model.SAActorHuman && t.ActorID != "" && t.ActorID == p.UserID {
			return model.SAStrengthExplicit, "本机主人在网页上的请求"
		}
		return model.SAStrengthNone, "网页上只有账号本人可以发起"
	case model.SASourceQQ:
	default:
		return model.SAStrengthNone, "该来源不接受请求：" + t.Source
	}

	src := saSources(p)[t.Source]
	scope, groupID := saThreadScope(threadKey)
	if scope == "group" && !saListed(src.AllowGroups, groupID) {
		return model.SAStrengthNone, "这个群不在接收名单里"
	}

	switch t.ActorKind {
	case model.SAActorHuman:
	case model.SAActorBot:
		// 名单内的机器人只能产生推断候选，而且必须对 agent 说话。
		if saListed(src.Bots, t.ActorID) && addressed {
			return model.SAStrengthInferred, "名单内机器人的请求"
		}
		return model.SAStrengthNone, "机器人消息不受理"
	default:
		return model.SAStrengthNone, "系统消息不经过判断"
	}

	listed := saListed(src.AllowUsers, t.ActorID)
	adm := src.admission(scope)
	switch {
	case listed && addressed:
		return model.SAStrengthExplicit, "名单内的人对 agent 说话"
	case listed:
		return saCellStrength(adm.ListedUnaddressed), "名单内的人，没有对 agent 说话"
	case addressed && adm.UnlistedAddressed == saCellExplicit:
		// 准入表显式放开的范围：即使在有工单的 Thread 上也按 explicit。
		return model.SAStrengthExplicit, "准入表允许名单外的人发起"
	case hasTicket:
		return model.SAStrengthInferred, "名单外的人在已有工单的对话里跟进"
	case addressed:
		return saCellStrength(adm.UnlistedAddressed), "名单外的人对 agent 说话"
	default:
		return saCellStrength(adm.UnlistedUnaddressed), "名单外的人，没有对 agent 说话"
	}
}

func init() {
	saGate = func(p model.SAPolicy, t model.SATrigger, threadHasLiveTicket bool) (string, string) {
		return saGateDecide(p, t, saTriggerThreadKey(t), threadHasLiveTicket)
	}
}
