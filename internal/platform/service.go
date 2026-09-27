package platform

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"
)

// 服务名与展示信息（Windows SCM / Linux systemd / macOS launchd 共用同一个名字）。
const (
	ServiceName        = "agoramodel"
	ServiceDisplayName = "AgoraModel Gateway"
	ServiceDescription = "AI Agent 统一接入网关（OpenAI 协议透传 + 模型选择 + Web 控制台）"
)

// 服务管理子命令。
const (
	ServiceInstall   = "install"
	ServiceUninstall = "uninstall"
	ServiceStart     = "start"
	ServiceStop      = "stop"
	ServiceRestart   = "restart"
	ServiceStatus    = "status"
)

// ServiceSpec 描述注册系统服务所需的全部参数。
type ServiceSpec struct {
	ConfigPath string            // 引导配置，注册时转绝对路径
	DataDir    string            // 数据目录，空则用默认目录；注册时转绝对路径
	DBPath     string            // SQLite 路径，注册时转绝对路径
	Listen     string            // 监听地址
	Port       int               // 监听端口
	LogFile    string            // 服务日志文件，空则由 DefaultLogFile 推导
	LogFormat  string            // text | json
	LogLevel   string            // debug | info | warn | error
	EnvVars    map[string]string // 注入服务进程的环境变量（如 ADMIN_PASSWORD）
	Executable string            // 服务要运行的可执行文件，空则复制当前二进制到 <数据目录>/bin/
}

// RunFunc 是服务主体的运行函数，由调用方注入，避免 platform 反向依赖上层逻辑。
type RunFunc func(ctx context.Context) error

// ServiceCommand 判断位置参数列表的首个元素是否为服务管理子命令（不是则返回空串）。
//
// 说明：`agoramodel install --data-dir X` 里的 --data-dir 落在子命令之后，
// flag 包在遇到第一个非 flag 参数时即停止解析，调用方需要对这些参数补解析一次，
// 因此这里只负责识别，不负责摘除。
func ServiceCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	cmd := strings.ToLower(strings.TrimSpace(args[0]))
	switch cmd {
	case ServiceInstall, ServiceUninstall, ServiceStart, ServiceStop, ServiceRestart, ServiceStatus:
		return cmd
	}
	return ""
}

// ServiceInteractive 判断当前进程是否运行在交互式控制台。
// 由 SCM / systemd / launchd 拉起时为 false，此时生命周期交给服务管理器。
func ServiceInteractive() bool {
	return service.Interactive()
}

// DefaultServiceBinaryPath 返回服务可执行文件的稳定位置：<dataDir>/bin/agoramodel[.exe]。
//
// 为什么需要副本：通过 npm / npx 运行时 os.Executable() 指向 node_modules 或 _npx
// 缓存目录，全局升级、npm ci 或缓存清理都会**整体删除重建**该目录，
// 已注册的服务随即指向一个不存在的文件（Windows 1053 / systemd 203）。
func DefaultServiceBinaryPath(dataDir string) string {
	return filepath.Join(dataDir, "bin", serviceBinaryName())
}

// DefaultLogFile 返回服务模式日志文件的默认位置：<dataDir>/logs/agoramodel.log。
//
// 服务模式下进程没有控制台（DESIGN §12），stdout 会被丢弃，必须落文件（PRD FR-11.4）。
func DefaultLogFile(dataDir string) string {
	return filepath.Join(dataDir, "logs", ServiceName+".log")
}

func serviceBinaryName() string {
	if runtime.GOOS == "windows" {
		return ServiceName + ".exe"
	}
	return ServiceName
}

// ParseEnvFlags 解析 --service-env 的 KEY=VALUE 列表（可重复传入）。
func ParseEnvFlags(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--service-env 需要 KEY=VALUE 形式，收到 %q", p)
		}
		out[key] = value
	}
	return out, nil
}

