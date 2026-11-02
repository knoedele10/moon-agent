package web

import (
	"errors"
	"testing"

	"blog-my/internal/model"
)

func TestSAPolicyDefaultsFromEnv(t *testing.T) {
	setupSATestDB(t)
	t.Setenv("QQ_ALLOW_USERS", "2, 1;1")
	t.Setenv("QQ_ALLOW_GROUPS", "9")
	t.Setenv("QQ_BOT_KIND", "work")
	p, err := saPolicyForDB("u1")
	if err != nil {
		t.Fatal(err)
	}
	qq := saSources(p)[model.SASourceQQ]
	if p.Version != 1 || p.AcceptMode != model.SAAcceptObserve || len(qq.AllowUsers) != 2 || qq.AllowUsers[0] != "1" || qq.AllowGroups[0] != "9" {
		t.Fatalf("默认策略不对: %+v %+v", p, qq)
	}
	if qq.admission("group").UnlistedAddressed != saCellExplicit || qq.admission("private").UnlistedAddressed != saCellIgnore {
		t.Fatal("配了群名单时群里名单外 @ 应为 explicit，私聊仍为 ignore")
	}
	if p.GuestKind != KindWork || p.GuestMode != ModeConfirm {
		t.Fatalf("访客上限: %s/%s", p.GuestKind, p.GuestMode)
	}
	// 之后以表为准：环境变量变了不覆盖。
	t.Setenv("QQ_ALLOW_USERS", "3")
	again, _ := saPolicyForDB("u1")
	if again.Id != p.Id || saSources(again)[model.SASourceQQ].AllowUsers[0] != "1" {
		t.Fatal("已有策略不应被环境变量覆盖")
	}
}

func TestSAPolicySaveVersionAndValidation(t *testing.T) {
	setupSATestDB(t)
	p, _ := saPolicyForDB("u1")
	view := saPolicyToView(p)
	view.AcceptMode = "auto"
	view.Rules = []saRule{{ID: "r1", Name: "依赖升级", Instruction: "dependabot 的升级 PR 跑测试", Enabled: true}}
	saved, err := saSavePolicy("u1", p.Version, view)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != p.Version+1 || saved.AcceptMode != model.SAAcceptAuto || len(saEnabledRules(saved)) != 1 {
		t.Fatalf("保存结果: %+v", saved)
	}
	if _, err := saSavePolicy("u1", p.Version, view); !errors.Is(err, errTaskConflict) {
		t.Fatalf("旧版本保存应冲突: %v", err)
	}
	bad := saPolicyToView(saved)
	bad.AcceptMode = "sometimes"
	if _, err := saSavePolicy("u1", saved.Version, bad); err == nil {
		t.Fatal("非法模式应拒绝")
	}
	bad = saPolicyToView(saved)
	bad.Sources = map[string]saSourcePolicy{"qq": {Admission: map[string]saAdmission{"group": {ListedUnaddressed: saCellExplicit}}}}
	if _, err := saSavePolicy("u1", saved.Version, bad); err == nil {
		t.Fatal("没对 agent 说话的单元格不能设为 explicit")
	}
	bad = saPolicyToView(saved)
	bad.Rules = append(bad.Rules, saRule{ID: "r1", Name: "重复", Instruction: "x"})
	if _, err := saSavePolicy("u1", saved.Version, bad); err == nil {
		t.Fatal("重复规则 id 应拒绝")
	}
}
