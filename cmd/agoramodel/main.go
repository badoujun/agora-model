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
	"net"
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
	"agora-model/internal/platform"
	"agora-model/internal/presets"
	"agora-model/internal/store"
	"agora-model/internal/webui"
)

// version 是程序版本号（语义化版本）。发布构建可用
// -ldflags "-X main.version=<tag>" 注入 git tag；未注入时用这里的内置版本号，
// 保证控制台左上角始终显示可读的版本号而不是构建哈希。
var version = "0.3.0"

func main() {
	var (
		configPath  = flag.String("config", "config.json", "JSON 引导配置（仅当数据库中没有供应商时用于首次导入）")
		presetsPath = flag.String("presets", "", "国内常用供应商预设 JSON（默认使用编译期内置版本）")
		dataDir     = flag.String("data-dir", "", "数据目录（默认使用系统用户配置目录）")
		dbPath      = flag.String("db", "", "SQLite 文件路径（默认 <数据目录>/agora.db）")
		listen      = flag.String("listen", "", "监听地址（默认 127.0.0.1）")
		port        = flag.Int("port", 0, "监听端口（默认 9090）")
		logFormat   = flag.String("log-format", "text", "日志格式：text|json")
		logLevel    = flag.String("log-level", "info", "日志级别：debug|info|warn|error")
		logFile     = flag.String("log-file", "", "日志文件路径（默认写 stdout；install 缺省为 <数据目录>/logs/agoramodel.log）")
		noColor     = flag.Bool("no-color", false, "禁用彩色输出")
		resetKey    = flag.Bool("reset-gateway-key", false, "吊销现有网关 Key 并生成新的（明文只打印一次）")
		adminPass   = flag.String("admin-password", "", "Web UI 管理密码（也可用环境变量 ADMIN_PASSWORD；非回环监听时必填）")
		showVer     = flag.Bool("version", false, "打印版本并退出")
		svcExec     = flag.String("service-exec", "", "服务要运行的可执行文件（install 用；默认复制当前二进制到 <数据目录>/bin/ 后运行该副本）")
		svcEnv      stringList
	)
	flag.Var(&svcEnv, "service-env", "注入服务进程的环境变量，可重复：--service-env ADMIN_PASSWORD=xxx（明文会写入服务配置）")
	flag.Parse()
	_ = noColor // slog 的 text handler 不输出颜色，保留该开关以兼容文档中的用法

	if *showVer {
		fmt.Println(version)
		return
	}

	// 服务管理子命令（T5.7）：agoramodel install|uninstall|start|stop|restart|status
	//
	// 子命令允许带参数（如 `agoramodel install --data-dir X`），但 flag 包在遇到第一个
	// 非 flag 参数时就停止解析，因此对子命令之后的参数需要再解析一次。
	serviceCmd := ""
	if rest := flag.Args(); len(rest) > 0 {
		if cmd := platform.ServiceCommand(rest); cmd != "" {
			serviceCmd = cmd
			if err := flag.CommandLine.Parse(rest[1:]); err != nil {
				os.Exit(2)
			}
		}
	}

	// 服务模式没有控制台：install 时会缺省注入 --log-file，落到 <数据目录>/logs/agoramodel.log
	logger, logCloser, err := platform.NewLogger(platform.LoggerOptions{
		Format: *logFormat,
		Level:  *logLevel,
		File:   *logFile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化日志失败:", err)
		os.Exit(1)
	}
	defer func() { _ = logCloser.Close() }()

	envVars, err := platform.ParseEnvFlags(svcEnv)
	if err != nil {
		logger.Error("参数错误", "err", err)
		os.Exit(1)
	}

	spec := platform.ServiceSpec{
		ConfigPath: *configPath,
		DataDir:    *dataDir,
		DBPath:     *dbPath,
		Listen:     *listen,
		Port:       *port,
		LogFile:    *logFile,
		LogFormat:  *logFormat,
		LogLevel:   *logLevel,
		EnvVars:    envVars,
		Executable: *svcExec,
	}
	if serviceCmd != "" {
		if err := platform.RunServiceCommand(serviceCmd, spec, logger); err != nil {
			logger.Error("服务操作失败", "command", serviceCmd, "err", err)
			os.Exit(1)
		}
		return
	}

	opts := options{
		ConfigPath:      *configPath,
		PresetsPath:     *presetsPath,
		DataDir:         *dataDir,
		DBPath:          *dbPath,
		Listen:          *listen,
		Port:            *port,
		LogFile:         *logFile,
		ResetGatewayKey: *resetKey,
		AdminPassword:   *adminPass,
	}

	// 由 SCM / systemd / launchd 拉起时，生命周期交给各平台服务管理器；
	// 前台运行时自行响应信号。
	if !platform.ServiceInteractive() {
		if err := platform.RunAsService(spec, logger, func(ctx context.Context) error {
			return runServer(ctx, logger, opts)
		}); err != nil {
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

// stringList 是可重复传入的字符串 flag（--service-env A=1 --service-env B=2）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type options struct {
	ConfigPath      string
	PresetsPath     string
	DataDir         string
	DBPath          string
	Listen          string
	Port            int
	LogFile         string
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

	// 4) 网关 Key：确保存在，必要时打印新生成的明文
	if opts.ResetGatewayKey {
		plain, err := st.ResetGatewayKey(ctx, master)
		if err != nil {
			return err
		}
		logger.Warn("网关 Key 已重置：旧的 Key 立即失效，请更新所有 Agent 的配置", "gateway_key", plain)
	}
	if plain, created, err := st.EnsureGatewayKey(ctx, master); err != nil {
		return err
	} else if created {
		logger.Warn("已生成网关 Key（可在 Web UI「网关设置」查看或复制）", "gateway_key", plain)
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
		return fmt.Errorf("加载供应商配置失败: %w", err)
	}
	holder := config.NewHolder(snapshot)

	// 6) 请求日志记录器（异步批量写入）
	recorder := logging.NewRecorder(st, logger, logging.RecorderOptions{})
	recorder.Start(ctx)

	// 7) HTTP 服务
	presetStore := presets.New(presets.Options{
		OverridePath: opts.PresetsPath,
		Logger: func(msg string, args ...any) {
			logger.Warn(msg, args...)
		},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	api.SetLogStatsFunc(recorder.Stats)
	api.New(api.Options{
		Store: st, Master: master, Holder: holder,
		Reload: loadSnapshot, Logger: logger, Version: version,
		AdminPassword: adminPassword, LocalOnly: localOnly,
		Paths:   dataPaths(dir, dbFile, opts.LogFile),
		Presets: presetStore,
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

// dataPaths 汇总「数据位置」页面要展示的本机路径。
//
// 只报告真实使用的路径：日志在前台运行时写 stdout，因此返回空串由页面说明；
// 服务可执行文件副本只在存在（已安装为服务）时报告。
func dataPaths(dataDir, dbFile, logFileFlag string) api.DataPaths {
	paths := api.DataPaths{
		DataDir:       dataDir,
		DBPath:        dbFile,
		MasterKeyPath: filepath.Join(dataDir, "master.key"),
		LogFile:       strings.TrimSpace(logFileFlag),
	}
	if paths.LogFile == "" && !platform.ServiceInteractive() {
		// 由 SCM / systemd / launchd 拉起时没有控制台，日志一定落文件
		paths.LogFile = platform.DefaultLogFile(dataDir)
	}
	if bin := platform.DefaultServiceBinaryPath(dataDir); fileExists(bin) {
		paths.ServiceBinary = bin
	}
	return paths
}

// fileExists 报告路径是否存在且是文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
	if len(providers) == 0 {
		// 全新安装的正常状态：此时网关能跑、Web UI 能开，但还没有可路由的模型
		logger.Warn("尚未配置任何供应商：网关已启动，但 /v1 请求暂时无法路由",
			"next", "打开 "+webUIURL(addr)+" 在「供应商管理」中添加")
	}
	if cfg.Listen != "" && cfg.Listen != "127.0.0.1" && cfg.Listen != "localhost" {
		logger.Warn("监听地址不是本机回环：请确认访问来源已受限、网关 Key 足够强（DESIGN §9）",
			"listen", cfg.Listen)
	}
}

// webUIURL 把监听地址转成可直接打开的 Web UI 地址（通配监听时落到回环）。
func webUIURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	// JoinHostPort 会自动为 IPv6 补方括号
	return "http://" + net.JoinHostPort(host, port)
}

// isLoopbackHost 判断监听地址是否仅本机回环。
func isLoopbackHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}
