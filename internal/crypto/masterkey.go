package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvMasterKey 是主密钥的推荐来源（不进配置文件、不进数据库）。
const EnvMasterKey = "GW_MASTER_KEY"

// MasterKeyOptions 描述主密钥的来源与落盘位置。
type MasterKeyOptions struct {
	// FilePath 是主密钥文件路径（仅当环境变量缺失时使用）。
	FilePath string
	// Logger 可选：用于提示安全注意事项。
	Logger *slog.Logger
}

// LoadMasterKey 依次尝试：环境变量 GW_MASTER_KEY → 主密钥文件 → 首次生成并落盘（0600）。
//
// 环境变量缺失且主密钥文件不存在时会生成一个 32 字节的随机密钥，
// 并以 64 位 hex 写入 FilePath。
func LoadMasterKey(opts MasterKeyOptions) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv(EnvMasterKey)); raw != "" {
		key, err := ParseMasterKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvMasterKey, err)
		}
		return key, nil
	}
	if strings.TrimSpace(opts.FilePath) == "" {
		return nil, errors.New("未设置 " + EnvMasterKey + "，且未提供主密钥文件路径")
	}

	data, err := os.ReadFile(opts.FilePath)
	switch {
	case err == nil:
		key, perr := ParseMasterKey(string(data))
		if perr != nil {
			return nil, fmt.Errorf("主密钥文件 %s: %w", opts.FilePath, perr)
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("读取主密钥文件 %s: %w", opts.FilePath, err)
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成主密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(opts.FilePath), 0o700); err != nil {
		return nil, fmt.Errorf("创建主密钥目录失败: %w", err)
	}
	if err := os.WriteFile(opts.FilePath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("写入主密钥文件失败: %w", err)
	}

	if opts.Logger != nil {
		opts.Logger.Warn("已生成新的主密钥文件：请妥善备份；丢失后已加密的供应商凭证将无法解密",
			"path", opts.FilePath)
		if runtime.GOOS == "windows" {
			opts.Logger.Warn("Windows 上 0600 权限位不生效：建议改用环境变量 " + EnvMasterKey + "，或收紧该文件的 ACL")
		}
	}
	return key, nil
}
