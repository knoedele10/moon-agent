package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
)

// maxWorkspacePathLen 限制工作区路径长度，避免把超长字符串写进数据库。
const maxWorkspacePathLen = 512

// validateWorkspace 校验并规范化用户填的工作区路径。
//
// 规则（每条都对应一个真实会踩的坑）：
//  1. 必须是绝对路径——相对路径会跟着进程的工作目录漂移，用户以为指的是别处；
//  2. 不能是盘符根（`E:\` 这种）——那等于把整块盘交给模型删改；
//  3. 目录不存在时自动创建——用户填一个还没建的目录是最常见的情况；
//  4. 最终必须真的是目录——指向文件的路径会让所有文件工具直接报错。
func validateWorkspace(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", &workspaceError{"工作区路径不能为空"}
	}
	if len(p) > maxWorkspacePathLen {
		return "", &workspaceError{"工作区路径过长（最多 512 字符）"}
	}
	if !filepath.IsAbs(p) {
		return "", &workspaceError{"请填绝对路径，例如 D:\\my-workspace"}
	}
	clean := filepath.Clean(p)
	// 盘符根（E:\）或 UNC 根：拒绝，避免一句话把整块盘交出去。
	if clean == filepath.VolumeName(clean)+string(filepath.Separator) {
		return "", &workspaceError{"不能把盘符根目录当工作区，请指定一个子目录"}
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return "", &workspaceError{"无法创建该目录：" + err.Error()}
	}
	fi, err := os.Stat(clean)
	if err != nil {
		return "", &workspaceError{"无法访问该目录：" + err.Error()}
	}
	if !fi.IsDir() {
		return "", &workspaceError{"该路径不是目录"}
	}
	return clean, nil
}

// workspaceError 让校验失败与内部错误可区分（handler 里统一转 400）。
type workspaceError struct{ msg string }

func (e *workspaceError) Error() string { return e.msg }

// agentSettingsResponse 是 GET /api/agent/settings 的响应。
type agentSettingsResponse struct {
	Workspace    string   `json:"workspace"`
	Workspaces   []string `json:"workspaces"`
	Configured   bool     `json:"configured"`
	Source       string   `json:"source"` // user | env | none
	ToolsEnabled bool     `json:"tools_enabled"`
	// Mode 是当前生效的自主档位：plan / confirm / auto。
	Mode string `json:"mode"`
	// 灵魂：用户亲手设的人格 / 说话方式 / 做事方式（空串表示没设）。
	SoulPersonality string `json:"soul_personality"`
	SoulGrowth      string `json:"soul_growth"`
	SoulVoice       string `json:"soul_voice"`
	SoulWork        string `json:"soul_work"`
	// CodeRoot / LearnRoot 是两个模式的专属默认工作区（空串 = 没设，回落到 Workspace）。
	CodeRoot  string `json:"code_root"`
	LearnRoot string `json:"learn_root"`
}

// getAgentSettings 返回当前用户生效的工作区，以及它来自哪里。
func getAgentSettings(c *gin.Context) {
	userID := currentUserID(c)

	configured := false
	if model.DB != nil && userID != "" {
		var s model.AgentSetting
		if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err == nil {
			configured = strings.TrimSpace(s.FSRoot) != "" || strings.TrimSpace(s.WorkspaceRoots) != ""
		}
	}

	source := "none"
	switch {
	case configured:
		source = "user"
	case strings.TrimSpace(agentFSRoot()) != "":
		source = "env"
	}

	soulPersonality, soulVoice, soulWork := "", "", ""
	codeRoot, learnRoot := "", ""
	if model.DB != nil && userID != "" {
		var s model.AgentSetting
		if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err == nil {
			soulPersonality, soulVoice, soulWork = soulFromSetting(s)
			codeRoot = strings.TrimSpace(s.CodeRoot)
			learnRoot = strings.TrimSpace(s.LearnRoot)
		}
	}

	c.JSON(http.StatusOK, agentSettingsResponse{
		Workspace:       agentFSRootFor(userID),
		Configured:      configured,
		Source:          source,
		ToolsEnabled:    agentToolsEnabled(),
		Mode:            agentModeFor(userID),
		SoulPersonality: soulPersonality,
		SoulGrowth: func() string {
			if model.DB == nil || userID == "" {
				return ""
			}
			var s model.AgentSetting
			if model.DB.Where("user_id = ?", userID).First(&s).Error == nil {
				return soulGrowthSummary(s.SoulGrowth)
			}
			return ""
		}(),
		SoulVoice:  soulVoice,
		SoulWork:   soulWork,
		CodeRoot:   codeRoot,
		LearnRoot:  learnRoot,
		Workspaces: agentFSRootsFor(userID),
	})
}

