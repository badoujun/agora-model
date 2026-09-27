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
	"strconv"
	"strings"
	"syscall"
	"time"

	"agora-model/internal/api"
	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/gateway"
	"agora-model/internal/logging"
	"agora-model/internal/platform"
	"agora-model/internal/store"
	"agora-model/internal/webui"

	"github.com/kardianos/service"
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
	opts := options{
		ConfigPath:      *configPath,
		DataDir:         *dataDir,
		DBPath:          *dbPath,
		Listen:          *listen,
		Port:            *port,
		ResetGatewayKey: *resetKey,
		AdminPassword:   *adminPass,
	}

	// 服务管理子命令（T5.7）：agoramodel install|uninstall|start|stop|restart|status
	if cmd := serviceCommand(); cmd != "" {
		if err := runServiceCommand(cmd, logger, opts); err != nil {
			logger.Error("服务操作失败", "command", cmd, "err", err)
			os.Exit(1)
		}
		return
	}

	// 由 SCM / systemd / launchd 拉起时，生命周期交给各平台服务管理器；
	// 前台运行时自行响应信号。
	if !service.Interactive() {
		s, err := newService(logger, opts)
		if err != nil {
			logger.Error("初始化服务失败", "err", err)
			os.Exit(1)
		}
		if err := s.Run(); err != nil {
			logger.Error("服务运行失败", "err", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, logger, opts); err != nil {
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

// runServer 启动并运行网关主体逻辑，直到 ctx 被取消。
func runServer(ctx context.Context, logger *slog.Logger, opts options) error {
	// 派生可取消的 ctx：HTTP 服务异常退出时需要主动结束整个进程
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	st, err := store.Open(ctx, dbFile)
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
	imported, err := bootstrap(ctx, st, master, opts.ConfigPath, logger)
	if err != nil {
		return err
	}

	// 4) 网关 Key：确保存在，必要时打印新生成的明文（只打印一次）
	if opts.ResetGatewayKey {
		plain, err := st.ResetGatewayKey(ctx)
		if err != nil {
			return err
		}
		logger.Warn("网关 Key 已重置：旧的 Key 立即失效，请更新所有 Agent 的配置", "gateway_key", plain)
	}
	if plain, created, err := st.EnsureGatewayKey(ctx); err != nil {
		return err
	} else if created {
		logger.Warn("已生成网关 Key（明文只显示这一次，请立即保存）", "gateway_key", plain)
	}
	keyInfo, err := st.ActiveGatewayKey(ctx)
	if err != nil {
		return err
	}

	// 5) 构建快照：供应商 + 其勾选的模型
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
		return config.NewSnapshot(config.File{Gateway: gwSettings, Providers: providers})
	}

	snapshot, err := loadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w（可用 --config 提供引导配置，或等待 Web UI 添加供应商）", err)
	}
	holder := config.NewHolder(snapshot)

	// 6) 请求日志记录器（异步批量写入）
	recorder := logging.NewRecorder(st, logger, logging.RecorderOptions{})
	recorder.Start(ctx)

	// 7) HTTP 服务
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	api.SetLogStatsFunc(recorder.Stats)
	api.New(api.Options{
		Store: st, Master: master, Holder: holder,
		Reload: loadSnapshot, Logger: logger, Version: version,
		AdminPassword: adminPassword, LocalOnly: localOnly,
	}).Register(mux)

	// 8) 内嵌前端（构建后自动可用；未构建时只提供 API）
	if fsys, ferr := webui.FS(); ferr != nil {
		logger.Warn("加载内嵌前端失败", "err", ferr)
	} else if webui.Available(fsys) {
		if rerr := webui.Register(mux, fsys); rerr != nil {
			logger.Warn("挂载 Web UI 失败", "err", rerr)
		} else {
			logger.Info("Web UI 已挂载", "path", "/")
		}
	} else {
		logger.Info("未检测到前端构建产物，仅提供 API（需要 Web UI 请先执行 npm --prefix web run build）")
	}

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
			cancel()
		}
	}()

	<-ctx.Done()
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
	providers := snapshot.Providers()
	logger.Info("agoramodel 启动",
		"version", version, "addr", addr, "data_dir", dataDir, "db", dbFile,
		"providers", len(providers), "imported", imported, "gateway_key_hint", keyInfo.KeyHint)
	for _, p := range providers {
		logger.Info("已加载供应商",
			"id", p.ID, "name", p.Name,
			"models", len(p.Models), "aliases", len(p.ModelAliases),
			"allow_internal", p.AllowInternal)
		if len(p.Models) == 0 {
			logger.Warn("该供应商尚未勾选任何模型，不会被路由命中", "name", p.Name)
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

// ---------------- 服务化（T5.7） ----------------

// serviceCommand 解析服务管理子命令。
func serviceCommand() string {
	if len(os.Args) < 2 {
		return ""
	}
	switch strings.ToLower(os.Args[1]) {
	case "install", "uninstall", "start", "stop", "restart", "status":
		return strings.ToLower(os.Args[1])
	}
	return ""
}

// serviceArguments 生成服务启动参数（路径转绝对，避免服务工作目录不同导致找不到配置）。
//
// 刻意不写入管理密码：请通过服务配置注入 ADMIN_PASSWORD 环境变量，避免明文落在 unit 文件里。
func serviceArguments(opts options) []string {
	args := make([]string, 0, 10)
	appendPath := func(flagName, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if abs, err := filepath.Abs(value); err == nil {
			args = append(args, flagName, abs)
			return
		}
		args = append(args, flagName, value)
	}
	appendPath("--config", opts.ConfigPath)
	appendPath("--data-dir", opts.DataDir)
	appendPath("--db", opts.DBPath)
	if opts.Listen != "" {
		args = append(args, "--listen", opts.Listen)
	}
	if opts.Port != 0 {
		args = append(args, "--port", strconv.Itoa(opts.Port))
	}
	return args
}

func newService(logger *slog.Logger, opts options) (service.Service, error) {
	prg := &serviceProgram{logger: logger, opts: opts}
	return service.New(prg, &service.Config{
		Name:        "agoramodel",
		DisplayName: "AgoraModel Gateway",
		Description: "AI Agent 统一接入网关（OpenAI 协议透传 + 模型选择 + Web 控制台）",
		Arguments:   serviceArguments(opts),
	})
}

func runServiceCommand(cmd string, logger *slog.Logger, opts options) error {
	s, err := newService(logger, opts)
	if err != nil {
		return err
	}
	switch cmd {
	case "install":
		if err := s.Install(); err != nil {
			return err
		}
		logger.Info("服务已安装（Windows: SCM / Linux: systemd / macOS: launchd）", "name", "agoramodel")
		logger.Info("提示：如需登录保护，请在服务配置中注入环境变量 ADMIN_PASSWORD 后再启动")
		return nil
	case "uninstall":
		return s.Uninstall()
	case "start":
		return s.Start()
	case "stop":
		return s.Stop()
	case "restart":
		return s.Restart()
	case "status":
		status, err := s.Status()
		if err != nil {
			return err
		}
		logger.Info("服务状态", "status", serviceStatusText(status))
		return nil
	}
	return fmt.Errorf("未知的服务子命令：%s", cmd)
}

func serviceStatusText(status service.Status) string {
	switch status {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// serviceProgram 把网关主体接入各平台服务管理器的生命周期。
type serviceProgram struct {
	logger *slog.Logger
	opts   options
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *serviceProgram) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		if err := runServer(ctx, p.logger, p.opts); err != nil {
			p.logger.Error("服务已退出", "err", err)
		}
	}()
	return nil
}

// Stop 由服务管理器在停止时调用：取消 ctx，触发记录日志刷盘与数据库关闭。
func (p *serviceProgram) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			p.logger.Warn("服务停止超时：可能仍有连接在传输")
		}
	}
	return nil
}
