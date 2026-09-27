package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServiceCommandRecognizesKnownSubcommands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "install", args: []string{"install"}, want: ServiceInstall},
		{name: "uninstall", args: []string{"uninstall"}, want: ServiceUninstall},
		{name: "start", args: []string{"start"}, want: ServiceStart},
		{name: "stop", args: []string{"stop"}, want: ServiceStop},
		{name: "restart", args: []string{"restart"}, want: ServiceRestart},
		{name: "status", args: []string{"status"}, want: ServiceStatus},
		{name: "大小写不敏感", args: []string{"INSTALL"}, want: ServiceInstall},
		{name: "带多余参数", args: []string{"install", "--port", "9090"}, want: ServiceInstall},
		{name: "无参数", args: nil, want: ""},
		{name: "非子命令", args: []string{"--version"}, want: ""},
		{name: "未知子命令", args: []string{"frobnicate"}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ServiceCommand(tc.args); got != tc.want {
				t.Fatalf("ServiceCommand(%v) = %q, 期望 %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseEnvFlags(t *testing.T) {
	t.Parallel()

	t.Run("空输入返回 nil", func(t *testing.T) {
		t.Parallel()
		got, err := ParseEnvFlags(nil)
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if got != nil {
			t.Fatalf("期望 nil，得到 %v", got)
		}
	})

	t.Run("解析多个键值", func(t *testing.T) {
		t.Parallel()
		got, err := ParseEnvFlags([]string{"ADMIN_PASSWORD=s3cret", "GW_MASTER_KEY=abc"})
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if got["ADMIN_PASSWORD"] != "s3cret" || got["GW_MASTER_KEY"] != "abc" {
			t.Fatalf("解析结果不符: %v", got)
		}
	})

	t.Run("值中含等号只切第一个", func(t *testing.T) {
		t.Parallel()
		got, err := ParseEnvFlags([]string{"OPAQUE=a=b=c"})
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if got["OPAQUE"] != "a=b=c" {
			t.Fatalf("期望 a=b=c，得到 %q", got["OPAQUE"])
		}
	})

	t.Run("值可以为空", func(t *testing.T) {
		t.Parallel()
		got, err := ParseEnvFlags([]string{"EMPTY="})
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if v, ok := got["EMPTY"]; !ok || v != "" {
			t.Fatalf("期望空值存在，得到 %q (ok=%v)", v, ok)
		}
	})

	for _, bad := range []string{"NO_EQUALS", "=only-value"} {
		t.Run("非法输入 "+bad, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseEnvFlags([]string{bad}); err == nil {
				t.Fatalf("期望报错，输入 %q", bad)
			}
		})
	}
}

func TestDefaultServicePaths(t *testing.T) {
	t.Parallel()

	dataDir := filepath.Join("tmp", "data")

	logFile := DefaultLogFile(dataDir)
	if want := filepath.Join(dataDir, "logs", "agoramodel.log"); logFile != want {
		t.Fatalf("DefaultLogFile = %q, 期望 %q", logFile, want)
	}

	name := "agoramodel"
	if runtime.GOOS == "windows" {
		name = "agoramodel.exe"
	}
	if got, want := DefaultServiceBinaryPath(dataDir), filepath.Join(dataDir, "bin", name); got != want {
		t.Fatalf("DefaultServiceBinaryPath = %q, 期望 %q", got, want)
	}
}

func TestServiceArgumentsAbsolutizesPaths(t *testing.T) {
	t.Parallel()

	spec := ServiceSpec{
		ConfigPath: "config.json",
		DataDir:    filepath.Join(".", "data"),
		DBPath:     filepath.Join(".", "data", "agora.db"),
		Listen:     "0.0.0.0",
		Port:       9090,
		LogFile:    filepath.Join(".", "logs", "agoramodel.log"),
		LogFormat:  "json",
		LogLevel:   "debug",
	}
	args := ServiceArguments(spec)

	// 所有路径类参数都必须是绝对路径：服务的工作目录与安装时不同。
	for i := 0; i+1 < len(args); i += 2 {
		flagName, value := args[i], args[i+1]
		if !strings.HasPrefix(flagName, "--") {
			t.Fatalf("参数 %d 不是 flag: %q", i, flagName)
		}
		switch flagName {
		case "--config", "--data-dir", "--db", "--log-file":
			if !filepath.IsAbs(value) {
				t.Fatalf("%s 的值不是绝对路径: %q", flagName, value)
			}
		}
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{"--listen 0.0.0.0", "--port 9090", "--log-format json", "--log-level debug", "--log-file"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("参数中缺少 %q，实际为 %v", want, args)
		}
	}

	// 管理密码绝不写进服务参数：应通过 --service-env 注入。
	if strings.Contains(joined, "admin-password") || strings.Contains(joined, "ADMIN_PASSWORD") {
		t.Fatalf("服务参数中不应出现管理密码相关项: %v", args)
	}
}

func TestServiceArgumentsOmitsEmptyValues(t *testing.T) {
	t.Parallel()

	spec := ServiceSpec{ConfigPath: "config.json", DataDir: "data"}
	args := ServiceArguments(spec)
	joined := strings.Join(args, " ")

	for _, unwanted := range []string{"--port", "--listen", "--db", "--log-file", "--log-format", "--log-level"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("空值不应被写入服务参数：出现 %q，实际 %v", unwanted, args)
		}
	}
}

