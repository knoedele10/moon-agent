package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 自定义助手（assistant）管理：一个助手 = 一个可切换的人格 + 它自己的配置覆盖。
//
// 与用户全局设置（agent_setting）的关系：助手里的灵魂/档位/工作区都是**覆盖项**，
// 空串表示「继承全局」。这样用户改全局设置时，没覆盖过的助手会自动跟着变。
//
// 本文件只提供 handler，路由由主 agent 在 web.go 里接：
//
//	GET    /api/assistants        -> listAssistants
//	POST   /api/assistants        -> createAssistant
//	PATCH  /api/assistants/:id    -> updateAssistant
//	DELETE /api/assistants/:id    -> deleteAssistant
const (
	// assistantNameMaxRunes 是助手名的存储上限。列宽给到 128，但界面上留 64 字更像「名字」，
	// 也让列表一行放得下。
	assistantNameMaxRunes   = 64
	assistantDescMaxRunes   = 512
	assistantModelMaxRunes  = 128
	assistantFSRootMaxRunes = 1024
)

// assistantPatch 是一次创建/更新的三态输入：字段为 nil 表示「这次不改」，
// 非 nil 表示「改成这个值」。用指针是为了把「不传」和「传空串」区分开——
// 传空串是合法语义（清掉覆盖、回到继承全局），不传才是保持原样。
type assistantPatch struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	Model           *string `json:"model"`
	SoulPersonality *string `json:"soul_personality"`
	SoulVoice       *string `json:"soul_voice"`
	SoulWork        *string `json:"soul_work"`
	Mode            *string `json:"mode"`
	FSRoot          *string `json:"fs_root"`
	Enabled         *bool   `json:"enabled"`
}

// truncateRunes 按 rune（而不是字节）截断，避免中文被截成半个字。
// 不加省略号：这里截断是为了不超数据库列宽，超长在 MySQL 严格模式下会直接报 500。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// normalizeAssistantName 去空格并按 rune 截到 64；空 → 报错。
func normalizeAssistantName(raw string) (string, error) {
	n := strings.TrimSpace(raw)
	if n == "" {
		return "", errors.New("助手名字不能为空")
	}
	return truncateRunes(n, assistantNameMaxRunes), nil
}

// normalizeAssistantMode 校验并规范化档位。
// 返回空串表示「继承全局档位」（合法）；非空只接受 plan / confirm / auto。
func normalizeAssistantMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "":
		return "", nil
	case ModePlan, ModeConfirm, ModeAuto:
		return m, nil
	default:
		return "", errors.New("档位只能是 plan / confirm / auto")
	}
}

// applyAssistantPatch 把三态补丁应用到一条助手上。
//
// 先把结果算在副本上、全部校验通过才写回，保证「有一条校验失败时整条记录不变」——
// 否则 PATCH {name:"", mode:"bogus"} 可能清空名字、却因为 mode 报错而让用户以为没保存。
func applyAssistantPatch(a *model.AgentAssistant, p assistantPatch) error {
	next := *a
	if p.Name != nil {
		n, err := normalizeAssistantName(*p.Name)
		if err != nil {
			return err
		}
		next.Name = n
	}
	if p.Description != nil {
		next.Description = truncateRunes(strings.TrimSpace(*p.Description), assistantDescMaxRunes)
	}
	if p.Model != nil {
		next.Model = truncateRunes(strings.TrimSpace(*p.Model), assistantModelMaxRunes)
	}
	if p.SoulPersonality != nil {
		next.SoulPersonality = strings.TrimSpace(*p.SoulPersonality)
	}
	if p.SoulVoice != nil {
		next.SoulVoice = strings.TrimSpace(*p.SoulVoice)
	}
	if p.SoulWork != nil {
		next.SoulWork = strings.TrimSpace(*p.SoulWork)
	}
	if p.Mode != nil {
		m, err := normalizeAssistantMode(*p.Mode)
		if err != nil {
			return err
		}
		next.Mode = m
	}
	if p.FSRoot != nil {
		next.FSRoot = truncateRunes(strings.TrimSpace(*p.FSRoot), assistantFSRootMaxRunes)
	}
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	// 名字是唯一必填项：创建时没传、或更新时被清空，都在这里统一挡下。
	if strings.TrimSpace(next.Name) == "" {
		return errors.New("助手名字不能为空")
	}
	*a = next
	return nil
}

// assistantItem 是列表/创建返回给前端的形状。
type assistantItem struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Model           string `json:"model"`
	SoulPersonality string `json:"soul_personality"`
	SoulGrowth      string `json:"soul_growth"`
	SoulVoice       string `json:"soul_voice"`
	SoulWork        string `json:"soul_work"`
	Mode            string `json:"mode"`
	FSRoot          string `json:"fs_root"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at"`
}

func assistantItemFrom(a model.AgentAssistant) assistantItem {
	return assistantItem{
		ID: a.Id, Name: a.Name, Description: a.Description, Model: a.Model,
		SoulPersonality: a.SoulPersonality, SoulGrowth: soulGrowthSummary(a.SoulGrowth), SoulVoice: a.SoulVoice, SoulWork: a.SoulWork, Mode: a.Mode, FSRoot: a.FSRoot,
		Enabled: a.Enabled, CreatedAt: a.CreatedAt.Format(time.RFC3339),
	}
}

func listAssistants(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var rows []model.AgentAssistant
	if err := model.DB.Where("user_id = ?", userID).Order("id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取助手失败"})
		return
	}
	items := make([]assistantItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, assistantItemFrom(r))
	}
	c.JSON(http.StatusOK, gin.H{"assistants": items})
}

func createAssistant(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var body assistantPatch
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	// 没传 enabled 时默认启用。
	a := model.AgentAssistant{UserID: userID, Enabled: true}
	if err := applyAssistantPatch(&a, body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 同名视为冲突：明确拒绝，不静默覆盖（用户可能只是想放弃编辑，覆盖会丢数据）。
	taken, err := assistantNameTaken(userID, a.Name, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存助手失败"})
		return
	}
	if taken {
		c.JSON(http.StatusBadRequest, gin.H{"error": "已存在同名助手"})
		return
	}

	if err := model.DB.Create(&a).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存助手失败"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": a.Id})
}

func updateAssistant(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var body assistantPatch
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	var a model.AgentAssistant
	// 不属于当前用户时返回 404（而不是 403），避免泄露「这个 id 是否存在」。
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&a).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "助手不存在"})
		return
	}
	if err := applyAssistantPatch(&a, body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Name != nil {
		taken, err := assistantNameTaken(userID, a.Name, a.Id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存助手失败"})
			return
		}
		if taken {
			c.JSON(http.StatusBadRequest, gin.H{"error": "已存在同名助手"})
			return
		}
	}
	if err := model.DB.Save(&a).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存助手失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func deleteAssistant(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	res := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).Delete(&model.AgentAssistant{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除助手失败"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "助手不存在"})
		return
	}
	c.Status(http.StatusNoContent)
}

// assistantNameTaken 判断同一用户下是否已有同名助手（excludeID 用于编辑时排除自己）。
func assistantNameTaken(userID, name string, excludeID uint) (bool, error) {
	q := model.DB.Model(&model.AgentAssistant{}).Where("user_id = ? AND name = ?", userID, name)
	if excludeID != 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}
