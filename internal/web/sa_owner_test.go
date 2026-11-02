package web

import (
	"strings"
	"testing"

	"blog-my/internal/model"
)

// 主人：QQ 上登记的主人号走明确请求，工单按主人自己的设置，不受访客上限。
func TestSAOwnerFromEnvAndGate(t *testing.T) {
	setupSATestDB(t)
	model.DB.Create(&model.User{Id: "u1", Username: "owner"})
	t.Setenv("QQ_OWNER", "42")
	t.Setenv("QQ_ALLOW_USERS", "")
	t.Setenv("QQ_ALLOW_GROUPS", "900")
	p, err := saPolicyForDB("u1")
	if err != nil {
		t.Fatal(err)
	}
	if !saIsOwner(p, model.SASourceQQ, "42") || saIsOwner(p, model.SASourceQQ, "43") || !saIsOwner(p, model.SASourceWeb, "u1") {
		t.Fatal("主人识别不对")
	}
	owner := model.SATrigger{Source: model.SASourceQQ, ActorKind: model.SAActorHuman, ActorID: "42", Addressed: model.SAAddressedDirect}
	if got, _ := saGateDecide(p, owner, "qq:private:42", false); got != model.SAStrengthExplicit {
		t.Fatalf("主人私聊（不在名单里）也应是明确请求: %s", got)
	}
	owner.Addressed = model.SAAddressedMention
	if got, _ := saGateDecide(p, owner, "qq:group:901", false); got != model.SAStrengthNone {
		t.Fatalf("不在接收名单的群，主人也不受理: %s", got)
	}
	owner.Addressed = model.SAAddressedNone
	if got, _ := saGateDecide(p, owner, "qq:group:900", false); got == model.SAStrengthExplicit {
		t.Fatal("主人在群里没 @ 不应是明确请求")
	}

	req := saAcceptReq{UserID: "u1", Policy: p, Origin: model.SATrigger{Source: model.SASourceQQ, ActorID: "42"}}
	if !saActorIsOwner(req) {
		t.Fatal("QQ 主人发起的工单应按主人处理")
	}
	req.Origin.ActorID = "43"
	if saActorIsOwner(req) {
		t.Fatal("其他 QQ 号仍是访客")
	}
	// 只有本机主账号名下的策略能把 QQ 号认作主人。
	model.DB.Create(&model.User{Id: "u2", Username: "other"})
	p2, _ := saPolicyForDB("u2")
	if saActorIsOwner(saAcceptReq{UserID: "u2", Policy: p2, Origin: model.SATrigger{Source: model.SASourceQQ, ActorID: "42"}}) {
		t.Fatal("非主账号的策略不能产生主人")
	}
}

func TestSAOwnerRejectsWildcardAndShowsInPrompt(t *testing.T) {
	f := newSARouterFixture(t, "qq:private:42", func(v *saPolicyView) {
		qq := v.Sources[model.SASourceQQ]
		qq.Owners = []string{"42"}
		v.Sources[model.SASourceQQ] = qq
	})
	bad := saPolicyToView(f.policy)
	qq := bad.Sources[model.SASourceQQ]
	qq.Owners = []string{"*"}
	bad.Sources[model.SASourceQQ] = qq
	if _, err := saSavePolicy("u1", 0, bad); err == nil {
		t.Fatal("主人不能用 *")
	}
	f.trigger(t, "42", model.SAAddressedDirect, "帮我整理文档")
	if p := saFrontPrompt(&f.conv); !strings.Contains(p, "[明确请求·主人]") {
		t.Fatalf("前台提示应标出主人:\n%s", p)
	}
}