func TestResolveDataDir(t *testing.T) {
	t.Parallel()

	t.Run("缺省时落到系统数据目录且为绝对路径", func(t *testing.T) {
		t.Parallel()
		got, err := resolveDataDir("")
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("期望绝对路径，得到 %q", got)
		}
		want, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir 失败: %v", err)
		}
		if got != want {
			t.Fatalf("resolveDataDir(\"\") = %q, 期望 %q", got, want)
		}
	})

	t.Run("显式相对路径被绝对化", func(t *testing.T) {
		t.Parallel()
		got, err := resolveDataDir(filepath.Join(".", "rel"))
		if err != nil {
			t.Fatalf("未预期的错误: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("期望绝对路径，得到 %q", got)
		}
		if !strings.HasSuffix(got, "rel") {
			t.Fatalf("期望以 rel 结尾，得到 %q", got)
		}
	})
}

func TestResolveLogFile(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	if got := resolveLogFile("", dataDir); got != DefaultLogFile(dataDir) {
		t.Fatalf("空值应回落到默认日志文件，得到 %q", got)
	}

	custom := filepath.Join(".", "custom.log")
	got := resolveLogFile(custom, dataDir)
	if !filepath.IsAbs(got) {
		t.Fatalf("自定义日志路径应为绝对路径，得到 %q", got)
	}
	if !strings.HasSuffix(got, "custom.log") {
		t.Fatalf("期望以 custom.log 结尾，得到 %q", got)
	}
}

func TestCopyExecutable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src-bin")
	content := []byte("#!/bin/sh\necho agoramodel\n")
	if err := os.WriteFile(src, content, 0o755); err != nil {
		t.Fatalf("写入源文件失败: %v", err)
	}

	// 目标父目录不存在，应被自动创建（服务副本位于 <数据目录>/bin/）。
	dst := filepath.Join(dir, "nested", "bin", "agoramodel")
	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("复制失败: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读取目标失败: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("内容不一致：得到 %q", got)
	}

	if info, err := os.Stat(dst); err != nil {
		t.Fatalf("stat 目标失败: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("目标应保留可执行位，实际权限 %v", info.Mode().Perm())
	}

	// 临时文件不应残留。
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("临时文件未清理: %v", err)
	}

	// 覆盖已存在的目标（升级场景）。
	if err := os.WriteFile(src, []byte("v2"), 0o755); err != nil {
		t.Fatalf("重写源文件失败: %v", err)
	}
	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("覆盖复制失败: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "v2" {
		t.Fatalf("覆盖后内容不符: %q err=%v", got, err)
	}
}

func TestServiceConfigFor(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	binary := filepath.Join(dataDir, "bin", "agoramodel")
	spec := ServiceSpec{
		ConfigPath: filepath.Join(dataDir, "config.json"),
		DataDir:    dataDir,
		Listen:     "127.0.0.1",
		Port:       9090,
		LogFile:    filepath.Join(dataDir, "logs", "agoramodel.log"),
		LogFormat:  "json",
		LogLevel:   "info",
		EnvVars:    map[string]string{"ADMIN_PASSWORD": "s3cret", "GW_MASTER_KEY": "beef"},
		Executable: binary,
	}

	cfg := serviceConfigFor(spec)

	if cfg.Name != ServiceName {
		t.Fatalf("服务名 = %q, 期望 %q", cfg.Name, ServiceName)
	}
	if cfg.DisplayName == "" || cfg.Description == "" {
		t.Fatal("展示名与描述不应为空")
	}
	// Executable 决定服务启动哪个二进制，必须原样写入（install 已做过绝对化与复制）。
	if cfg.Executable != binary {
		t.Fatalf("Executable = %q, 期望 %q", cfg.Executable, binary)
	}
	// EnvVars → Linux 的 Environment= 行 / Windows 注册表的 Environment 值。
	if len(cfg.EnvVars) != 2 {
		t.Fatalf("EnvVars 数量 = %d, 期望 2", len(cfg.EnvVars))
	}
	if cfg.EnvVars["ADMIN_PASSWORD"] != "s3cret" || cfg.EnvVars["GW_MASTER_KEY"] != "beef" {
		t.Fatalf("EnvVars 内容不符: %v", cfg.EnvVars)
	}

	joined := strings.Join(cfg.Arguments, " ")
	for _, want := range []string{"--data-dir", "--log-file", "--port 9090", "--log-format json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Arguments 缺少 %q: %v", want, cfg.Arguments)
		}
	}
}

func TestIsPermissionError(t *testing.T) {
	t.Parallel()

	if isPermissionError(nil) {
		t.Fatal("nil 不应判定为权限错误")
	}
	if !isPermissionError(os.ErrPermission) {
		t.Fatal("os.ErrPermission 应判定为权限错误")
	}

	cases := []struct {
		msg  string
		want bool
	}{
		{"Access is denied.", true},
		{"Access is denied: creating service", true},
		{"open /etc/systemd/system/agoramodel.service: permission denied", true},
		{"must be root to install service", true},
		{"operation not permitted", true},
		{"The specified service does not exist as an installed service.", false},
		{"服务已存在", false},
	}
	for _, tc := range cases {
		err := errString(tc.msg)
		if got := isPermissionError(err); got != tc.want {
			t.Fatalf("isPermissionError(%q) = %v, 期望 %v", tc.msg, got, tc.want)
		}
	}
}

// errString 是一个最小 error 实现，用于驱动 isPermissionError 的字符串判定分支。
type errString string

func (e errString) Error() string { return string(e) }
