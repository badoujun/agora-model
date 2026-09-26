package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Protocol 标识入站与上游的协议风格。
type Protocol string

const (
	// ProtocolOpenAI 对应 /v1/chat/completions。
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAnthropic 对应 /v1/messages。
	ProtocolAnthropic Protocol = "anthropic"
)

// Path 返回该协议在网关对外暴露的路径。
func (p Protocol) Path() string {
	switch p {
	case ProtocolOpenAI:
		return "/v1/chat/completions"
	case ProtocolAnthropic:
		return "/v1/messages"
	}
	return ""
}

// 默认值与上限。
const (
	DefaultTimeoutSeconds = 120
	DefaultPriority       = 100
	DefaultSSEIdleSeconds = 15
	DefaultMaxBodyBytes   = int64(16 << 20) // 16MB
	DefaultListen         = "127.0.0.1"
	DefaultPort           = 9090
)

var (
	// ErrNoAPIKey 表示网关 Key 未配置。
	ErrNoAPIKey = errors.New("gateway.api_key 未配置")
	// ErrNoProviders 表示没有任何启用的供应商。
	ErrNoProviders = errors.New("未配置任何启用的供应商（providers[].enabled 需为 true）")
)

// Provider 是一个上游供应商的接入配置。
//
// Phase 1 由 JSON 引导配置提供；Phase 2 起改由 SQLite 构建快照（字段语义不变）。
type Provider struct {
	ID                        string            `json:"id"`
	Name                      string            `json:"name"`
	OpenAIBaseURL             string            `json:"openai_base_url"`
	AnthropicBaseURL          string            `json:"anthropic_base_url"`
	OpenAIEndpointOverride    string            `json:"openai_endpoint_override"`
	AnthropicEndpointOverride string            `json:"anthropic_endpoint_override"`
	APIKey                    string            `json:"api_key"`
	Models                    []string          `json:"models"`
	Priority                  int               `json:"priority"`
	TimeoutSeconds            int               `json:"timeout_seconds"`
	ExtraHeaders              map[string]string `json:"extra_headers"`
	ExtraBody                 map[string]any    `json:"extra_body"`
	Enabled                   *bool             `json:"enabled"`
}

