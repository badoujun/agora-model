// Command agoramodel 是 AgoraModel 网关的入口。
//
// Phase 0 骨架：解析启动参数、暴露 /healthz、响应信号优雅退出。
// 数据面（/v1/chat/completions、/v1/messages、/v1/models）与管理 API 在 Phase 1 起逐步实现，
// 完整启动配置（环境变量回退、数据目录、主密钥）见 docs/DESIGN.md §4.4。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 由构建脚本以 -ldflags "-X main.version=<ver>" 注入。
var version = "dev"

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1", "监听地址（默认仅本机；改为 0.0.0.0 时须设置管理密码）")
		port      = flag.Int("port", 9090, "监听端口")
		dbPath    = flag.String("db", "", "SQLite 数据库路径（默认使用用户数据目录）")
		logFormat = flag.String("log-format", "text", "日志格式：text|json")
		noColor   = flag.Bool("no-color", false, "禁用彩色输出")
		showVer   = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()

	// TODO(Phase 1 · T1.1)：接入环境变量回退（LISTEN_ADDR / PORT / DB_PATH / GW_MASTER_KEY /
	// SSE_IDLE_SECONDS / MAX_BODY_BYTES / LOG_LEVEL / ADMIN_PASSWORD）与平台数据目录。
	_ = dbPath
	_ = noColor

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := newLogger(*logFormat)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": version,
		})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *listen, *port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("agoramodel 启动", "addr", srv.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
		os.Exit(1)
	}
	logger.Info("已停止")
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
