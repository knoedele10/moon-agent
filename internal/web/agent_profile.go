package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"blog-my/internal/model"
	"blog-my/internal/rag"
	"gorm.io/gorm"
)

// 用户画像：把零散的事实定期汇总成一段「这个人是谁、在意什么、最近在忙什么」。
//
// 为什么需要它：记忆检索每轮只注入 top-k 条零散事实，模型看得到「端口是 8300」，
// 却看不到「你是谁、你的风格偏好、你最近的目标」——所以它只能答具体问题，做不到
// 「懂你想要什么」。画像短且稳定，每轮都带上；零散记忆再按相关度补充。
//
// 存储上复用了 kb_document（source="profile"），一个用户只保留一条：
// 画像不是事实，不需要留历史版本。

const (
	// profileEveryTurnsDefault 每多少轮刷新一次画像。太勤会白烧模型调用，太懒则画像跟不上。
	// 可用 AGENT_PROFILE_EVERY_TURNS 覆盖（测试时把它调到 3 就能快速验证整条链路）。
	profileEveryTurnsDefault = 20
	// profileMaxRunes 画像正文上限：它每轮都注入，必须短。
	profileMaxRunes = 1200
	// profileSourceRunes 汇总时喂给模型的记忆条数与单条长度。
	profileSourceLimit = 40
	profileSourceRunes = 200
)

// profileEveryTurns 返回生效的画像刷新间隔：环境变量给了合法正整数就用它，否则用默认值。
// 做成函数而不是常量，是为了让端到端测试能把阈值临时调低，不必改代码重编译。
func profileEveryTurns() int {
	v := strings.TrimSpace(os.Getenv("AGENT_PROFILE_EVERY_TURNS"))
	if v == "" {
		return profileEveryTurnsDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return profileEveryTurnsDefault
	}
	return n
}

func profileRefreshDue(turns, threshold int) bool {
	return threshold > 0 && turns >= threshold
}

const profileSystemPrompt = `你在维护一份「用户画像」，用来让助手长期更懂这个人。

输入是系统从历史对话里抽取的一批事实（可能有重复、过时或噪声）。
请把它们归纳成一份**结构化、简洁**的画像，只输出画像正文，不要解释过程。

要求：
1. 分四节，没内容的节直接省略：
   ## 身份与背景（称呼、职业、所在领域、语言习惯）
   ## 稳定偏好（沟通风格、工作方式、明确的喜恶）
   ## 当前项目与目标（在做什么、关键约束、时间安排）
   ## 需要留意（易错点、明确的禁忌、待确认的事）
2. 只写有事实支撑的内容，**不要推断、不要编造**。
3. 同一件事有多条冲突的，以**更新的那条**为准，并直接写结论，不要写“之前…后来…”。
4. 用第三人称“用户”指代这个人。
5. 全文控制在 600 字以内。`

var (
	profileMu      sync.Mutex
	profileRunning = map[string]bool{}
)

// kbProfileBlock 读取画像并拼成可注入的提示词块；没有画像时返回空串。
func kbProfileBlock(userID string) string {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return ""
	}
	var doc model.KBDocument
	if err := model.DB.Where("user_id = ? AND source = ?", userID, "profile").
		Order("id desc").First(&doc).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("[profile] 读取画像失败：%v", err)
		}
		return ""
	}
	text := strings.TrimSpace(string(doc.Content))
	if text == "" {
		return ""
	}
	return formatProfileBlock(text)
}

func formatProfileBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "\n【关于这位用户的长期画像】（系统定期汇总，可能不完整；与用户当下的话冲突时以当下的话为准）\n" + text + "\n"
}

