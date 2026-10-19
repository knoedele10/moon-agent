package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// 提示词库（prompt）管理：用户攒下来、随时复制的常用提示词片段。
//
// 与助手分开：助手是「人格 + 配置」，提示词只是纯文本，没有行为语义。
// 本文件只提供 handler，路由由主 agent 在 web.go 里接：
//
//	GET    /api/prompts        -> listPrompts
//	POST   /api/prompts        -> createPrompt
//	PATCH  /api/prompts/:id    -> updatePrompt
//	DELETE /api/prompts/:id    -> deletePrompt
const promptTitleMaxRunes = 60

// promptItem 是返回给前端的形状（content 可能很长，但提示词库需要预览，所以整条返回）。
type promptItem struct {
	ID        uint   `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func promptItemFrom(p model.AgentPrompt) promptItem {
	return promptItem{
		ID: p.Id, Title: p.Title, Content: p.Content,
		CreatedAt: p.CreatedAt.Format(time.RFC3339),
	}
}

// normalizePromptTitle 去空格并按 rune 截到 60；空 → 报错。
func normalizePromptTitle(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", errors.New("提示词标题不能为空")
	}
	return truncateRunes(t, promptTitleMaxRunes), nil
}

// applyPromptPatch 把三态补丁应用到一条提示词上：title / content 为 nil 表示「不改」。
// 和助手一样，先在副本上算完再写回，保证校验失败时原记录一点不动。
func applyPromptPatch(p *model.AgentPrompt, title, content *string) error {
	next := *p
	if title != nil {
		t, err := normalizePromptTitle(*title)
		if err != nil {
			return err
		}
		next.Title = t
	}
	if content != nil {
		c := strings.TrimSpace(*content)
		if c == "" {
			return errors.New("提示词内容不能为空")
		}
		next.Content = c
	}
	*p = next
	return nil
}

func listPrompts(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var rows []model.AgentPrompt
	// 最近改过的排前面：提示词是「刚刚用过、还想再用」的场景最多。
	if err := model.DB.Where("user_id = ?", userID).Order("updated_at desc, id desc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取提示词失败"})
		return
	}
	items := make([]promptItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, promptItemFrom(r))
	}
	c.JSON(http.StatusOK, gin.H{"prompts": items})
}

func createPrompt(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	p := model.AgentPrompt{UserID: userID}
	if err := applyPromptPatch(&p, &body.Title, &body.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := model.DB.Create(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存提示词失败"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": p.Id})
}

func updatePrompt(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var body struct {
		Title   *string `json:"title"`
		Content *string `json:"content"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	var p model.AgentPrompt
	// 不属于当前用户 → 404，语义与助手一致（不泄露存在性）。
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&p).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "提示词不存在"})
		return
	}
	if err := applyPromptPatch(&p, body.Title, body.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := model.DB.Save(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存提示词失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func deletePrompt(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	res := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).Delete(&model.AgentPrompt{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除提示词失败"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "提示词不存在"})
		return
	}
	c.Status(http.StatusNoContent)
}
