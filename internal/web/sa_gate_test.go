package web

import (
	"encoding/json"
	"testing"

	"blog-my/internal/model"
)

func saTestPolicy(t *testing.T, mode string, qq saSourcePolicy) model.SAPolicy {
	t.Helper()
	b, err := json.Marshal(map[string]saSourcePolicy{model.SASourceQQ: qq})
	if err != nil {
		t.Fatal(err)
	}
	return model.SAPolicy{UserID: "owner", Version: 3, AcceptMode: mode, Sources: string(b), Rules: "[]"}
}

// 契约 §3.2 的准入表：每一行一个用例。
func TestSAGateTable(t *testing.T) {
	qq := saSourcePolicy{AllowUsers: []string{"100"}, AllowGroups: []string{"900"}, Bots: []string{"777"}}
	open := qq
	open.Admission = map[string]saAdmission{"group": {UnlistedAddressed: saCellExplicit}}
	quiet := qq
	quiet.Admission = map[string]saAdmission{"group": {ListedUnaddressed: saCellIgnore}}

	human := func(actor, addressed string) model.SATrigger {
		return model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorHuman, ActorID: actor, Addressed: addressed}
	}
	cases := []struct {
		name      string
		policy    model.SAPolicy
		trig      model.SATrigger
		threadKey string
		hasTicket bool
		want      string
	}{
		{"本机主人网页", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceWeb, ActorKind: model.SAActorHuman, ActorID: "owner", Addressed: model.SAAddressedDirect}, "web:conv:1", false, model.SAStrengthExplicit},
		{"网页上的别人", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceWeb, ActorKind: model.SAActorHuman, ActorID: "x"}, "web:conv:1", false, model.SAStrengthNone},
		{"名单内私聊", saTestPolicy(t, "observe", qq), human("100", model.SAAddressedDirect), "qq:private:100", false, model.SAStrengthExplicit},
		{"名单内群 @", saTestPolicy(t, "observe", qq), human("100", model.SAAddressedMention), "qq:group:900", false, model.SAStrengthExplicit},
		{"名单内群没 @", saTestPolicy(t, "observe", qq), human("100", model.SAAddressedNone), "qq:group:900", false, model.SAStrengthInferred},
		{"名单内群没 @，准入表 ignore", saTestPolicy(t, "observe", quiet), human("100", model.SAAddressedNone), "qq:group:900", false, model.SAStrengthNone},
		{"名单外私聊，默认 ignore", saTestPolicy(t, "observe", qq), human("200", model.SAAddressedDirect), "qq:private:200", false, model.SAStrengthNone},
		{"名单外群 @，准入表 explicit", saTestPolicy(t, "observe", open), human("200", model.SAAddressedMention), "qq:group:900", false, model.SAStrengthExplicit},
		{"名单外群 @，准入表 explicit，有工单仍 explicit", saTestPolicy(t, "off", open), human("200", model.SAAddressedMention), "qq:group:900", true, model.SAStrengthExplicit},
		{"名单外在有工单的对话里跟进", saTestPolicy(t, "observe", qq), human("200", model.SAAddressedNone), "qq:group:900", true, model.SAStrengthInferred},
		{"不在名单的群", saTestPolicy(t, "observe", qq), human("100", model.SAAddressedMention), "qq:group:901", false, model.SAStrengthNone},
		{"名单内机器人 @", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorBot, ActorID: "777", Addressed: model.SAAddressedMention}, "qq:group:900", false, model.SAStrengthInferred},
		{"名单内机器人没 @", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorBot, ActorID: "777", Addressed: model.SAAddressedNone}, "qq:group:900", false, model.SAStrengthNone},
		{"其它机器人", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorBot, ActorID: "778", Addressed: model.SAAddressedMention}, "qq:group:900", false, model.SAStrengthNone},
		{"系统消息", saTestPolicy(t, "observe", qq), model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorSystem}, "qq:private:100", false, model.SAStrengthNone},
		{"off 模式下推断不受理", saTestPolicy(t, "off", qq), human("100", model.SAAddressedNone), "qq:group:900", false, model.SAStrengthNone},
		{"off 模式下明确请求照常", saTestPolicy(t, "off", qq), human("100", model.SAAddressedDirect), "qq:private:100", false, model.SAStrengthExplicit},
		{"未知来源", saTestPolicy(t, "observe", qq), model.SATrigger{Source: "mail", ActorKind: model.SAActorHuman}, "mail:x", false, model.SAStrengthNone},
	}
	for _, c := range cases {
		got, why := saGateDecide(c.policy, c.trig, c.threadKey, c.hasTicket)
		if got != c.want {
			t.Errorf("%s: got %s want %s (%s)", c.name, got, c.want, why)
		}
		if why == "" {
			t.Errorf("%s: 缺少原因", c.name)
		}
	}
}

func TestSAGateWildcardList(t *testing.T) {
	p := saTestPolicy(t, "observe", saSourcePolicy{AllowUsers: []string{"*"}, AllowGroups: []string{"*"}})
	trig := model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorHuman, ActorID: "5", Addressed: model.SAAddressedMention}
	if got, _ := saGateDecide(p, trig, "qq:group:1", false); got != model.SAStrengthExplicit {
		t.Fatalf("* 应放行所有人和群: %s", got)
	}
}
