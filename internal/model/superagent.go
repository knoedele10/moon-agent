package model

import "time"

// Superagent 主框架的领域表（docs/agent/18-superagent.md）。
//
// 这些表是三条并行工作线的**共享契约**：字段的含义、取值和唯一约束都以契约文档为准。
// 工作线只读写自己负责的列；要加列或改语义，先改契约文档，再改这里。
//
//	Source ──Trigger──▶ Thread ──(gate + 前台判断)──▶ Decision ──accept──▶ Ticket(=AgentTask) + Mandate
//	                       │                                                   │
//	                       └──────────── Subscription（后续消息扇出）◀──────────┘
//	Ticket / 前台回复 ──▶ Outbound（先落库再发送）──▶ Source 的 Channel

// 来源（Trigger.Source / Thread.Source）。
const (
	SASourceWeb       = "web"
	SASourceQQ        = "qq"
	SASourceWakeup    = "wakeup"
	SASourceProactive = "proactive"
)

// 发送者类型（Trigger.ActorKind）。
const (
	SAActorHuman  = "human"
	SAActorBot    = "bot"
	SAActorSystem = "system"
)

// 是否在对 agent 说话（Trigger.Addressed）。
const (
	SAAddressedDirect  = "direct"  // 私聊、网页对话
	SAAddressedMention = "mention" // 群里 @ 了机器人
	SAAddressedNone    = "none"    // 群里没 @、旁听到的消息
)

// Trigger 的去向（Trigger.Disposition）。
const (
	SADispFiltered  = "filtered"   // gate 拒收（不在名单、模式 off 等）
	SADispQueued    = "queued"     // 等待前台判断
	SADispEvaluated = "evaluated"  // 已有 Decision
	SADispFannedOut = "fanned_out" // 交给了已有工单（不经过判断）
	SADispInline    = "inline"     // 调用方自己同步跑了前台轮次（网页 SSE）
)

// 授权强度（Decision.Strength / Mandate.Strength）。
const (
	SAStrengthExplicit = "explicit"
	SAStrengthInferred = "inferred"
	SAStrengthNone     = "none"
)

// 决定（Decision.Kind）。
const (
	SADecisionReply       = "reply"       // 前台直接回答，没建工单
	SADecisionAccept      = "accept"      // 建工单
	SADecisionAttach      = "attach"      // 交给已有工单
	SADecisionStop        = "stop"        // 停止工单
	SADecisionIgnore      = "ignore"      // 不处理（observe 模式下的「本会接单」也记成 ignore）
	SADecisionUnavailable = "unavailable" // 判断没跑完（超时、出错）
)

// 接单模式（Policy.AcceptMode）。
const (
	SAAcceptOff     = "off"
	SAAcceptObserve = "observe"
	SAAcceptAuto    = "auto"
)

// 订阅角色（Subscription.Role）。
const (
	SASubOrigin   = "origin"   // 工单发起的那个 Thread：结果、提问回到这里
	SASubAttached = "attached" // 前台显式 attach 进来的 Thread
	SASubContext  = "context"  // 只读上下文，不赋予任何权限
)

// 外发消息（Outbound）的用途与状态。
const (
	SAPurposeReply    = "reply"    // 前台回答
	SAPurposeAck      = "ack"      // 工单受理确认
	SAPurposeQuestion = "question" // 需要人回答
	SAPurposeResult   = "result"   // 工单结局
	SAPurposeNotice   = "notice"   // 其它通知

	SAOutPending = "pending"
	SAOutSent    = "sent"
	SAOutFailed  = "failed" // 连续失败，仍会重试，计入系统问题
	SAOutDropped = "dropped"
)

