package security

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var (
	// ErrBadScheme 表示协议不是 http/https。
	ErrBadScheme = errors.New("仅支持 http/https")
	// ErrBadHost 表示 URL 缺少主机名。
	ErrBadHost = errors.New("URL 缺少主机名")
	// ErrInternalAddr 表示解析到了内网 / 回环地址。
	ErrInternalAddr = errors.New("解析到内网或回环地址（如需接入本地服务请显式开启 allow_internal）")
)

// ValidateUpstreamURL 校验供应商地址（SSRF 防护）。
//
// allowInternal 为 true 时跳过 IP 段检查，用于本地 Ollama / vLLM 等内网服务；
// 校验发生在保存配置时，转发路径不重复校验（避免每请求 DNS 开销）。
func ValidateUpstreamURL(raw string, allowInternal bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w：%q", ErrBadScheme, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return ErrBadHost
	}
	if allowInternal {
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败: %w", err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("%w：%s -> %s", ErrInternalAddr, host, ip)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
