package web

// QQ 接入：OneBot v11 反向 WebSocket（docs/agent/17-qq-onebot.md）。
//
// NapCat / LLOneBot / Lagrange 这类 OneBot 实现登录一个 QQ 号，再主动连到
// /onebot/v11/ws。适配器只做三件事：
//   - 收消息：转成 saTriggerIn 交给 saIngest。私聊 Addressed=direct，群里 @ 了是 mention，
//     其余（含群里没 @）是 none。群里没 @ 的也入库，回不回话由 gate 决定。
//   - 本地命令：保留 /new、/help。/new 只换这个聊天的前台会话，Thread 不变。
//   - 连接：校验 access token，并把 OneBot 的 echo 回执交给渠道发送（sa_channel_qq.go）。
//
// 适配器不判断谁有权，也不再直接跑一轮。QQ_ALLOW_USERS / QQ_ALLOW_GROUPS 只作为
// 策略首次建行的默认值（契约 §3.1、§3.2）；名单、@ 和模式都交给 gate。
// 业务回复只经发件箱。连接必须带 token。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blog-my/internal/llm"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

type qqConfig struct {
	Enabled bool
	Token   string
	// UserID 是 QQ 消息记在哪个本地账号名下（用它的 key、设置、记忆）。空 = 本机主账号。
	UserID string
	Model  string
	Kind   string
	// AllowUsers / AllowGroups 只保留 QQ_ALLOW_* 的解析结果，供策略默认值对照。
	// 适配器不再用它们决定收不收、回不回（契约 §3.1）。
	AllowUsers  map[string]bool
	AllowGroups map[string]bool
	Timeout     time.Duration
}

func loadQQConfig() qqConfig {
	timeout := time.Duration(agentIntEnv("QQ_BOT_TIMEOUT_SECONDS")) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	kind := KindChat
	if k := strings.TrimSpace(os.Getenv("QQ_BOT_KIND")); k != "" {
		kind = normalizeKind(k)
	}
	return qqConfig{
		Enabled:     envTruthy(os.Getenv("QQ_ONEBOT_ENABLED")),
		Token:       strings.TrimSpace(os.Getenv("QQ_ONEBOT_TOKEN")),
		UserID:      strings.TrimSpace(os.Getenv("QQ_BOT_USER_ID")),
		Model:       strings.TrimSpace(os.Getenv("QQ_BOT_MODEL")),
		Kind:        kind,
		AllowUsers:  qqIDSet(os.Getenv("QQ_ALLOW_USERS")),
		AllowGroups: qqIDSet(os.Getenv("QQ_ALLOW_GROUPS")),
		Timeout:     timeout,
	}
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func qqIDSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if part = strings.TrimSpace(part); part != "" {
			out[part] = true
		}
	}
	return out
}

// allowed 描述 QQ_ALLOW_USERS / QQ_ALLOW_GROUPS 的集合语义（空 = 都不包含，* = 全放行）。
// qqHandleMessage 不调用它。消息是否进入判断由 gate 根据策略决定。
func (c qqConfig) allowed(ev qqEvent) bool {
	if ev.MessageType == "group" {
		return c.AllowGroups["*"] || c.AllowGroups[ev.GroupID.String()]
	}
	return c.AllowUsers["*"] || c.AllowUsers[ev.UserID.String()]
}

// ---------------------------------------------------------------------------
// OneBot 事件解析
// ---------------------------------------------------------------------------

// qqID 兼容 OneBot 实现把 QQ 号发成数字或字符串两种写法。
type qqID string

func (id *qqID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	*id = qqID(s)
	return nil
}

func (id qqID) String() string { return string(id) }

type qqSegment struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type qqEvent struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	SelfID      qqID            `json:"self_id"`
	UserID      qqID            `json:"user_id"`
	GroupID     qqID            `json:"group_id"`
	MessageID   qqID            `json:"message_id"`
	Message     json.RawMessage `json:"message"`
	RawMessage  string          `json:"raw_message"`
	Sender      struct {
		Nickname string `json:"nickname"`
		Card     string `json:"card"`
	} `json:"sender"`
}

func (ev qqEvent) senderName() string {
	if n := strings.TrimSpace(ev.Sender.Card); n != "" {
		return n
	}
	if n := strings.TrimSpace(ev.Sender.Nickname); n != "" {
		return n
	}
	return ev.UserID.String()
}

func (ev qqEvent) chatKey() string {
	if ev.MessageType == "group" {
		return "group:" + ev.GroupID.String()
	}
	return "private:" + ev.UserID.String()
}