// SAThread 是一个对话：网页会话、QQ 私聊、QQ 群。同一 Thread 同一时刻最多一次前台判断。
type SAThread struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;size:36;not null;index"`
	// ThreadKey 全局唯一："web:conv:<id>" / "qq:private:<qq>" / "qq:group:<gid>"。
	ThreadKey string `gorm:"column:thread_key;size:96;not null;uniqueIndex"`
	Source    string `gorm:"column:source;size:16;not null"`
	Label     string `gorm:"column:label;size:200"`
	// ConversationID 是这个 Thread 的前台会话：前台轮次在这里跑，网页渠道的回复也写在这里。
	ConversationID uint `gorm:"column:conversation_id;index"`
	// EvalState：idle / queued / evaluating；EvalLease 是 evaluating 的租约到期时间。
	EvalState string     `gorm:"column:eval_state;size:16;not null;default:idle"`
	EvalLease *time.Time `gorm:"column:eval_lease"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

func (SAThread) TableName() string { return "sa_thread" }

// SATrigger 是一条不可变的入站事件。(Source, SourceMsgID) 唯一：同一事件只入库一次。
type SATrigger struct {
	Id          uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      string `gorm:"column:user_id;size:36;not null;index"`
	ThreadID    uint   `gorm:"column:thread_id;not null;index"`
	Source      string `gorm:"column:source;size:16;not null;uniqueIndex:idx_sa_trigger_src,priority:1"`
	SourceMsgID string `gorm:"column:source_msg_id;size:96;not null;uniqueIndex:idx_sa_trigger_src,priority:2"`
	// ReplyToMsgID 是被回复消息在来源里的 id（QQ 引用回复）；用来判断是不是在回某个工单的消息。
	ReplyToMsgID string `gorm:"column:reply_to_msg_id;size:96"`
	ActorID      string `gorm:"column:actor_id;size:64"`
	ActorName    string `gorm:"column:actor_name;size:128"`
	ActorKind    string `gorm:"column:actor_kind;size:16"`
	Addressed    string `gorm:"column:addressed;size:16"`
	Text         string `gorm:"column:text;type:text"`
	// Raw 是来源原始载荷（JSON），只供排查，不喂给模型。
	Raw         string    `gorm:"column:raw;type:longtext"`
	Disposition string    `gorm:"column:disposition;size:16;index"`
	DecisionID  uint      `gorm:"column:decision_id"`
	CreatedAt   time.Time `gorm:"column:created_at;index"`
}

func (SATrigger) TableName() string { return "sa_trigger" }

// SADecision 是一次判断的结果。一个批次可能产出多条（批量路由时每项一条）。
type SADecision struct {
	Id         uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID     string `gorm:"column:user_id;size:36;not null;index"`
	ThreadID   uint   `gorm:"column:thread_id;not null;index"`
	TriggerIDs string `gorm:"column:trigger_ids;type:text"` // JSON 数组
	Kind       string `gorm:"column:kind;size:16;not null"`
	Strength   string `gorm:"column:strength;size:16"`
	Reason     string `gorm:"column:reason;type:text"`
	// WouldAccept：observe 模式下判断为「本会接单」，但记成 ignore。
	WouldAccept   bool      `gorm:"column:would_accept"`
	TaskID        uint      `gorm:"column:task_id;index"`
	PolicyVersion int       `gorm:"column:policy_version"`
	By            string    `gorm:"column:by;size:16"` // gate / router / panel
	CreatedAt     time.Time `gorm:"column:created_at;index"`
}

func (SADecision) TableName() string { return "sa_decision" }

// SAMandate 是工单的授权：谁、经由哪里、以什么强度、在哪个策略版本下授权了这份工作。创建后不改，
// 只有 WithdrawnAt（人叫停）与 RevokedAt（策略复核失败）两个终结字段可写。
type SAMandate struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;size:36;not null;index"`
	TaskID uint   `gorm:"column:task_id;not null;uniqueIndex"`
	// OriginTriggerID 唯一：同一条消息被接单两次只会有一个工单。
	OriginTriggerID uint   `gorm:"column:origin_trigger_id;not null;uniqueIndex"`
	ThreadID        uint   `gorm:"column:thread_id;not null"`
	Source          string `gorm:"column:source;size:16"`
	ActorID         string `gorm:"column:actor_id;size:64"`
	Strength        string `gorm:"column:strength;size:16"`
	Scope           string `gorm:"column:scope;type:text"`
	// Kind / Mode 是这份授权允许的上限：工单的执行配置不得超过它。
	Kind          string     `gorm:"column:kind;size:16"`
	Mode          string     `gorm:"column:mode;size:16"`
	MatchedRules  string     `gorm:"column:matched_rules;type:text"` // JSON 数组，推断接单时必填
	PolicyVersion int        `gorm:"column:policy_version"`
	WithdrawnAt   *time.Time `gorm:"column:withdrawn_at"`
	RevokedAt     *time.Time `gorm:"column:revoked_at"`
	EndReason     string     `gorm:"column:end_reason;size:255"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

func (SAMandate) TableName() string { return "sa_mandate" }

// SASubscription：工单 ↔ Thread 多对多。DroppedAt 非空表示已退订。
type SASubscription struct {
	Id        uint       `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    uint       `gorm:"column:task_id;not null;uniqueIndex:idx_sa_sub,priority:1"`
	ThreadID  uint       `gorm:"column:thread_id;not null;uniqueIndex:idx_sa_sub,priority:2;index"`
	Role      string     `gorm:"column:role;size:16;not null"`
	DroppedAt *time.Time `gorm:"column:dropped_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

func (SASubscription) TableName() string { return "sa_subscription" }

// SAOutbound 是外发消息：先落库，再由发送器按 Thread 内顺序发出，失败退避重试。
type SAOutbound struct {
	Id       uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID   string `gorm:"column:user_id;size:36;not null;index"`
	ThreadID uint   `gorm:"column:thread_id;not null;index"`
	TaskID   uint   `gorm:"column:task_id;index"`
	Purpose  string `gorm:"column:purpose;size:16;not null"`
	Text     string `gorm:"column:text;type:longtext"`
	// ReplyToTriggerID：渠道支持引用时引用这条入站消息。
	ReplyToTriggerID uint `gorm:"column:reply_to_trigger_id"`
	// DedupeKey 唯一：同一件事重复入队只发一次（如 "task:12:result:3"）。
	DedupeKey     string     `gorm:"column:dedupe_key;size:128;uniqueIndex"`
	Status        string     `gorm:"column:status;size:16;not null;index"`
	Attempts      int        `gorm:"column:attempts;not null;default:0"`
	NextAttemptAt *time.Time `gorm:"column:next_attempt_at;index"`
	LastError     string     `gorm:"column:last_error;size:512"`
	// SentRef 是发出后来源给的消息 id（QQ message_id / 网页 message.id）；
	// 之后有人「回复这条」时据此找到它属于哪个工单。
	SentRef    string     `gorm:"column:sent_ref;size:96;index"`
	SentAt     *time.Time `gorm:"column:sent_at"`
	DropReason string     `gorm:"column:drop_reason;size:255"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