// IsEnabled 报告该供应商是否启用（缺省视为启用）。
func (p *Provider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Endpoint 返回指定协议下的完整上游 URL；空字符串表示该协议未配置。
func (p *Provider) Endpoint(proto Protocol) string {
	switch proto {
	case ProtocolOpenAI:
		if u := strings.TrimSpace(p.OpenAIEndpointOverride); u != "" {
			return u
		}
		if u := strings.TrimSpace(p.OpenAIBaseURL); u != "" {
			return strings.TrimRight(u, "/") + "/chat/completions"
		}
	case ProtocolAnthropic:
		if u := strings.TrimSpace(p.AnthropicEndpointOverride); u != "" {
			return u
		}
		if u := strings.TrimSpace(p.AnthropicBaseURL); u != "" {
			return strings.TrimRight(u, "/") + "/messages"
		}
	}
	return ""
}

// KnowsModel 报告该供应商是否声明支持某模型。
//
// Phase 1 只看手动模型列表；Phase 3 起改为 (手动 ∪ 自动拉取) − 排除列表。
func (p *Provider) KnowsModel(model string) bool {
	for _, m := range p.Models {
		if m == model {
			return true
		}
	}
	return false
}

// Timeout 返回上游请求超时。
func (p *Provider) Timeout() time.Duration {
	if p.TimeoutSeconds <= 0 {
		return DefaultTimeoutSeconds * time.Second
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// Gateway 是网关自身的对外设置。
type Gateway struct {
	APIKey         string `json:"api_key"`
	Listen         string `json:"listen"`
	Port           int    `json:"port"`
	SSEIdleSeconds int    `json:"sse_idle_seconds"`
	MaxBodyBytes   int64  `json:"max_body_bytes"`
}

// SSEIdle 返回 SSE 空闲心跳阈值。
func (g Gateway) SSEIdle() time.Duration {
	if g.SSEIdleSeconds <= 0 {
		return DefaultSSEIdleSeconds * time.Second
	}
	return time.Duration(g.SSEIdleSeconds) * time.Second
}

// Addr 返回监听地址。
func (g Gateway) Addr() string {
	listen := g.Listen
	if listen == "" {
		listen = DefaultListen
	}
	port := g.Port
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("%s:%d", listen, port)
}

// File 是 JSON 引导配置的顶层结构（Phase 1 专用）。
type File struct {
	Gateway   Gateway    `json:"gateway"`
	Providers []Provider `json:"providers"`
}

// LoadFile 读取、规范化并校验 JSON 引导配置。
func LoadFile(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	return f.Normalize()
}

// Normalize 补齐默认值并校验，返回可直接构建快照的配置。
func (f File) Normalize() (File, error) {
	if f.Gateway.Listen == "" {
		f.Gateway.Listen = DefaultListen
	}
	if f.Gateway.Port == 0 {
		f.Gateway.Port = DefaultPort
	}
	if f.Gateway.SSEIdleSeconds <= 0 {
		f.Gateway.SSEIdleSeconds = DefaultSSEIdleSeconds
	}
	if f.Gateway.MaxBodyBytes <= 0 {
		f.Gateway.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if strings.TrimSpace(f.Gateway.APIKey) == "" {
		return File{}, ErrNoAPIKey
	}

	seen := make(map[string]bool, len(f.Providers))
	enabled := 0
	for i := range f.Providers {
		p := &f.Providers[i]
		p.normalize(i)
		if seen[p.ID] {
			return File{}, fmt.Errorf("供应商 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if err := p.validate(); err != nil {
			return File{}, err
		}
		if p.IsEnabled() {
			enabled++
		}
	}
	if enabled == 0 {
		return File{}, ErrNoProviders
	}
	return f, nil
}

func (p *Provider) normalize(idx int) {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		p.ID = fmt.Sprintf("provider-%d", idx+1)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Priority == 0 {
		p.Priority = DefaultPriority
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if p.Enabled == nil {
		enabled := true
		p.Enabled = &enabled
	}
	// 模型名去空格、去重，保持顺序
	models := make([]string, 0, len(p.Models))
	seen := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		models = append(models, m)
	}
	p.Models = models
	// 端点 base URL 去掉尾部斜杠
	p.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	p.AnthropicBaseURL = strings.TrimRight(strings.TrimSpace(p.AnthropicBaseURL), "/")
	p.OpenAIEndpointOverride = strings.TrimSpace(p.OpenAIEndpointOverride)
	p.AnthropicEndpointOverride = strings.TrimSpace(p.AnthropicEndpointOverride)
}

// validate 做基础校验；SSRF 的 IP 段校验在 Phase 2（T2.5）补充。
func (p *Provider) validate() error {
	for _, f := range []struct {
		field string
		value string
	}{
		{"openai_base_url", p.OpenAIBaseURL},
		{"anthropic_base_url", p.AnthropicBaseURL},
		{"openai_endpoint_override", p.OpenAIEndpointOverride},
		{"anthropic_endpoint_override", p.AnthropicEndpointOverride},
	} {
		if err := validateUpstreamURL(f.value, "providers["+p.ID+"]."+f.field); err != nil {
			return err
		}
	}
	if p.Endpoint(ProtocolOpenAI) == "" && p.Endpoint(ProtocolAnthropic) == "" {
		return fmt.Errorf("providers[%s] 至少需要配置一个协议地址（openai_base_url 或 anthropic_base_url）", p.ID)
	}
	return nil
}

func validateUpstreamURL(raw, field string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s 不是合法 URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s 仅支持 http/https，得到 %q", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s 缺少主机名: %s", field, raw)
	}
	return nil
}