var qqCQCode = regexp.MustCompile(`\[CQ:([a-z_]+)((?:,[^\]]*)?)\]`)

// qqExtractText 取出消息里的纯文本，并告诉调用方这条消息有没有 @ 机器人。
// message 字段可能是段数组（array 上报格式），也可能是 CQ 码字符串（string 上报格式）。
func qqExtractText(ev qqEvent) (text string, atSelf bool) {
	self := ev.SelfID.String()
	var segs []qqSegment
	if err := json.Unmarshal(ev.Message, &segs); err == nil && segs != nil {
		var b strings.Builder
		for _, s := range segs {
			switch s.Type {
			case "text":
				var d struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(s.Data, &d)
				b.WriteString(d.Text)
			case "at":
				var d struct {
					QQ qqID `json:"qq"`
				}
				_ = json.Unmarshal(s.Data, &d)
				if self != "" && d.QQ.String() == self {
					atSelf = true
				}
			}
		}
		return strings.TrimSpace(b.String()), atSelf
	}
	raw := ev.RawMessage
	var s string
	if json.Unmarshal(ev.Message, &s) == nil && s != "" {
		raw = s
	}
	text = qqCQCode.ReplaceAllStringFunc(raw, func(code string) string {
		m := qqCQCode.FindStringSubmatch(code)
		if m[1] == "at" && self != "" && strings.Contains(m[2], "qq="+self) {
			atSelf = true
		}
		return ""
	})
	text = strings.NewReplacer("&#91;", "[", "&#93;", "]", "&#44;", ",", "&amp;", "&").Replace(text)
	return strings.TrimSpace(text), atSelf
}

func qqReplyToID(ev qqEvent) string {
	var segs []qqSegment
	if json.Unmarshal(ev.Message, &segs) == nil {
		for _, s := range segs {
			if s.Type != "reply" {
				continue
			}
			var d struct {
				ID qqID `json:"id"`
			}
			if json.Unmarshal(s.Data, &d) == nil && d.ID.String() != "" {
				return d.ID.String()
			}
		}
	}
	raw := ev.RawMessage
	var message string
	if json.Unmarshal(ev.Message, &message) == nil && message != "" {
		raw = message
	}
	m := regexp.MustCompile(`\[CQ:reply,[^\]]*id=([^,\]]+)`)
	if hit := m.FindStringSubmatch(raw); len(hit) == 2 {
		return strings.TrimSpace(hit[1])
	}
	return ""
}

// ---------------------------------------------------------------------------
// 连接与发送
// ---------------------------------------------------------------------------

type qqConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

type qqActionResponse struct {
	messageID string
	err       error
}

var (
	qqPendingMu sync.Mutex
	qqPending   = map[string]chan qqActionResponse{}
)

func qqWaitRegister(echo string) chan qqActionResponse {
	ch := make(chan qqActionResponse, 1)
	qqPendingMu.Lock()
	qqPending[echo] = ch
	qqPendingMu.Unlock()
	return ch
}

func qqWaitRemove(echo string) {
	qqPendingMu.Lock()
	delete(qqPending, echo)
	qqPendingMu.Unlock()
}

func qqWaitResolve(echo string, result qqActionResponse) {
	qqPendingMu.Lock()
	ch := qqPending[echo]
	delete(qqPending, echo)
	qqPendingMu.Unlock()
	if ch != nil {
		ch <- result
	}
}

func qqEchoValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), " \t\r\n\"")
}

func (c *qqConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.ws.WriteJSON(v)
}

var (
	// 按 self_id 记当前活着的连接：回复可能在一轮跑完时才发，期间 OneBot 端可能重连过。
	qqConnsMu sync.Mutex
	qqConns   = map[string]*qqConn{}
	qqEchoSeq atomic.Int64
)

func qqSetConn(selfID string, c *qqConn) {
	qqConnsMu.Lock()
	qqConns[selfID] = c
	qqConnsMu.Unlock()
}

func qqDropConn(selfID string, c *qqConn) {
	qqConnsMu.Lock()
	if qqConns[selfID] == c {
		delete(qqConns, selfID)
	}
	qqConnsMu.Unlock()
}

func qqGetConn(selfID string) *qqConn {
	qqConnsMu.Lock()
	defer qqConnsMu.Unlock()
	return qqConns[selfID]
}

// qqMaxChunk 是单条 QQ 消息的字数上限；超了按段落/行切开分几条发。
const qqMaxChunk = 1800

