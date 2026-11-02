package web

// Superagent 主框架的跨工作线接口（docs/agent/18-superagent.md §4）。
//
// 三条工作线并行开发，彼此只通过这里的函数变量调用：
//   - A 入口与渠道（sa_intake*.go / sa_outbound*.go / sa_channel_*.go）
//   - B 判断与路由（sa_gate*.go / sa_policy*.go / sa_router*.go）
//   - C 工单与执行（sa_ticket*.go / task_*.go）
//
// 每个变量的默认值是「未实现」桩，负责方在自己文件的 init() 里赋上真实实现；
// 其它工作线的单测用 saStub* 帮手换成假实现。签名或语义要改，先改契约文档，再改这里。

import (
	"context"
	"errors"
	"log"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

var errSANotReady = errors.New("superagent: 该环节尚未实现")

// ---------------------------------------------------------------------------
// 共享类型
// ---------------------------------------------------------------------------

// saTriggerIn 是来源适配器交给入口的一条入站事件。
type saTriggerIn struct {
	UserID       string
	Source       string // model.SASource*
	SourceMsgID  string // 来源里的唯一 id；网页用 message.id
	ReplyToMsgID string
	ThreadKey    string // "web:conv:<id>" / "qq:private:<qq>" / "qq:group:<gid>"
	ThreadLabel  string
	// ConversationID：网页来源必填（就是那个会话）；其它来源为 0，由入口为 Thread 建前台会话。
	ConversationID uint
	ActorID        string
	ActorName      string
	ActorKind      string // model.SAActor*
	Addressed      string // model.SAAddressed*
	Text           string
	Raw            string
	// Inline=true：调用方会自己同步跑前台轮次（网页 SSE）。入口只记录、只做扇出判断，不排队判断。
	Inline bool
}

// saIngestResult 是入口的处理结果。
type saIngestResult struct {
	Trigger   model.SATrigger
	Thread    model.SAThread
	Duplicate bool   // 同一 (source, source_msg_id) 已入库：什么都没再做
	FannedOut []uint // 被哪些工单认领（非空时不进入判断）
}

// saOutboundReq 是一条待发消息。DedupeKey 必填。
type saOutboundReq struct {
	UserID           string
	ThreadID         uint
	TaskID           uint
	Purpose          string // model.SAPurpose*
	Text             string
	ReplyToTriggerID uint
	DedupeKey        string
}

// saChannel 是一个来源的写端。Send 返回来源给的消息 id（没有就返回空串）。
// 返回 saPermanentError 包装的错误表示「这条永远发不出去」，发送器会直接丢弃而不重试。
type saChannel interface {
	Send(ctx context.Context, thread model.SAThread, out model.SAOutbound, replyTo *model.SATrigger) (sentRef string, err error)
}

type saPermanentError struct{ err error }

func (e saPermanentError) Error() string { return e.err.Error() }
func (e saPermanentError) Unwrap() error { return e.err }

// saAcceptReq 是路由决定接单时交给工单线的请求。
type saAcceptReq struct {
	UserID      string
	Thread      model.SAThread
	Origin      model.SATrigger // 发起这份工作的那条消息
	Strength    string          // explicit / inferred
	Title       string
	Goal        string // 用户原意 + 路由补充的事实；不得替用户扩大范围
	Constraints string
	Criteria    []taskCriterion
	MatchedRule []string // 推断接单时必填
	Ack         string   // 受理确认（≤100 字），工单线负责经 Outbound 发到原对话
	Policy      model.SAPolicy
	DecisionID  uint
}

// saTicketCard 是前台判断时看到的一张工单摘要。
type saTicketCard struct {
	TaskID uint   `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Stage  string `json:"stage"` // 见契约 §3.4
	Role   string `json:"role"`  // 本 Thread 对它的订阅角色
}

// ---------------------------------------------------------------------------
// A 入口与渠道 提供
// ---------------------------------------------------------------------------

// saIngest：入库 Trigger（去重）→ 找/建 Thread → 调 saFanOut → 未被认领且非 Inline 时调 saEvaluate。
var saIngest = func(ctx context.Context, in saTriggerIn) (saIngestResult, error) {
	return saIngestResult{}, errSANotReady
}

// saEnqueueOutbound：落一条待发消息（按 DedupeKey 幂等）并唤醒发送器。
var saEnqueueOutbound = func(req saOutboundReq) (model.SAOutbound, error) {
	return model.SAOutbound{}, errSANotReady
}

// saChannels：来源 → 渠道实现。A 注册 web 与 qq。
var saChannels = map[string]saChannel{}

// ---------------------------------------------------------------------------
// B 判断与路由 提供
// ---------------------------------------------------------------------------

// saPolicyFor：取用户当前策略；没有就按默认值（含 QQ_* 环境变量）建一行。
var saPolicyFor = func(userID string) (model.SAPolicy, error) {
	return model.SAPolicy{}, errSANotReady
}

// saGate：确定性准入。threadHasLiveTicket 表示该 Thread 有活着的订阅工单（用于后续消息强度）。
var saGate = func(p model.SAPolicy, t model.SATrigger, threadHasLiveTicket bool) (strength, reason string) {
	return model.SAStrengthNone, errSANotReady.Error()
}

// saIsOwner：这个来源身份是不是本机主人（网页上是账号本人；QQ 上是策略里登记的主人 QQ 号）。
// 主人的请求按 explicit 处理，工单用主人自己的设置而不是访客上限。
var saIsOwner = func(p model.SAPolicy, source, actorID string) bool {
	return source == model.SASourceWeb && actorID != "" && actorID == p.UserID
}

// saEvaluate：异步安排该 Thread 的一次前台判断（合并期间到达的 queued Trigger）。不得阻塞调用方。
var saEvaluate = func(ctx context.Context, threadID uint) {
	log.Printf("superagent: saEvaluate 未实现，thread=%d 的消息停在 queued", threadID)
}

// saHandBack：执行端把一条它拥有的消息交回；没有其它工单拥有它时重新进入判断。
var saHandBack = func(ctx context.Context, taskID uint, trig model.SATrigger, reason string) error {
	return errSANotReady
}

// ---------------------------------------------------------------------------
// C 工单与执行 提供
// ---------------------------------------------------------------------------

// saFanOut：这条消息该不该直接交给已有工单（显式引用规则，见契约 §3.3）。返回认领它的工单。
var saFanOut = func(ctx context.Context, trig model.SATrigger) ([]uint, error) {
	return nil, nil
}

// saAcceptTicket：建工单（AgentTask）+ Mandate + origin 订阅 + 受理确认。同一 Origin 幂等。
var saAcceptTicket = func(ctx context.Context, req saAcceptReq) (model.AgentTask, error) {
	return model.AgentTask{}, errSANotReady
}

// saAttachTicket：把一条消息交给已有工单（attached 订阅 + 投递为补充输入），不扩大授权。
var saAttachTicket = func(ctx context.Context, taskID uint, trig model.SATrigger, note string) error {
	return errSANotReady
}

// saStopTicket：停止工单并收回授权。by：router（人在对话里叫停）/ panel。
var saStopTicket = func(ctx context.Context, taskID uint, by, reason string) error {
	return errSANotReady
}

// saLiveTickets：该 Thread 上订阅着的工单（含最近结束、可被续上的），供前台判断。
var saLiveTickets = func(threadID uint) ([]saTicketCard, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 路由注册
// ---------------------------------------------------------------------------

// saRouteRegistrars：各工作线在 init() 里追加自己的 /api 路由，web.go 不再为它们改动。
var saRouteRegistrars []func(api *gin.RouterGroup)
