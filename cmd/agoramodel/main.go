// Command agoramodel 是 AgoraModel 网关的入口。
//
// Phase 1：双协议原样透传（/v1/chat/completions、/v1/messages）+ 单网关 Key 鉴权 +
// 按模型名路由 + SSE 心跳保活 + 断连取消；配置来自 JSON 引导文件（Phase 2 起换 SQLite）。
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

	"agora-model/internal/config"
	"agora-model/internal/gateway"
)

// version 由构建脚本以 -ldflags "-X main.version=<ver>" 注入。
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.json", "JSON 引导配置文件路径")
		listen     = flag.String("listen", "", "监听地址（覆盖配置文件；默认 127.0.0.1）")
		port       = flag.Int("port", 0, "监听端口（覆盖配置文件；默认 9090）")
		logFormat  = flag.String("log-format", "text", "日志格式：text|json")
		logLevel   = flag.String("log-level", "info", "日志级别：debug|info|warn|error")
		noColor    = flag.Bool("no-color", false, "禁用彩色输出")
		showVer    = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()
	_ = noColor // slog 的 text handler 不输出颜色，保留该开关以兼容文档中的用法

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := newLogger(*logFormat, *logLevel)

	file, err := config.LoadFile(*configPath)
	if err != nil {
		logger.Error("加载配置失败", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if *listen != "" {
		file.Gateway.Listen = *listen
	}
	if *port != 0 {
		file.Gateway.Port = *port
	}

	snapshot, err := config.NewSnapshot(file)
	if err != nil {
		logger.Error("配置校验失败", "err", err)
		os.Exit(1)
	}
	holder := config.NewHolder(snapshot)
	cfg := snapshot.Gateway()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	gateway.New(holder, logger).Register(mux)

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logStartup(logger, srv.Addr, snapshot)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
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

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"version": version,
	})
}

func logStartup(logger *slog.Logger, addr string, snapshot *config.Snapshot) {
	providers := snapshot.ProvidersByPriority()
	logger.Info("agoramodel 启动", "version", version, "addr", addr, "providers", len(providers))
	for _, p := range providers {
		logger.Info("已加载供应商",
			"id", p.ID, "name", p.Name, "priority", p.Priority,
			"openai_configured", p.Endpoint(config.ProtocolOpenAI) != "",
			"anthropic_configured", p.Endpoint(config.ProtocolAnthropic) != "",
			"models", len(p.Models))
		if len(p.Models) == 0 {
			logger.Warn("该供应商未配置任何模型，Phase 1 不会被路由命中", "id", p.ID)
		}
	}
	cfg := snapshot.Gateway()
	if cfg.Listen != "" && cfg.Listen != "127.0.0.1" && cfg.Listen != "localhost" {
		logger.Warn("监听地址不是本机回环：请确认访问来源已受限、网关 Key 足够强（DESIGN §9）",
			"listen", cfg.Listen)
	}
}

func newLogger(format, level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
