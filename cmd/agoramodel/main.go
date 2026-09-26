// Command agoramodel 是 AgoraModel 网关的入口。
//
// Phase 2：供应商/网关 Key/请求日志持久化到 SQLite（WAL），凭证以 AES-256-GCM 加密存储；
// JSON 引导配置仅在数据库为空时用于首次导入。
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agora-model/internal/api"
	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/gateway"
	"agora-model/internal/logging"
	"agora-model/internal/models"
	"agora-model/internal/platform"
	"agora-model/internal/store"
)

// version 由构建脚本以 -ldflags "-X main.version=<ver>" 注入。
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.json", "JSON 引导配置（仅当数据库中没有供应商时用于首次导入）")
		dataDir    = flag.String("data-dir", "", "数据目录（默认使用系统用户配置目录）")
		dbPath     = flag.String("db", "", "SQLite 文件路径（默认 <数据目录>/agora.db）")
		listen     = flag.String("listen", "", "监听地址（默认 127.0.0.1）")
		port       = flag.Int("port", 0, "监听端口（默认 9090）")
		logFormat  = flag.String("log-format", "text", "日志格式：text|json")
		logLevel   = flag.String("log-level", "info", "日志级别：debug|info|warn|error")
		noColor    = flag.Bool("no-color", false, "禁用彩色输出")
		resetKey   = flag.Bool("reset-gateway-key", false, "吊销现有网关 Key 并生成新的（明文只打印一次）")
		adminPass  = flag.String("admin-password", "", "Web UI 管理密码（也可用环境变量 ADMIN_PASSWORD；非回环监听时必填）")
		showVer    = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()
	_ = noColor // slog 的 text handler 不输出颜色，保留该开关以兼容文档中的用法

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := newLogger(*logFormat, *logLevel)
	if err := run(logger, options{
		ConfigPath:      *configPath,
		DataDir:         *dataDir,
		DBPath:          *dbPath,
		Listen:          *listen,
		Port:            *port,
		ResetGatewayKey: *resetKey,
		AdminPassword:   *adminPass,
	}); err != nil {
		logger.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

type options struct {
	ConfigPath      string
	DataDir         string
	DBPath          string
	Listen          string
	Port            int
	ResetGatewayKey bool
	AdminPassword   string
}

func run(logger *slog.Logger, opts options) error {
	// 1) 数据目录与数据库
	dir := opts.DataDir
	if dir == "" {
		d, err := platform.DataDir()
		if err != nil {
			return err
		}
		dir = d
	}
	if err := platform.EnsureDataDir(dir); err != nil {
		return err
	}
	dbFile := opts.DBPath
	if dbFile == "" {
		dbFile = filepath.Join(dir, "agora.db")
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(rootCtx, dbFile)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Warn("关闭数据库失败", "err", cerr)
		}
	}()

	// 2) 主密钥（环境变量优先，其次主密钥文件）
	master, err := crypto.LoadMasterKey(crypto.MasterKeyOptions{
		FilePath: filepath.Join(dir, "master.key"),
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	// 3) 首次启动：用 JSON 引导配置导入供应商（之后以数据库为准）
	imported, err := bootstrap(rootCtx, st, master, opts.ConfigPath, logger)
	if err != nil {
		return err
	}

	// 4) 网关 Key：确保存在，必要时打印新生成的明文（只打印一次）
	if opts.ResetGatewayKey {
		plain, err := st.ResetGatewayKey(rootCtx)
		if err != nil {
			return err
		}
		logger.Warn("网关 Key 已重置：旧的 Key 立即失效，请更新所有 Agent 的配置", "gateway_key", plain)
	}
	if plain, created, err := st.EnsureGatewayKey(rootCtx); err != nil {
		return err
	} else if created {
		logger.Warn("已生成网关 Key（明文只显示这一次，请立即保存）", "gateway_key", plain)
	}
	keyInfo, err := st.ActiveGatewayKey(rootCtx)
	if err != nil {
		return err
	}

	// 5) 构建快照：供应商 + 模型缓存（(手动 ∪ 自动) − 排除）
	gwSettings := config.Gateway{
		Listen:         config.DefaultListen,
		Port:           config.DefaultPort,
		SSEIdleSeconds: config.DefaultSSEIdleSeconds,
		MaxBodyBytes:   config.DefaultMaxBodyBytes,
	}
	if f, err := config.LoadFile(opts.ConfigPath); err == nil {
		// 引导配置里的网关设置作为默认值；Phase 2 起网关 Key 由数据库管理，此处忽略 api_key
		if f.Gateway.Listen != "" {
			gwSettings.Listen = f.Gateway.Listen
		}
		if f.Gateway.Port != 0 {
			gwSettings.Port = f.Gateway.Port
		}
		if f.Gateway.SSEIdleSeconds > 0 {
			gwSettings.SSEIdleSeconds = f.Gateway.SSEIdleSeconds
		}
		if f.Gateway.MaxBodyBytes > 0 {
			gwSettings.MaxBodyBytes = f.Gateway.MaxBodyBytes
		}
	}
	if opts.Listen != "" {
		gwSettings.Listen = opts.Listen
	}
	if opts.Port != 0 {
		gwSettings.Port = opts.Port
	}

	// 访问控制：默认仅回环；非回环监听必须设置管理密码（DESIGN §9）
	adminPassword := strings.TrimSpace(opts.AdminPassword)
	if adminPassword == "" {
		adminPassword = strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	}
	localOnly := isLoopbackHost(gwSettings.Listen)
	if !localOnly && adminPassword == "" {
		return fmt.Errorf("监听 %s 但未设置管理密码：请设置 ADMIN_PASSWORD（或 --admin-password）后再暴露到非回环地址", gwSettings.Listen)
	}

	loadSnapshot := func(ctx context.Context) (*config.Snapshot, error) {
		providers, err := st.LoadProviders(ctx, master)
		if err != nil {
			return nil, err
		}
		cache, err := st.LoadAllModelCache(ctx)
		if err != nil {
			return nil, err
		}
		for i := range providers {
			providers[i].Models = models.Available(providers[i].Models, cache[providers[i].ID], providers[i].ModelsExcluded)
		}
		return config.NewSnapshot(config.File{Gateway: gwSettings, Providers: providers})
	}

	snapshot, err := loadSnapshot(rootCtx)
	if err != nil {
		return fmt.Errorf("%w（可用 --config 提供引导配置，或等待 Web UI 添加供应商）", err)
	}
	holder := config.NewHolder(snapshot)

	// 6) 模型聚合：启动即拉取一次，随后按间隔刷新，完成后原子替换快照
	aggregator := models.NewAggregator(st, logger, models.Options{
		Master: master,
		OnRefresh: func(ctx context.Context) {
			next, err := loadSnapshot(ctx)
			if err != nil {
				logger.Warn("模型聚合后重建快照失败", "err", err)
				return
			}
			holder.Store(next)
		},
	})
	aggregator.Start(rootCtx)

	// 7) 请求日志记录器（异步批量写入）
	recorder := logging.NewRecorder(st, logger, logging.RecorderOptions{})
	recorder.Start(rootCtx)

	// 8) HTTP 服务
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	api.SetLogStatsFunc(recorder.Stats)
	api.New(api.Options{
		Store: st, Master: master, Holder: holder, Aggregator: aggregator,
		Reload: loadSnapshot, Logger: logger, Version: version,
		AdminPassword: adminPassword, LocalOnly: localOnly,
	}).Register(mux)
	gateway.New(holder, logger).
		WithKeyStore(st).
		WithRecorder(recorder).
		Register(mux)

	srv := &http.Server{
		Addr:              snapshot.Gateway().Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logStartup(logger, srv.Addr, dir, dbFile, keyInfo, snapshot, imported)
	if localOnly {
		if adminPassword == "" {
			logger.Info("Web UI 免登录（仅本机回环监听）")
		} else {
			logger.Info("Web UI 已启用登录（本机回环监听 + 管理密码）")
		}
	} else {
		logger.Info("Web UI 已启用登录（非回环监听）", "listen", gwSettings.Listen)
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "err", err)
			stop()
		}
	}()

	<-rootCtx.Done()
	logger.Info("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
	}

	// 等待日志刷完（ctx 已取消，Recorder 会把剩余批次写完）
	if !recorder.Wait(5 * time.Second) {
		logger.Warn("请求日志刷新超时，可能有少量日志未落库")
	}
	written, dropped := recorder.Stats()
	logger.Info("已停止", "logs_written", written, "logs_dropped", dropped)
	return nil
}

// bootstrap 在数据库为空时导入 JSON 引导配置。
func bootstrap(ctx context.Context, st *store.Store, master []byte, configPath string, logger *slog.Logger) (int, error) {
	count, err := st.ProviderCount(ctx)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		return 0, nil
	}

	file, err := config.LoadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("数据库中没有供应商，且未找到引导配置：网关将以空供应商启动（可稍后通过 Web UI 添加）",
				"config", configPath)
			return 0, nil
		}
		return 0, err
	}

	n, err := st.ImportProviders(ctx, master, file.Providers)
	if err != nil {
		return 0, err
	}
	logger.Info("已从引导配置导入供应商", "config", configPath, "count", n)
	return n, nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"version": version,
	})
}