// putAgentSettings 保存当前用户的工作区。
func putAgentSettings(c *gin.Context) {
	userID := currentUserID(c)

	var body struct {
		Workspace  *string   `json:"workspace"`
		Workspaces *[]string `json:"workspaces"`
		Mode       *string   `json:"mode"`
		// 灵魂字段：传了就更新（允许传空串来清空），不传则保持原值。
		SoulPersonality *string `json:"soul_personality"`
		SoulGrowth      *string `json:"soul_growth"`
		SoulVoice       *string `json:"soul_voice"`
		SoulWork        *string `json:"soul_work"`
		// 模式专属工作区：传了就更新（允许传空串清空），不传保持原值。
		CodeRoot  *string `json:"code_root"`
		LearnRoot *string `json:"learn_root"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	// 灵魂字段也能单独保存：设置页的「保存灵魂」只发 soul_voice/soul_work，
	// 旧判断要求必须带 workspace 或 mode，会让那次保存直接 400、灵魂根本存不进去。
	if body.Workspace == nil && body.Workspaces == nil && body.Mode == nil && body.SoulGrowth == nil && body.SoulPersonality == nil && body.SoulVoice == nil && body.SoulWork == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少要给出 workspace / mode / soul_personality / soul_voice / soul_work 中的一个"})
		return
	}
	var clean string
	var cleanWorkspaces []string
	var workspacesJSON string
	if body.Workspaces != nil {
		if len(*body.Workspaces) > 20 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "最多同时启用 20 个工作目录"})
			return
		}
		for _, raw := range *body.Workspaces {
			v, err := validateWorkspace(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			cleanWorkspaces = append(cleanWorkspaces, v)
		}
		cleanWorkspaces = cleanWorkspaceRoots(cleanWorkspaces)
		encoded, _ := json.Marshal(cleanWorkspaces)
		workspacesJSON = string(encoded)
		if len(cleanWorkspaces) > 0 {
			clean = cleanWorkspaces[0]
		}
	}
	if body.Workspace != nil {
		v, err := validateWorkspace(*body.Workspace)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		clean = v
		if body.Workspaces == nil {
			cleanWorkspaces = []string{clean}
			encoded, _ := json.Marshal(cleanWorkspaces)
			workspacesJSON = string(encoded)
		}
	}
	var newMode string
	// 模式专属工作区：允许传空串（= 清空，回落到通用工作区）；非空则要过同一套校验
	// （必须是绝对路径、不能是盘符根、目录会自动创建）。
	cleanRoots := map[string]string{}
	for name, ptr := range map[string]*string{"代码目录": body.CodeRoot, "笔记目录": body.LearnRoot} {
		if ptr == nil {
			continue
		}
		v := strings.TrimSpace(*ptr)
		if v == "" {
			cleanRoots[name] = ""
			continue
		}
		cleaned, err := validateWorkspace(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": name + "：" + err.Error()})
			return
		}
		cleanRoots[name] = cleaned
	}
	if body.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*body.Mode))
		if m != ModePlan && m != ModeConfirm && m != ModeAuto {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mode 只能是 plan / confirm / auto"})
			return
		}
		newMode = m
	}
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}

	var s model.AgentSetting
	if err := model.DB.Where("user_id = ?", userID).First(&s).Error; err != nil {
		s = model.AgentSetting{UserID: userID, FSRoot: clean, Mode: newMode,
			CodeRoot: cleanRoots["代码目录"], LearnRoot: cleanRoots["笔记目录"]}
		if body.Workspaces != nil || body.Workspace != nil {
			s.WorkspaceRoots = workspacesJSON
		}
		if body.SoulGrowth != nil {
			s.SoulGrowth = strings.TrimSpace(*body.SoulGrowth)
		}
		if body.SoulPersonality != nil {
			s.SoulPersonality = strings.TrimSpace(*body.SoulPersonality)
		}
		if body.SoulVoice != nil {
			s.SoulVoice = strings.TrimSpace(*body.SoulVoice)
		}
		if body.SoulWork != nil {
			s.SoulWork = strings.TrimSpace(*body.SoulWork)
		}
		if err := model.DB.Create(&s).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存工作区失败"})
			return
		}
	} else {
		if body.Workspaces != nil || body.Workspace != nil {
			s.FSRoot = clean
			s.WorkspaceRoots = workspacesJSON
		}
		if newMode != "" {
			s.Mode = newMode
		}
		// 传了才改（map 里没键 = 没传），传空串 = 清空。
		if v, ok := cleanRoots["代码目录"]; ok {
			s.CodeRoot = v
		}
		if v, ok := cleanRoots["笔记目录"]; ok {
			s.LearnRoot = v
		}
		if body.SoulGrowth != nil {
			s.SoulGrowth = strings.TrimSpace(*body.SoulGrowth)
		}
		if body.SoulPersonality != nil {
			s.SoulPersonality = strings.TrimSpace(*body.SoulPersonality)
		}
		if body.SoulVoice != nil {
			s.SoulVoice = strings.TrimSpace(*body.SoulVoice)
		}
		if body.SoulWork != nil {
			s.SoulWork = strings.TrimSpace(*body.SoulWork)
		}
		if err := model.DB.Save(&s).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存工作区失败"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "workspace": s.FSRoot, "workspaces": agentFSRootsFor(userID), "mode": normalizeMode(s.Mode)})
}
