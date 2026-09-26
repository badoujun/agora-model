package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppName 是数据目录使用的前缀。
const AppName = "AgoraModel"

// linuxDirName 在 Linux 上使用小写目录名（~/.config/agoramodel）。
const linuxDirName = "agoramodel"

// DataDir 返回默认数据目录：
//
//	Windows: %AppData%\AgoraModel
//	Linux:   ~/.config/agoramodel
//	macOS:   ~/Library/Application Support/AgoraModel
//
// 统由 os.UserConfigDir() 得出，不做硬编码路径拼接。
func DataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户配置目录: %w", err)
	}
	name := AppName
	if runtime.GOOS == "linux" {
		name = linuxDirName
	}
	return filepath.Join(base, name), nil
}

// EnsureDataDir 创建数据目录（0700）。
func EnsureDataDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("数据目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	return nil
}
