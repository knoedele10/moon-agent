package model

import (
	"fmt"
	"os"
	"strings"
)

// SelectDriver 决定本次进程用哪一种数据库。
//
// 显式 DB_DRIVER=sqlite / mysql 优先。没写 DB_DRIVER 时：只要环境里出现了
// DB_HOST / DB_USER / DB_NAME / DB_PASSWORD 中的任意一个，就继续走 MySQL，
// 避免老安装在升级后突然连上一份空的 SQLite。干净环境（这些变量都空）默认 SQLite。
func SelectDriver() (string, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DB_DRIVER"))) {
	case "sqlite", "sqlite3":
		return "sqlite", nil
	case "mysql":
		return "mysql", nil
	case "":
		if mysqlEnvPresent() {
			return "mysql", nil
		}
		return "sqlite", nil
	default:
		return "", fmt.Errorf("DB_DRIVER=%q 只支持 sqlite 或 mysql", os.Getenv("DB_DRIVER"))
	}
}

// Driver 返回当前选择；非法值时返回空串，InitDB 会报错。
func Driver() string {
	d, err := SelectDriver()
	if err != nil {
		return ""
	}
	return d
}

func mysqlEnvPresent() bool {
	for _, key := range []string{"DB_HOST", "DB_USER", "DB_NAME", "DB_PASSWORD"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}
