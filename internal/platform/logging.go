package platform

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// LoggerOptions 描述进程日志的输出目标。
type LoggerOptions struct {
	Format string // text | json
	Level  string // debug | info | warn | error
	File   string // 为空则写 stdout；服务模式必须指定（DESIGN §12：服务模式没有控制台）
}

// nopCloser 用于「日志写 stdout」时占位，保证调用方无需区分分支。
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// NewLogger 构造进程日志器。
//
// 指定 File 时追加写入该文件（自动创建父目录）；否则写 stdout。
// 之所以需要这个开关：服务模式下进程没有控制台（Windows SCM 尤其如此），
// stdout 会被直接丢弃，必须落文件才能排查问题（PRD FR-11.4）。
//
// 返回的 io.Closer 在 File 为空时是空实现，调用方可以无条件 Close。
func NewLogger(opts LoggerOptions) (*slog.Logger, io.Closer, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(opts.Level))); err != nil {
		level = slog.LevelInfo
	}
	handlerOpts := &slog.HandlerOptions{Level: level}

	var (
		writer io.Writer = os.Stdout
		closer io.Closer = nopCloser{}
	)

	if target := strings.TrimSpace(opts.File); target != "" {
		path, err := filepath.Abs(target)
		if err != nil {
			path = target
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, fmt.Errorf("创建日志目录失败: %w", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
		}
		writer = f
		closer = f
	}

	var handler slog.Handler
	if strings.TrimSpace(opts.Format) == "json" {
		handler = slog.NewJSONHandler(writer, handlerOpts)
	} else {
		handler = slog.NewTextHandler(writer, handlerOpts)
	}
	return slog.New(handler), closer, nil
}