func logStartup(logger *slog.Logger, addr, dataDir, dbFile string, keyInfo store.GatewayKeyInfo, snapshot *config.Snapshot, imported int) {
	providers := snapshot.ProvidersByPriority()
	logger.Info("agoramodel 启动",
		"version", version, "addr", addr, "data_dir", dataDir, "db", dbFile,
		"providers", len(providers), "imported", imported, "gateway_key_hint", keyInfo.KeyHint)
	for _, p := range providers {
		logger.Info("已加载供应商",
			"id", p.ID, "name", p.Name, "priority", p.Priority,
			"openai_configured", p.Endpoint(config.ProtocolOpenAI) != "",
			"anthropic_configured", p.Endpoint(config.ProtocolAnthropic) != "",
			"models", len(p.Models), "allow_internal", p.AllowInternal)
		if len(p.Models) == 0 {
			logger.Warn("该供应商未配置任何模型，Phase 3 前不会被路由命中", "id", p.ID)
		}
	}
	cfg := snapshot.Gateway()
	if cfg.Listen != "" && cfg.Listen != "127.0.0.1" && cfg.Listen != "localhost" {
		logger.Warn("监听地址不是本机回环：请确认访问来源已受限、网关 Key 足够强（DESIGN §9）",
			"listen", cfg.Listen)
	}
}

// isLoopbackHost 判断监听地址是否仅本机回环。
func isLoopbackHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
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
