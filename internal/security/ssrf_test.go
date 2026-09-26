package security

import (
	"errors"
	"net"
	"testing"
)

func TestValidateUpstreamURLAcceptsPublicAndEmpty(t *testing.T) {
	// 直接给公网 IP，避免测试依赖 DNS
	if err := ValidateUpstreamURL("https://8.8.8.8/v1", false); err != nil {
		t.Fatalf("公网地址应通过: %v", err)
	}
	if err := ValidateUpstreamURL("", false); err != nil {
		t.Fatalf("空地址应视为未配置并通过: %v", err)
	}
	if err := ValidateUpstreamURL("   ", false); err != nil {
		t.Fatalf("空白地址应通过: %v", err)
	}
}

func TestValidateUpstreamURLRejectsInternal(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:9999/v1",
		"http://10.1.2.3/v1",
		"http://192.168.1.10:8000/v1",
		"http://169.254.1.1/v1",
		"http://[::1]:9999/v1",
		"http://0.0.0.0:8080/v1",
	}
	for _, raw := range cases {
		err := ValidateUpstreamURL(raw, false)
		if !errors.Is(err, ErrInternalAddr) {
			t.Errorf("ValidateUpstreamURL(%q) 期望 ErrInternalAddr，得到 %v", raw, err)
		}
	}
}

func TestValidateUpstreamURLAllowInternal(t *testing.T) {
	if err := ValidateUpstreamURL("http://127.0.0.1:11434/v1", true); err != nil {
		t.Fatalf("allow_internal 应放行内网地址: %v", err)
	}
}

func TestValidateUpstreamURLBadSchemeAndHost(t *testing.T) {
	if err := ValidateUpstreamURL("ftp://example.com/v1", false); !errors.Is(err, ErrBadScheme) {
		t.Fatalf("非 http(s) 应报 ErrBadScheme，得到 %v", err)
	}
	if err := ValidateUpstreamURL("http:///v1", false); err == nil {
		t.Fatal("缺少主机名应报错")
	}
}

func TestIsBlockedIPCoversPrivateRanges(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "::1", "fe80::1"}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试数据非法: %s", s)
		}
		if !isBlockedIP(ip) {
			t.Errorf("%s 应被判定为受保护地址", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试数据非法: %s", s)
		}
		if isBlockedIP(ip) {
			t.Errorf("%s 不应被判定为受保护地址", s)
		}
	}
}