func qqSplit(text string, max int) []string {
	var out []string
	for text != "" {
		r := []rune(text)
		if len(r) <= max {
			out = append(out, text)
			break
		}
		cut := string(r[:max])
		if i := strings.LastIndex(cut, "\n"); i > len(cut)/2 {
			cut = cut[:i]
		}
		out = append(out, strings.TrimRight(cut, "\n"))
		text = strings.TrimLeft(text[len(cut):], "\n")
	}
	return out
}

// qqSend 把回复发回原聊天。群消息引用原消息，方便群友看出是在回谁。
func qqSend(ev qqEvent, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	conn := qqGetConn(ev.SelfID.String())
	if conn == nil {
		log.Printf("qq: self_id=%s 没有在线连接，回复丢弃", ev.SelfID)
		return
	}
	for i, chunk := range qqSplit(text, qqMaxChunk) {
		msg := []map[string]any{}
		if i == 0 && ev.MessageType == "group" && ev.MessageID != "" {
			msg = append(msg, map[string]any{"type": "reply", "data": map[string]any{"id": ev.MessageID.String()}})
		}
		msg = append(msg, map[string]any{"type": "text", "data": map[string]any{"text": chunk}})
		action, params := "send_private_msg", map[string]any{"user_id": qqNumber(ev.UserID), "message": msg}
		if ev.MessageType == "group" {
			action, params = "send_group_msg", map[string]any{"group_id": qqNumber(ev.GroupID), "message": msg}
		}
		frame := map[string]any{"action": action, "params": params, "echo": "blog-my-" + strconv.FormatInt(qqEchoSeq.Add(1), 10)}
		if err := conn.writeJSON(frame); err != nil {
			log.Printf("qq: 发送失败 %s: %v", ev.chatKey(), err)
			return
		}
	}
}

// qqNumber 尽量按数字发 id：部分 OneBot 实现只认数字类型。
func qqNumber(id qqID) any {
	if n, err := strconv.ParseInt(id.String(), 10, 64); err == nil {
		return n
	}
	return id.String()
}

// ---------------------------------------------------------------------------
// WebSocket 入口
// ---------------------------------------------------------------------------

var qqUpgrader = websocket.Upgrader{
	// OneBot 客户端不是浏览器，不带 Origin；鉴权靠 access token。
	CheckOrigin: func(*http.Request) bool { return true },
}

func qqTokenFromRequest(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		for _, p := range []string{"Bearer ", "Token "} {
			if strings.HasPrefix(h, p) {
				return strings.TrimSpace(h[len(p):])
			}
		}
		return h
	}
	return r.URL.Query().Get("access_token")
}

