package web

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const executionModeContextKey = "executionMode"

// An explicit composer choice takes precedence over an assistant preset.
func setExecutionMode(c *gin.Context, value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return true
	}
	if value != ModeAuto && value != ModePlan && value != ModeConfirm {
		c.JSON(http.StatusBadRequest, gin.H{"error": "execution_mode 只能是 plan / confirm / auto"})
		return false
	}
	c.Set(executionModeContextKey, value)
	return true
}

func agentLimitRecoveryRounds(mode string) int {
	if normalizeMode(mode) != ModeAuto {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_LIMIT_RECOVERY_ROUNDS")))
	if err != nil || n < 0 {
		return 2
	}
	if n > 8 {
		return 8
	}
	return n
}
