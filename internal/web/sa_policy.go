package web

// Superagent 接单策略（docs/agent/18-superagent.md §3.2 / §3.7）。工作线 B。
//
// 每个用户一行 sa_policy。第一次读到时按默认值建行：QQ 名单取自 QQ_ALLOW_USERS /
// QQ_ALLOW_GROUPS，之后只以表为准（环境变量改了不再覆盖）。每次保存 version+1，
// 工单的 Mandate 记下接单时的版本，复核时据此判断规则是否变化。

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 准入单元格的取值。
const (
	saCellExplicit = "explicit"
	saCellInferred = "inferred"
	saCellIgnore   = "ignore"
)

// saAdmission 是一个接收范围的准入表（dsh 002 §Admission）：
//   - ListedUnaddressed：名单内、没对 agent 说话 → inferred / ignore（默认 inferred）
//   - UnlistedAddressed：名单外、对 agent 说话 → explicit / inferred / ignore（默认 ignore）
//   - UnlistedUnaddressed：名单外、没对 agent 说话 → inferred / ignore（默认 ignore）
type saAdmission struct {
	ListedUnaddressed   string `json:"listed_unaddressed,omitempty"`
	UnlistedAddressed   string `json:"unlisted_addressed,omitempty"`
	UnlistedUnaddressed string `json:"unlisted_unaddressed,omitempty"`
}

// saSourcePolicy 是一个来源在策略里的部分，身份一律用该来源自己的 id。
type saSourcePolicy struct {
	// Owners 是本机主人在这个来源上的身份（QQ 号）。只认精确匹配，不支持 "*"。
	Owners      []string `json:"owners"`
	AllowUsers  []string `json:"allow_users"`
	AllowGroups []string `json:"allow_groups"`
	Bots        []string `json:"bots"`
	// Admission 按接收范围分：QQ 为 "private" / "group"。缺省的范围按默认值。
	Admission map[string]saAdmission `json:"admission,omitempty"`
}

type saRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
	Enabled     bool   `json:"enabled"`
}

func saSources(p model.SAPolicy) map[string]saSourcePolicy {
	out := map[string]saSourcePolicy{}
	if strings.TrimSpace(p.Sources) != "" {
		_ = json.Unmarshal([]byte(p.Sources), &out)
	}
	return out
}

func saRules(p model.SAPolicy) []saRule {
	var out []saRule
	if strings.TrimSpace(p.Rules) != "" {
		_ = json.Unmarshal([]byte(p.Rules), &out)
	}
	return out
}