// noteTurnAndMaybeProfile 记一轮；攒够阈值就在后台刷新画像（不阻塞对话）。
func noteTurnAndMaybeProfile(userID, modelID, key string) {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return
	}
	// 计数写入数据库而非进程内 map：重启后保留进度；成功刷新后才清零，
	// 模型/数据库失败时，下轮仍会重试。
	n, err := incrementProfileTurns(userID)
	if err != nil {
		log.Printf("[profile] 更新画像轮数失败：%v", err)
		return
	}
	if !profileRefreshDue(n, profileEveryTurns()) {
		return
	}
	profileMu.Lock()
	if profileRunning[userID] {
		profileMu.Unlock()
		return
	}
	profileRunning[userID] = true
	profileMu.Unlock()
	go func() {
		defer func() {
			profileMu.Lock()
			delete(profileRunning, userID)
			profileMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[profile] 刷新画像时发生意外（不影响对话）：%v", r)
			}
		}()
		if err := regenerateProfile(context.Background(), userID, modelID, key); err != nil {
			log.Printf("[profile] 刷新画像失败（不影响对话）：%v", err)
			return
		}
		if err := model.DB.Model(&model.AgentSetting{}).Where("user_id = ?", userID).
			Update("profile_turns", gorm.Expr("GREATEST(profile_turns - ?, 0)", profileEveryTurns())).Error; err != nil {
			log.Printf("[profile] 清零画像轮数失败：%v", err)
		}
		log.Printf("[profile] 已刷新用户画像")
	}()
}

func incrementProfileTurns(userID string) (int, error) {
	if model.DB == nil || strings.TrimSpace(userID) == "" {
		return 0, errors.New("画像计数数据库或用户 ID 为空")
	}
	if err := model.DB.Exec("INSERT INTO agent_setting (user_id, profile_turns) VALUES (?, 1) "+
		"ON DUPLICATE KEY UPDATE profile_turns = profile_turns + 1", userID).Error; err != nil {
		return 0, err
	}
	var setting model.AgentSetting
	if err := model.DB.Select("profile_turns").Where("user_id = ?", userID).First(&setting).Error; err != nil {
		return 0, err
	}
	return setting.ProfileTurns, nil
}

// regenerateProfile 用一次模型调用把最近的记忆汇总成画像。
func regenerateProfile(ctx context.Context, userID, modelID, key string) error {
	return regenerateProfileWithCaller(ctx, userID, memoryLLMCaller(modelID, key))
}

func regenerateProfileWithCaller(ctx context.Context, userID string, call rag.LLMCaller) error {
	var rows []model.KBDocument
	if err := model.DB.Where("user_id = ? AND source = ?", userID, "memory").
		Order("id desc").Limit(profileSourceLimit).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil // 还没有任何记忆，不必生成空画像
	}

	var b strings.Builder
	for _, r := range rows {
		b.WriteString("- ")
		b.WriteString(clipRunes(string(r.Content), profileSourceRunes))
		b.WriteString("\n")
	}

	out, err := call(ctx, profileSystemPrompt, b.String())
	if err != nil {
		return err
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return nil
	}
	// Go 和 MySQL 驱动均以 UTF-8 传输；拒绝非法 UTF-8 或替换字符，避免把乱码
	// 覆盖掉一份原本可用的画像。失败时保留旧画像，等待下一次刷新重试。
	if err := validateProfileText(text); err != nil {
		return err
	}
	if len([]rune(text)) > profileMaxRunes {
		text = string([]rune(text)[:profileMaxRunes])
	}

	doc := model.KBDocument{
		UserID: userID,
		Kind:   "text",
		Title:  "用户画像",
		Source: "profile",
		Mime:   "text/plain; charset=utf-8",
		Bytes:  int64(len(text)),
		// sha256 是全局唯一索引：留空的话整个库里只有第一篇画像能写进去，
		// 之后所有用户的画像刷新都会撞唯一键而静默失败（实测踩到过）。
		// 用「用户 + 正文」做指纹：既满足唯一索引，又不会让画像相同的两个用户互相顶掉。
		Sha256:  profileFingerprint(userID, text),
		Content: []byte(text),
	}
	// 替换放在事务里：新记录创建失败时不会丢掉旧画像。
	return model.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND source = ?", userID, "profile").Delete(&model.KBDocument{}).Error; err != nil {
			return err
		}
		return tx.Create(&doc).Error
	})
}

func validateProfileText(text string) error {
	if !utf8.ValidString(text) || strings.ContainsRune(text, utf8.RuneError) {
		return errors.New("画像模型输出包含无效 UTF-8/替换字符，已拒绝保存")
	}
	return nil
}

// profileFingerprint 生成画像文档的内容指纹（满足 kb_document.sha256 的唯一索引）。
func profileFingerprint(userID, text string) string {
	sum := sha256.Sum256([]byte("profile:" + userID + ":" + text))
	return hex.EncodeToString(sum[:])
}
