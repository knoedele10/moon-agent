package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"blog-my/internal/agent/skill"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// ---------- 目录导入 ----------

// validateSkillImportDir 校验用户给的技能库目录。
//
// 与工作区校验同一套标准：必须绝对路径、必须存在、必须是目录。
// 额外拒绝**盘符根**——那等于让服务端把整块盘递归扫一遍。
func validateSkillImportDir(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("请填写技能库目录")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("请填绝对路径，例如 C:\\Users\\you\\.agents\\skills")
	}
	clean := filepath.Clean(p)
	if vol := filepath.VolumeName(clean); vol != "" && clean == vol+string(filepath.Separator) {
		return "", fmt.Errorf("不要用盘符根目录，请指向技能库文件夹")
	}
	fi, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("目录不存在：%s", clean)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s 不是目录", clean)
	}
	return clean, nil
}

// importSkills 把一个目录下的 SKILL.md 批量导入当前用户（幂等，可重复跑）。
//
// 默认 enabled=true：导入的目的就是“能用”，导完还要用户逐个去开一遍不合理。
func importSkills(c *gin.Context) {
	userID := currentUserID(c)
	var body struct {
		Dir           string `json:"dir"`
		Enabled       *bool  `json:"enabled"`
		IncludeHidden bool   `json:"include_hidden"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	dir, err := validateSkillImportDir(body.Dir)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// enabled 三态：不传 = 保留每条已有技能自己的开关（新建的默认启用），
	// 传 true/false = 一律设置。这样重跑导入不会把用户手工关掉的技能又打开。
	stats, err := model.ImportSkillDir(userID, dir, body.Enabled, skill.ScanOptions{IncludeHidden: body.IncludeHidden})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入失败：" + err.Error()})
		return
	}
	// enabled 报“新建技能会是什么状态”；preserve_existing 说明已有技能的开关有没有被动过。
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "root": stats.Root, "files": stats.Files, "imported": stats.Imported,
		"created": stats.Created, "updated": stats.Updated,
		"enabled": enabled, "preserve_existing": body.Enabled == nil,
		"duplicates": stats.Duplicates, "errors": stats.Errors,
	})
}
