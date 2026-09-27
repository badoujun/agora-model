package api

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// dataPathItem 是「数据位置」页面的一行：本程序在磁盘上的一个落点。
type dataPathItem struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // dir | file | stdout
	// Label 是给人看的中文名称，Note 说明它的用途。
	Label     string `json:"label"`
	Note      string `json:"note"`
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"size_bytes"`
	// SizeLabel 是已格式化的大小（目录为空）。
	SizeLabel string `json:"size_label,omitempty"`
}

// handleDataLocations 返回本程序产生的数据在本机的存放位置。
//
// 路径全部来自实际启动参数（main 填充 Options.Paths），不做路径猜测；
// 未配置的项（例如前台运行的日志文件）返回 kind=stdout 并说明原因。
func (s *Server) handleDataLocations(w http.ResponseWriter, r *http.Request) {
	items := make([]dataPathItem, 0, 5)

	if dir := strings.TrimSpace(s.paths.DataDir); dir != "" {
		items = append(items, dirItem(dataPathItem{
			Key:   "data_dir",
			Kind:  "dir",
			Label: "数据目录",
			Note:  "本程序的根目录：数据库、主密钥与日志都在这里；删除它等于清空全部配置",
			Path:  dir,
		}))
	}

	if db := strings.TrimSpace(s.paths.DBPath); db != "" {
		item := fileItem(dataPathItem{
			Key:   "database",
			Kind:  "file",
			Label: "SQLite 数据库",
			Note:  "供应商、网关 Key、模型候选与请求日志；WAL 模式下同目录还有 -wal/-shm 附属文件",
			Path:  db,
		})
		// 附属文件也算数据库占用的空间
		item.SizeBytes += sidecarSize(db+"-wal") + sidecarSize(db+"-shm")
		applySizeLabel(&item)
		items = append(items, item)
	}

	if key := strings.TrimSpace(s.paths.MasterKeyPath); key != "" {
		items = append(items, fileItem(dataPathItem{
			Key:   "master_key",
			Kind:  "file",
			Label: "主密钥文件",
			Note:  "AES-256-GCM 密钥，用于加密供应商凭证与网关 Key 明文；请与数据库一并备份（也可改用环境变量提供）",
			Path:  key,
		}))
	}

	if log := strings.TrimSpace(s.paths.LogFile); log != "" {
		items = append(items, fileItem(dataPathItem{
			Key:   "log_file",
			Kind:  "file",
			Label: "运行日志",
			Note:  "后台服务模式的日志文件；前台运行时写标准输出",
			Path:  log,
		}))
	} else {
		items = append(items, dataPathItem{
			Key:   "log_file",
			Kind:  "stdout",
			Label: "运行日志",
			Note:  "当前前台运行，日志写标准输出，未落盘（安装为服务后默认写 <数据目录>/logs/agoramodel.log）",
		})
	}

	if bin := strings.TrimSpace(s.paths.ServiceBinary); bin != "" {
		items = append(items, fileItem(dataPathItem{
			Key:   "service_binary",
			Kind:  "file",
			Label: "服务可执行文件",
			Note:  "安装为系统服务时复制的二进制副本（升级时自动替换）",
			Path:  bin,
		}))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"os":         runtime.GOOS,
		"os_label":   osLabel(runtime.GOOS),
		"version":    s.version,
		"data_dir":   s.paths.DataDir,
		"executable": executablePath(),
		"items":      items,
	})
}

// dirItem 填充目录项的存在性（目录不计大小）。
func dirItem(item dataPathItem) dataPathItem {
	if info, err := os.Stat(item.Path); err == nil && info.IsDir() {
		item.Exists = true
	}
	return item
}

// fileItem 填充文件项的存在性与大小。
func fileItem(item dataPathItem) dataPathItem {
	if info, err := os.Stat(item.Path); err == nil && !info.IsDir() {
		item.Exists = true
		item.SizeBytes = info.Size()
		applySizeLabel(&item)
	}
	return item
}

// sidecarSize 返回附属文件的大小（不存在时 0）。
func sidecarSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

func applySizeLabel(item *dataPathItem) {
	if item.SizeBytes > 0 {
		item.SizeLabel = humanSize(item.SizeBytes)
	}
}

// humanSize 把字节数格式化成便于阅读的形式。
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	value := float64(bytes)
	for _, name := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return strconv.FormatFloat(value, 'f', 1, 64) + " " + name
		}
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " PB"
}

func osLabel(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	default:
		return goos
	}
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}