// ServiceArguments 生成服务的启动参数。
//
// 路径一律转绝对：服务的工作目录与安装时的 cwd 不同（Windows system32 / Linux /），
// 相对路径会导致找不到配置与数据目录。
//
// 刻意不写入管理密码：请用 --service-env 注入 ADMIN_PASSWORD，避免明文散落在参数里。
func ServiceArguments(spec ServiceSpec) []string {
	args := make([]string, 0, 14)

	appendPath := func(flagName, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		args = append(args, flagName, absOr(value))
	}

	appendPath("--config", spec.ConfigPath)
	appendPath("--data-dir", spec.DataDir)
	appendPath("--db", spec.DBPath)
	if strings.TrimSpace(spec.Listen) != "" {
		args = append(args, "--listen", spec.Listen)
	}
	if spec.Port != 0 {
		args = append(args, "--port", strconv.Itoa(spec.Port))
	}
	appendPath("--log-file", spec.LogFile)
	if strings.TrimSpace(spec.LogFormat) != "" {
		args = append(args, "--log-format", spec.LogFormat)
	}
	if strings.TrimSpace(spec.LogLevel) != "" {
		args = append(args, "--log-level", spec.LogLevel)
	}
	return args
}

// RunServiceCommand 执行服务管理子命令。
func RunServiceCommand(cmd string, spec ServiceSpec, logger *slog.Logger) error {
	switch cmd {
	case ServiceInstall:
		return installService(spec, logger)
	case ServiceUninstall:
		return uninstallService(spec, logger)
	case ServiceStart, ServiceStop, ServiceRestart:
		s, err := newServiceHandle(spec, logger, nil)
		if err != nil {
			return err
		}
		var opErr error
		switch cmd {
		case ServiceStart:
			opErr = s.Start()
		case ServiceStop:
			opErr = s.Stop()
		case ServiceRestart:
			opErr = s.Restart()
		}
		if opErr != nil {
			return serviceOpError(opErr)
		}
		logger.Info("服务操作完成", "name", ServiceName, "command", cmd)
		return nil
	case ServiceStatus:
		s, err := newServiceHandle(spec, logger, nil)
		if err != nil {
			return err
		}
		status, err := s.Status()
		if err != nil {
			return fmt.Errorf("查询服务状态失败（服务可能尚未安装）: %w", err)
		}
		logger.Info("服务状态", "name", ServiceName, "status", serviceStatusText(status))
		return nil
	}
	return fmt.Errorf("未知的服务子命令：%s", cmd)
}

// RunAsService 以服务模式运行：把生命周期交给各平台服务管理器。
func RunAsService(spec ServiceSpec, logger *slog.Logger, run RunFunc) error {
	s, err := newServiceHandle(spec, logger, run)
	if err != nil {
		return err
	}
	return s.Run()
}

// installService 注册系统服务。
//
// 幂等：同名服务已存在时先停止并卸载，再重新安装——这也是升级二进制后的标准动作
// （先卸旧服务才能覆盖正在运行的副本）。
func installService(spec ServiceSpec, logger *slog.Logger) error {
	dataDir, err := resolveDataDir(spec.DataDir)
	if err != nil {
		return err
	}
	if err := EnsureDataDir(dataDir); err != nil {
		return err
	}
	spec.DataDir = dataDir
	spec.LogFile = resolveLogFile(spec.LogFile, dataDir)
	spec.DBPath = absOr(spec.DBPath)
	spec.ConfigPath = absOr(spec.ConfigPath)

	// 决定服务要运行的可执行文件；用户未显式指定时复制到 <数据目录>/bin/ 下的稳定位置。
	copyFrom := ""
	if strings.TrimSpace(spec.Executable) == "" {
		current, err := os.Executable()
		if err != nil {
			return fmt.Errorf("无法确定当前可执行文件路径: %w", err)
		}
		stable := DefaultServiceBinaryPath(dataDir)
		spec.Executable = stable
		if !sameFile(current, stable) {
			copyFrom = current
		}
	} else {
		spec.Executable = absOr(spec.Executable)
	}

	s, err := newServiceHandle(spec, logger, nil)
	if err != nil {
		return err
	}

	// 服务已存在 → 先停再卸：既让 install 幂等，也释放被占用的二进制副本
	// （Windows 不允许覆盖正在运行的 exe）。
	if _, statusErr := s.Status(); statusErr == nil {
		logger.Info("检测到同名服务已存在，先停止并卸载旧服务", "name", ServiceName)
		if err := s.Stop(); err != nil {
			logger.Warn("停止旧服务失败（可能本就未运行）", "err", err)
		}
		if err := s.Uninstall(); err != nil {
			return fmt.Errorf("卸载旧服务失败: %w", serviceOpError(err))
		}
	}

	if copyFrom != "" {
		if err := copyExecutable(copyFrom, spec.Executable); err != nil {
			return fmt.Errorf("复制可执行文件到 %s 失败（若服务正在运行请先执行 `%s stop`）: %w",
				spec.Executable, ServiceName, err)
		}
		logger.Info("已复制可执行文件到稳定位置", "from", copyFrom, "to", spec.Executable)
	}

	if err := s.Install(); err != nil {
		return fmt.Errorf("注册系统服务失败: %w", serviceOpError(err))
	}

	logger.Info("服务已安装",
		"name", ServiceName,
		"exec", spec.Executable,
		"data_dir", spec.DataDir,
		"log_file", spec.LogFile,
	)
	logger.Info("后续操作", "next", ServiceName+" start")
	if len(spec.EnvVars) > 0 {
		logger.Warn("已注入环境变量：明文会落在服务配置中（Linux systemd unit / Windows 注册表），请限制该文件权限",
			"keys", envKeys(spec.EnvVars))
	} else {
		logger.Info("提示：如需登录保护或免 master.key 文件，请用 --service-env 注入 ADMIN_PASSWORD / GW_MASTER_KEY 后重装")
	}
	return nil
}

