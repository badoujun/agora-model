package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoggerWritesToFile(t *testing.T) {
	t.Parallel()

	// 父目录不存在：应自动创建（服务模式的日志目录位于 <数据目录>/logs/）。
	logFile := filepath.Join(t.TempDir(), "logs", "agoramodel.log")

	logger, closer, err := NewLogger(LoggerOptions{Format: "text", Level: "info", File: logFile})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	logger.Info("网关已启动", "addr", "127.0.0.1:9090")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "网关已启动") || !strings.Contains(text, "127.0.0.1:9090") {
		t.Fatalf("日志内容不符: %q", text)
	}
}

func TestNewLoggerAppendsInsteadOfTruncating(t *testing.T) {
	t.Parallel()

	logFile := filepath.Join(t.TempDir(), "agoramodel.log")
	if err := os.WriteFile(logFile, []byte("上一轮运行的遗留内容\n"), 0o600); err != nil {
		t.Fatalf("预置日志文件失败: %v", err)
	}

	logger, closer, err := NewLogger(LoggerOptions{Format: "text", Level: "info", File: logFile})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	logger.Info("本轮启动")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "上一轮运行的遗留内容") {
		t.Fatalf("追加模式应保留旧内容: %q", text)
	}
	if !strings.Contains(text, "本轮启动") {
		t.Fatalf("缺少本轮日志: %q", text)
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	t.Parallel()

	logFile := filepath.Join(t.TempDir(), "agoramodel.log")
	logger, closer, err := NewLogger(LoggerOptions{Format: "text", Level: "warn", File: logFile})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	logger.Debug("调试信息")
	logger.Info("普通信息")
	logger.Warn("告警信息")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "调试信息") || strings.Contains(text, "普通信息") {
		t.Fatalf("低于级别的日志不应写入: %q", text)
	}
	if !strings.Contains(text, "告警信息") {
		t.Fatalf("缺少告警日志: %q", text)
	}
}

func TestNewLoggerJSONFormat(t *testing.T) {
	t.Parallel()

	logFile := filepath.Join(t.TempDir(), "agoramodel.log")
	logger, closer, err := NewLogger(LoggerOptions{Format: "json", Level: "info", File: logFile})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	logger.Info("启动", "port", 9090)
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"port":9090`) {
		t.Fatalf("不是预期的 JSON 日志: %q", line)
	}
}

func TestNewLoggerWithoutFileUsesStdout(t *testing.T) {
	t.Parallel()

	// 未指定文件时写 stdout：closer 必须是安全的空实现，调用方可以无条件 Close。
	logger, closer, err := NewLogger(LoggerOptions{Format: "text", Level: "info"})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	if logger == nil {
		t.Fatal("logger 不应为 nil")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("空 closer 不应报错: %v", err)
	}
}

func TestNewLoggerUnknownLevelFallsBackToInfo(t *testing.T) {
	t.Parallel()

	logFile := filepath.Join(t.TempDir(), "agoramodel.log")
	logger, closer, err := NewLogger(LoggerOptions{Format: "text", Level: "not-a-level", File: logFile})
	if err != nil {
		t.Fatalf("NewLogger 失败: %v", err)
	}
	logger.Info("应被写入")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if !strings.Contains(string(data), "应被写入") {
		t.Fatalf("非法级别应回落到 info: %q", data)
	}
}