func saEnabledRules(p model.SAPolicy) []saRule {
	var out []saRule
	for _, r := range saRules(p) {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

func (s saSourcePolicy) admission(scope string) saAdmission {
	a := s.Admission[scope]
	if a.ListedUnaddressed == "" {
		a.ListedUnaddressed = saCellInferred
	}
	if a.UnlistedAddressed == "" {
		a.UnlistedAddressed = saCellIgnore
	}
	if a.UnlistedUnaddressed == "" {
		a.UnlistedUnaddressed = saCellIgnore
	}
	return a
}

func saListed(list []string, id string) bool {
	for _, v := range list {
		if v == "*" || (id != "" && v == id) {
			return true
		}
	}
	return false
}

// saSplitIDs 把「逗号/空格/分号分隔」的 id 列表拆开、去重、排序。
func saSplitIDs(raw string) []string {
	set := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		set[part] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// saDefaultPolicy 是用户第一次用到 superagent 时的策略。
//
// 兼容接入 QQ 时的行为：配了 QQ_ALLOW_GROUPS 时，群里任何人 @ 机器人都会被回答，
// 所以群范围的「名单外 @」默认设为 explicit（受访客上限约束），其余单元格按 dsh 默认。
func saDefaultPolicy(userID string) model.SAPolicy {
	qq := saSourcePolicy{
		Owners:      saSplitIDs(os.Getenv("QQ_OWNER")),
		AllowUsers:  saSplitIDs(os.Getenv("QQ_ALLOW_USERS")),
		AllowGroups: saSplitIDs(os.Getenv("QQ_ALLOW_GROUPS")),
		Bots:        []string{},
	}
	if len(qq.AllowGroups) > 0 {
		qq.Admission = map[string]saAdmission{"group": {UnlistedAddressed: saCellExplicit}}
	}
	sources, _ := json.Marshal(map[string]saSourcePolicy{model.SASourceQQ: qq})
	guestKind := KindChat
	if k := strings.TrimSpace(os.Getenv("QQ_BOT_KIND")); k != "" {
		guestKind = normalizeKind(k)
	}
	return model.SAPolicy{
		UserID: userID, Version: 1, AcceptMode: model.SAAcceptObserve,
		Sources: string(sources), Rules: "[]", GuestKind: guestKind, GuestMode: ModeConfirm,
	}
}

func saPolicyForDB(userID string) (model.SAPolicy, error) {
	if model.DB == nil {
		return model.SAPolicy{}, errors.New("数据库未初始化")
	}
	if strings.TrimSpace(userID) == "" {
		return model.SAPolicy{}, errors.New("缺少用户")
	}
	var p model.SAPolicy
	res := model.DB.Where("user_id = ?", userID).Limit(1).Find(&p)
	if res.Error != nil {
		return p, res.Error
	}
	if res.RowsAffected > 0 {
		return p, nil
	}
	p = saDefaultPolicy(userID)
	// 并发首读：唯一索引兜底，输家回头再读一次。
	if err := model.DB.Create(&p).Error; err != nil {
		var again model.SAPolicy
		if model.DB.Where("user_id = ?", userID).First(&again).Error == nil {
			return again, nil
		}
		return p, err
	}
	return p, nil
}

func normalizeAcceptMode(m string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case model.SAAcceptOff:
		return model.SAAcceptOff, true
	case model.SAAcceptObserve:
		return model.SAAcceptObserve, true
	case model.SAAcceptAuto:
		return model.SAAcceptAuto, true
	}
	return "", false
}

func validCell(v string, allowExplicit bool) bool {
	switch v {
	case "", saCellInferred, saCellIgnore:
		return true
	case saCellExplicit:
		return allowExplicit
	}
	return false
}

// saPolicyView 是接口里的策略形状：JSON 列展开成结构。
type saPolicyView struct {
	Version    int                       `json:"version"`
	AcceptMode string                    `json:"accept_mode"`
	Sources    map[string]saSourcePolicy `json:"sources"`
	Rules      []saRule                  `json:"rules"`
	GuestKind  string                    `json:"guest_kind"`
	GuestMode  string                    `json:"guest_mode"`
}

func saPolicyToView(p model.SAPolicy) saPolicyView {
	rules := saRules(p)
	if rules == nil {
		rules = []saRule{}
	}
	return saPolicyView{Version: p.Version, AcceptMode: p.AcceptMode, Sources: saSources(p), Rules: rules, GuestKind: p.GuestKind, GuestMode: p.GuestMode}
}

// saSavePolicy 校验并保存策略。expectVersion>0 时做乐观锁：版本不符返回 errTaskConflict。
func saSavePolicy(userID string, expectVersion int, in saPolicyView) (model.SAPolicy, error) {
	cur, err := saPolicyForDB(userID)
	if err != nil {
		return cur, err
	}
	mode, ok := normalizeAcceptMode(in.AcceptMode)
	if !ok {
		return cur, errors.New("accept_mode 只能是 off / observe / auto")
	}
	for name, s := range in.Sources {
		for _, o := range s.Owners {
			if strings.TrimSpace(o) == "" || strings.Contains(o, "*") {
				return cur, errors.New("主人只能填具体的账号，不能用 *：" + name)
			}
		}
		for scope, a := range s.Admission {
			if !validCell(a.ListedUnaddressed, false) || !validCell(a.UnlistedAddressed, true) || !validCell(a.UnlistedUnaddressed, false) {
				return cur, errors.New("准入单元格取值不合法：" + name + "/" + scope)
			}
		}
	}
	seen := map[string]bool{}
	for i, r := range in.Rules {
		r.ID, r.Name, r.Instruction = strings.TrimSpace(r.ID), strings.TrimSpace(r.Name), strings.TrimSpace(r.Instruction)
		if r.ID == "" || r.Name == "" || r.Instruction == "" || seen[r.ID] {
			return cur, errors.New("规则需要唯一的 id、name 和 instruction")
		}
		seen[r.ID] = true
		in.Rules[i] = r
	}
	if in.Rules == nil {
		in.Rules = []saRule{}
	}
	sources, _ := json.Marshal(in.Sources)
	rules, _ := json.Marshal(in.Rules)
	updates := map[string]any{
		"version": gorm.Expr("version + 1"), "accept_mode": mode, "sources": string(sources), "rules": string(rules),
		"guest_kind": normalizeKind(in.GuestKind), "guest_mode": normalizeMode(in.GuestMode),
	}
	q := model.DB.Model(&model.SAPolicy{}).Where("user_id = ?", userID)
	if expectVersion > 0 {
		q = q.Where("version = ?", expectVersion)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return cur, res.Error
	}
	if res.RowsAffected == 0 {
		return cur, errTaskConflict
	}
	return saPolicyForDB(userID)
}

func getSAPolicyAPI(c *gin.Context) {
	p, err := saPolicyFor(currentUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, saPolicyToView(p))
}

func putSAPolicyAPI(c *gin.Context) {
	var body saPolicyView
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	p, err := saSavePolicy(currentUserID(c), body.Version, body)
	if errors.Is(err, errTaskConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": "策略已被修改，请刷新后重试"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, saPolicyToView(p))
}

// saIsOwnerActor：网页上是账号本人；其它来源看策略里登记的主人。
func saIsOwnerActor(p model.SAPolicy, source, actorID string) bool {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return false
	}
	if source == model.SASourceWeb {
		return actorID == p.UserID
	}
	for _, o := range saSources(p)[source].Owners {
		if o == actorID {
			return true
		}
	}
	return false
}

func init() {
	saPolicyFor = saPolicyForDB
	saIsOwner = saIsOwnerActor
	saRouteRegistrars = append(saRouteRegistrars, func(api *gin.RouterGroup) {
		api.GET("/sa/policy", getSAPolicyAPI)
		api.PUT("/sa/policy", putSAPolicyAPI)
		api.GET("/sa/decisions", listSADecisionsAPI)
	})
}