// uninstallService 注销系统服务。数据目录（含数据库与主密钥）刻意保留，避免误删用户数据。
func uninstallService(spec ServiceSpec, logger *slog.Logger) error {
	s, err := newServiceHandle(spec, logger, nil)
	if err != nil {
		return err
	}
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("注销系统服务失败: %w", serviceOpError(err))
	}
	dataDir := spec.DataDir
	if strings.TrimSpace(dataDir) == "" {
		if d, err := DataDir(); err == nil {
			dataDir = d
		}
	}
	logger.Info("服务已注销", "name", ServiceName)
	if strings.TrimSpace(dataDir) != "" {
		logger.Info("数据目录已保留（含数据库与主密钥），如需彻底清理请手动删除", "data_dir", absOr(dataDir))
	}
	return nil
}

// newServiceHandle 构造 kardianos/service 句柄。run 为 nil 时仅用于管理操作。
func newServiceHandle(spec ServiceSpec, logger *slog.Logger, run RunFunc) (service.Service, error) {
	prog := &serviceProgram{logger: logger, run: run}
	s, err := service.New(prog, serviceConfigFor(spec))
	if err != nil {
		return nil, fmt.Errorf("初始化服务句柄失败: %w", err)
	}
	return s, nil
}

// serviceConfigFor 构造平台服务配置（纯函数，便于测试）。
func serviceConfigFor(spec ServiceSpec) *service.Config {
	return &service.Config{
		Name:        ServiceName,
		DisplayName: ServiceDisplayName,
		Description: ServiceDescription,
		Executable:  spec.Executable,
		Arguments:   ServiceArguments(spec),
		EnvVars:     spec.EnvVars,
	}
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
	run    RunFunc
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *serviceProgram) Start(service.Service) error {
	if p.run == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		if err := p.run(ctx); err != nil {
			p.logger.Error("服务已退出", "err", err)
		}
	}()
	return nil
}

// Stop 由服务管理器在停止时调用：取消 ctx，触发日志刷盘、HTTP 优雅关闭与数据库关闭。
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

// resolveDataDir 推导数据目录并转绝对路径。
//
// 服务模式的账户往往与前台运行不同（Windows SCM 默认 LocalSystem），
// os.UserConfigDir() 因此会解析到另一个位置；把绝对路径固化进服务配置，
// 可以避免「服务起来了但数据是空的」这类困惑。
func resolveDataDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		d, err := DataDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("数据目录转绝对路径失败: %w", err)
	}
	return abs, nil
}

func resolveLogFile(path, dataDir string) string {
	if strings.TrimSpace(path) == "" {
		return DefaultLogFile(dataDir)
	}
	return absOr(path)
}

func absOr(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func envKeys(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	return keys
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// copyExecutable 原子复制二进制：先写临时文件再改名，避免半截文件被服务启动。
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// serviceOpError 在权限类失败上补充人话提示。
func serviceOpError(err error) error {
	if err == nil {
		return nil
	}
	if isPermissionError(err) {
		return fmt.Errorf("%w（注册 / 注销系统服务需要管理员权限：Windows 请以管理员身份运行，Linux / macOS 请使用 sudo）", err)
	}
	return err
}

func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"access is denied",
		"permission denied",
		"operation not permitted",
		"must be root",
		"requires root",
		"拒绝访问",
		"权限",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
