// Package app owns application startup, HTTP lifecycle and shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"blog-my/internal/llm"
	"blog-my/internal/model"
	"blog-my/internal/web"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func Run(ctx context.Context) error {
	if err := godotenv.Load(); err != nil {
		log.Printf("未加载 .env，将使用系统环境变量：%v", err)
	}
	if os.Getenv("APP_SECRET") == "" {
		return errors.New("APP_SECRET 未设置，拒绝启动")
	}
	if os.Getenv("AGENT_ENGINE") == "" {
		_ = os.Setenv("AGENT_ENGINE", "eino")
	}
	if err := model.InitDB(); err != nil {
		return err
	}
	if db, err := model.DB.DB(); err == nil {
		defer db.Close()
	}
	r := gin.Default()
	templates := env("TEMPLATES_DIR", "templates")
	r.LoadHTMLGlob(filepath.Join(templates, "*", "*.html"))
	r.Static("/static", filepath.Join(templates, "static"))
	web.Setup(r, llm.NewDefaultRegistry())
	defer web.CloseAgentRuntime()
	ln, err := net.Listen("tcp", net.JoinHostPort(env("APP_HOST", "127.0.0.1"), env("APP_PORT", "8080")))
	if err != nil {
		return fmt.Errorf("监听失败: %w", err)
	}
	defer ln.Close()
	web.StartProactiveScheduler(ctx)
	defer web.StopProactiveScheduler()
	// 排期到点自动导出：到期的自媒体排期生成本地成品包，推进台账到 exported。
	web.StartContentPublishScheduler(ctx)
	defer web.StopContentPublishScheduler()
	// 定时唤醒：到点让 agent 在目标会话里自己跑一轮（见 docs/agent/12-wakeup.md）。
	web.StartWakeupScheduler(ctx)
	defer web.StopWakeupScheduler()
	log.Printf("READY http://%s", ln.Addr().String())
	return serve(ctx, ln, r)
}

func serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("关闭 HTTP 服务: %w", err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