func qqOneBotWS(c *gin.Context) {
	cfg := loadQQConfig()
	if !cfg.Enabled {
		c.Status(http.StatusNotFound)
		return
	}
	// 没配 token 一律拒绝：这个口子能让外部 QQ 消息驱动 agent，不能裸奔。
	if cfg.Token == "" || subtle.ConstantTimeCompare([]byte(qqTokenFromRequest(c.Request)), []byte(cfg.Token)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "access token 无效"})
		return
	}
	ws, err := qqUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	conn := &qqConn{ws: ws}
	selfID := c.GetHeader("X-Self-ID")
	if selfID != "" {
		qqSetConn(selfID, conn)
	}
	defer func() { qqDropConn(selfID, conn) }()
	log.Printf("qq: OneBot 已连接 self_id=%s", selfID)
	ws.SetReadLimit(4 << 20)
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("qq: OneBot 断开 self_id=%s: %v", selfID, err)
			return
		}
		var ev qqEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		// OneBot action replies have no post_type. Resolve the waiting sender
		// so the outbox can persist the returned message_id.
		var resp struct {
			Status  string          `json:"status"`
			Message string          `json:"message"`
			Wording string          `json:"wording"`
			RetCode int             `json:"retcode"`
			Echo    json.RawMessage `json:"echo"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &resp) == nil && len(resp.Echo) > 0 && resp.Status != "" {
			echo := qqEchoValue(resp.Echo)
			if resp.Status == "failed" || resp.RetCode != 0 {
				errText := strings.TrimSpace(resp.Message)
				if errText == "" {
					errText = strings.TrimSpace(resp.Wording)
				}
				if errText == "" {
					errText = "onebot action failed"
				}
				qqWaitResolve(echo, qqActionResponse{err: errors.New(errText)})
			} else {
				var payload struct {
					MessageID qqID `json:"message_id"`
				}
				_ = json.Unmarshal(resp.Data, &payload)
				qqWaitResolve(echo, qqActionResponse{messageID: payload.MessageID.String()})
			}
			continue
		}
		if ev.PostType == "" {
			continue
		}
		if s := ev.SelfID.String(); s != "" && s != selfID {
			// 握手头里没带 X-Self-ID 的实现：从第一条事件里认领。
			if selfID != "" {
				qqDropConn(selfID, conn)
			}
			selfID = s
			qqSetConn(selfID, conn)
		}
		if ev.PostType == "message" {
			go qqHandleMessage(cfg, ev)
		}
	}
}

// ---------------------------------------------------------------------------
// 消息处理
// ---------------------------------------------------------------------------

const qqHelpText = "我是接在 QQ 上的助手。\n直接发消息和我聊（群里请 @我）。\n/new 开始新对话（清空上下文）\n/help 显示这条说明"

// qqRunTurn 是旧的「适配器里直接跑一轮」。普通消息不再调用它：回复只经发件箱。
// 变量留下来，是为了既有测试还能把旧轮次换成桩，而不会在入口被重新接上。
var qqRunTurn = qqRunAgentTurn

// qqDispatch 只服务适配器本地命令（/new、/help）和「还没有本地账号」的提示。
// 普通消息的业务回复不得走这里。测试可以把它换成记录桩。
var qqDispatch = qqSend

func qqHandleMessage(cfg qqConfig, ev qqEvent) {
	if ev.MessageType != "private" && ev.MessageType != "group" {
		return
	}
	// 机器人自己发的消息在适配器里丢掉，不入库（契约 §3.1）。
	if ev.UserID != "" && ev.UserID == ev.SelfID {
		return
	}
	text, atSelf := qqExtractText(ev)
	if text == "" {
		return
	}
	userID := cfg.UserID
	if userID == "" {
		userID = ownerUserID()
	}
	if userID == "" {
		qqDispatch(ev, "服务端还没有任何账号，请先在网页上注册。")
		return
	}

	// /new 与 /help 是适配器本地命令，不进入判断。除此之外不看白名单、不看 @、
	// 也不直接跑轮次：群里没 @ 的同样入库，交给 gate。
	switch strings.ToLower(strings.Fields(text)[0]) {
	case "/help", "/帮助":
		qqDispatch(ev, qqHelpText)
		return
	case "/new", "/reset", "/新对话":
		if _, err := qqNewConversation(cfg, userID, ev); err != nil {
			qqDispatch(ev, "新建对话失败："+err.Error())
			return
		}
		qqDispatch(ev, "好的，已开始新的对话。")
		return
	}

	addressed := model.SAAddressedNone
	if ev.MessageType == "private" {
		addressed = model.SAAddressedDirect
	} else if atSelf {
		addressed = model.SAAddressedMention
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		log.Printf("qq: 入站序列化失败 %s: %v", ev.chatKey(), err)
		return
	}
	msgID := ev.MessageID.String()
	if msgID == "" {
		msgID = fmt.Sprintf("raw:%x", raw)
	}
	// 看不到可靠的「对方是机器人」标记时按 human 填。自己的消息已在上面丢弃。
	_, err = saIngest(context.Background(), saTriggerIn{
		UserID:       userID,
		Source:       model.SASourceQQ,
		SourceMsgID:  msgID,
		ReplyToMsgID: qqReplyToID(ev),
		ThreadKey:    "qq:" + ev.chatKey(),
		ThreadLabel:  qqConversationTitle(ev),
		ActorID:      ev.UserID.String(),
		ActorName:    ev.senderName(),
		ActorKind:    model.SAActorHuman,
		Addressed:    addressed,
		Text:         text,
		Raw:          string(raw),
		Inline:       false,
	})
	if err != nil {
		log.Printf("qq: 入站失败 %s: %v", ev.chatKey(), err)
	}
}

// qqModelFor 定本轮用哪个模型：QQ_BOT_MODEL > 会话已选的 > 该用户最近用过的。
func qqModelFor(cfg qqConfig, userID string) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	var last model.Conversation
	if err := model.DB.Where("user_id = ? AND model <> ''", userID).Order("updated_at desc").First(&last).Error; err == nil {
		return last.Model
	}
	return ""
}

func qqConversationTitle(ev qqEvent) string {
	if ev.MessageType == "group" {
		return "QQ 群 " + ev.GroupID.String()
	}
	return "QQ · " + ev.senderName()
}

func qqNewConversation(cfg qqConfig, userID string, ev qqEvent) (*model.Conversation, error) {
	if model.DB == nil {
		return nil, errors.New("数据库未初始化")
	}
	conv := model.Conversation{UserID: userID, Title: qqConversationTitle(ev), Model: qqModelFor(cfg, userID), Kind: cfg.Kind}
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&conv).Error; err != nil {
			return err
		}
		var b model.QQChatBinding
		res := tx.Where("chat_key = ?", ev.chatKey()).Limit(1).Find(&b)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			if err := tx.Create(&model.QQChatBinding{ChatKey: ev.chatKey(), UserID: userID, ConversationID: conv.Id}).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&b).Updates(map[string]any{"conversation_id": conv.Id, "user_id": userID}).Error; err != nil {
			return err
		}
		// 入口只在 Thread 还不存在时采纳旧 binding。Thread 一旦建好，前台会话以
		// sa_thread.conversation_id 为准，/new 必须改这一列，Thread 本身不变。
		if !tx.Migrator().HasTable(&model.SAThread{}) {
			return nil
		}
		return tx.Model(&model.SAThread{}).
			Where("thread_key = ? AND user_id = ?", "qq:"+ev.chatKey(), userID).
			Update("conversation_id", conv.Id).Error
	})
	if err != nil {
		return nil, err
	}
	return &conv, nil
}

// qqConversationFor 取这个 QQ 聊天绑定的会话；没有、或会话已被删掉/换了账号，就新建一个。
// 普通消息不再走这里：Thread 和前台会话由 saIngest 找/建，/new 走 qqNewConversation。
func qqConversationFor(cfg qqConfig, userID string, ev qqEvent) (*model.Conversation, error) {
	if model.DB == nil {
		return nil, errors.New("数据库未初始化")
	}
	var b model.QQChatBinding
	if res := model.DB.Where("chat_key = ?", ev.chatKey()).Limit(1).Find(&b); res.Error == nil && res.RowsAffected > 0 && b.UserID == userID {
		var conv model.Conversation
		if err := model.DB.Where("id = ? AND user_id = ?", b.ConversationID, userID).First(&conv).Error; err == nil {
			if strings.TrimSpace(conv.Model) == "" {
				if m := qqModelFor(cfg, userID); m != "" {
					conv.Model = m
					model.DB.Model(&conv).Update("model", m)
				}
			}
			return &conv, nil
		}
	}
	return qqNewConversation(cfg, userID, ev)
}

// qqRunAgentTurn 与网页 /api/chat 走同一条路：先落 user 消息，再按 AGENT_TOOLS 决定
// 走 agent 循环还是直连模型；把增量文本攒起来当回复。
func qqRunAgentTurn(ctx context.Context, conv *model.Conversation, userID, content string) (string, error) {
	if registry == nil {
		return "", errors.New("模型注册表未就绪")
	}
	modelID := strings.TrimSpace(conv.Model)
	if modelID == "" {
		return "", errors.New("没有可用的模型：请设置 QQ_BOT_MODEL，或先在网页上用某个模型聊一次")
	}
	ep, ok := registry.FindByModel(modelID)
	if !ok {
		return "", fmt.Errorf("找不到模型 %s 的端点", modelID)
	}
	key := resolveKey(userID, ep)
	saveChatMessage(conv.Id, "user", content)
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ?", conv.Id).Order("id asc").Find(&rows).Error; err != nil {
		return "", err
	}
	history := rebuildLLMHistory(rows)

	if !shouldUseAgent(true) {
		stream, err := registry.Stream(ctx, modelID, key, directChatRequest(history, chatParams{}))
		if err != nil {
			return "", err
		}
		var acc strings.Builder
		for ev := range stream {
			switch ev.Type {
			case llm.EventDelta:
				acc.WriteString(ev.Content)
			case llm.EventError:
				saveChatMessage(conv.Id, "assistant", acc.String())
				return acc.String(), ev.Err
			}
		}
		saveChatMessage(conv.Id, "assistant", acc.String())
		touchConversation(conv.Id)
		return acc.String(), nil
	}

	var (
		acc       strings.Builder
		question  string
		runErrMsg string
	)
	runAgentChat(turnInput{
		ctx:     ctx,
		conv:    conv,
		userID:  userID,
		modelID: modelID,
		key:     key,
		content: content,
		history: history,
		kind:    normalizeKind(conv.Kind),
		sendEvent: func(name string, v any) {
			m, _ := v.(gin.H)
			switch name {
			case "delta":
				s, _ := m["content"].(string)
				acc.WriteString(s)
			case "need_input":
				question, _ = m["question"].(string)
			case "error":
				runErrMsg, _ = m["error"].(string)
			}
		},
	})
	reply := strings.TrimSpace(acc.String())
	if q := strings.TrimSpace(question); q != "" && !strings.Contains(reply, q) {
		reply = strings.TrimSpace(reply + "\n\n" + q)
	}
	if runErrMsg != "" {
		return reply, errors.New(runErrMsg)
	}
	return reply, nil
}