func (SAOutbound) TableName() string { return "sa_outbound" }

// SAPolicy 是每个用户的接单策略，Version 每次保存 +1（工单的 Mandate 记录它）。
type SAPolicy struct {
	Id         uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID     string `gorm:"column:user_id;size:36;not null;uniqueIndex"`
	Version    int    `gorm:"column:version;not null;default:1"`
	AcceptMode string `gorm:"column:accept_mode;size:16;not null;default:observe"`
	// Sources 是各来源的准入配置 JSON：{"qq":{"allow_users":[],"allow_groups":[],"bots":[],"admission":{...}}}。
	Sources string `gorm:"column:sources;type:longtext"`
	// Rules 是主动接单规则 JSON 数组：[{"id","name","instruction","enabled"}]。
	Rules string `gorm:"column:rules;type:longtext"`
	// GuestKind / GuestMode 是非本机主人发起的工单的授权上限。
	GuestKind string    `gorm:"column:guest_kind;size:16;not null;default:chat"`
	GuestMode string    `gorm:"column:guest_mode;size:16;not null;default:confirm"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (SAPolicy) TableName() string { return "sa_policy" }

// SAModels 是 superagent 的全部表（MySQL 与 SQLite 两条迁移路径共用）。
func SAModels() []any {
	return []any{&SAThread{}, &SATrigger{}, &SADecision{}, &SAMandate{}, &SASubscription{}, &SAOutbound{}, &SAPolicy{}}
}
