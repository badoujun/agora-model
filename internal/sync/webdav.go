// Package sync 提供供应商配置的 WebDAV 同步：
//
//   - 上传本地供应商配置到远端（push）
//   - 从远端下载并导入（pull）
//   - 探测远端状态：是否存在、与本地是否一致（status）
//
// WebDAV 凭证（URL、用户名、密码）由 API 层用 AES 加密后存到 settings 表；
// 这里只负责上传/下载业务逻辑。
//
// 冲突处理（避免覆盖丢失）：
//   - push：先 HEAD 远端，若 SHA256 与本地不同则拒绝（由前端弹出确认对话框）
//   - pull：先把本地供应商导出为临时字节流并算 SHA256；
//     若与本地当前 dist 不一致（说明在别处改过），且请求里没有 force=true，也拒绝
//
// 文件格式：与 /api/export 一致（exportFormat = "agoramodel.providers/v1"），
// 便于跨实例交换。
package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config 是 WebDAV 凭证（明文）。
//
// URL 视为「远程根目录」（必填），文件名由包内常量固定（providers.json），不再支持自定义。
type Config struct {
	URL      string `json:"url"`      // 例如 https://dav.example.com/agora/
	Username string `json:"username"` // 可空
	Password string `json:"password"` // 明文，调用方负责保密传输与落库
}

// RemoteFilename 是远端同步文件的名字，写死在包里避免不同机器各自漂移。
//
// 历史：早期版本允许用户自定义；现已统一为单一固定文件，便于在 NAS 上 / 等查询时锁定目标。
const RemoteFilename = "agoramodel-providers.json"

// Client 在 Config 之上封装 HTTP 调用。
type Client struct {
	cfg Config
	hc  *http.Client
}

// New 创建 WebDAV 客户端；timeout <= 0 使用 30s 默认值。
func New(cfg Config, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		cfg: cfg,
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil,
				DisableCompression:  true,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

// ErrConflict 表示远端（或本地临时导出）与期望的不同，需要用户选择。
type ErrConflict struct {
	// RemoteSHA256 是远端的指纹；为空表示远端不存在。
	RemoteSHA256 string
}

func (e *ErrConflict) Error() string {
	if e.RemoteSHA256 == "" {
		return "远端不存在，确认上传？"
	}
	return "远端文件与本地不同（" + e.RemoteSHA256[:8] + "…），请确认是否覆盖"
}

// ErrUnauthorized 表示 401/403，需要用户检查凭证。
var ErrUnauthorized = errors.New("WebDAV 认证失败：请检查用户名与密码")

// target 拼出最终要访问的远端 URL：URL 当目录处理 + 包内常量固定文件名。
func (c *Client) target() (string, error) {
	base := strings.TrimRight(strings.TrimSpace(c.cfg.URL), "/")
	if base == "" {
		return "", errors.New("WebDAV 地址为空")
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("WebDAV 地址不合法: %q", base)
	}
	return base + "/" + RemoteFilename, nil
}

// do 包装 HTTP 请求：自动加 basic auth、超时与错误归一化。
func (c *Client) do(ctx context.Context, method string, body io.Reader, contentType string) (*http.Response, error) {
	target, err := c.target()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if c.cfg.Username != "" || c.cfg.Password != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, ErrUnauthorized
	}
	return resp, nil
}

// ReadRemote 读取远端文件并返回字节流与 SHA256；不存在返回 (nil, "", nil)。
func (c *Client) ReadRemote(ctx context.Context) ([]byte, string, error) {
	resp, err := c.do(ctx, http.MethodGet, nil, "")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return nil, "", fmt.Errorf("读取远端响应失败: %w", err)
		}
		sum := sha256.Sum256(body)
		return body, hex.EncodeToString(sum[:]), nil
	case http.StatusNotFound:
		return nil, "", nil
	default:
		return nil, "", fmt.Errorf("WebDAV GET 返回 %d", resp.StatusCode)
	}
}

// WriteRemote 上传文件；若 expectSHA256 非空，会先 HEAD/GET 校验远端指纹，
// 与本地不一致则返回 *ErrConflict。
func (c *Client) WriteRemote(ctx context.Context, data []byte, expectSHA256 string) error {
	if expectSHA256 != "" {
		remoteBytes, remoteSum, err := c.ReadRemote(ctx)
		if err != nil {
			return err
		}
		if remoteBytes != nil && remoteSum != expectSHA256 {
			return &ErrConflict{RemoteSHA256: remoteSum}
		}
	}

	resp, err := c.do(ctx, http.MethodPut, bytes.NewReader(data), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// 部分 WebDAV 服务端 PUT 返回 201（创建），不在 2xx 范围的话读出错误体帮助排查
	hint, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("WebDAV PUT 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(hint)))
}

// HashBytes 计算字节流的 SHA256（hex 编码）。
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
