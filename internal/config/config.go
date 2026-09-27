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

// ChatCompletionsPath 是网关对外暴露的端点（OpenAI 兼容协议）。
const ChatCompletionsPath = "/v1/chat/completions"

// ModelsPath 是网关对外暴露聚合模型列表的端点。
const ModelsPath = "/v1/models"

// ProtocolName 是网关唯一对外协议的名称（OpenAI 兼容），用于请求日志展示。
const ProtocolName = "openai"

// 默认值与上限。
const (
	DefaultTimeoutSeconds = 120
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

// Provider 是一个上游供应商的接入配置（OpenAI 兼容协议）。
//
// Phase 1 由 JSON 引导配置提供；Phase 2 起改由 SQLite 构建快照（字段语义不变）。
type Provider struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	OpenAIBaseURL          string `json:"openai_base_url"`
	OpenAIEndpointOverride string `json:"openai_endpoint_override"`
	// WebsiteURL 是供应商官网地址，仅用于在控制台展示与跳转，不参与转发。
	WebsiteURL string `json:"website_url"`
	APIKey     string `json:"api_key"`
	// Models 是**已启用**的上游模型名（在供应商编辑页从拉取结果中勾选），顺序即展示顺序。
	Models []string `json:"models"`
	// ModelAliases 把上游模型名映射为对外别名；别名即 Agent 请求时使用的模型名。
	ModelAliases   map[string]string `json:"model_aliases"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	ExtraBody      map[string]any    `json:"extra_body"`
	AllowInternal  bool              `json:"allow_internal"`
	Enabled        *bool             `json:"enabled"`
}

// IsEnabled 报告该供应商是否启用（缺省视为启用）。
func (p *Provider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Endpoint 返回上游对话接口的完整 URL；空字符串表示未配置。
func (p *Provider) Endpoint() string {
	if u := strings.TrimSpace(p.OpenAIEndpointOverride); u != "" {
		return u
	}
	if u := strings.TrimSpace(p.OpenAIBaseURL); u != "" {
		return strings.TrimRight(u, "/") + "/chat/completions"
	}
	return ""
}

// ModelsEndpoint 返回上游模型列表接口的 URL；端点被覆盖或未配置 base URL 时返回空。
func (p *Provider) ModelsEndpoint() string {
	if strings.TrimSpace(p.OpenAIEndpointOverride) != "" {
		// 覆盖了完整对话端点，无法可靠推导 /models 地址
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/models"
}

// ExposedModel 返回模型对外暴露的名称：配置了别名时用别名，否则用上游模型名。
func (p *Provider) ExposedModel(model string) string {
	if alias := strings.TrimSpace(p.ModelAliases[model]); alias != "" {
		return alias
	}
	return model
}

// RealModel 把对外模型名（裸名或别名）解析为上游真实模型名。
func (p *Provider) RealModel(exposed string) (string, bool) {
	exposed = strings.TrimSpace(exposed)
	if exposed == "" {
		return "", false
	}
	for _, m := range p.Models {
		if p.ExposedModel(m) == exposed {
			return m, true
		}
	}
	return "", false
}

// KnowsModel 报告该供应商是否对外提供某模型（裸名或别名）。
func (p *Provider) KnowsModel(exposed string) bool {
	_, ok := p.RealModel(exposed)
	return ok
}

// ExposedModels 返回该供应商对外暴露的模型名列表（保持勾选顺序）。
func (p *Provider) ExposedModels() []string {
	out := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, p.ExposedModel(m))
	}
	return out
}

// CleanSelection 规整模型选择与别名：去空白、去重，并丢弃未勾选模型的别名。
func (p *Provider) CleanSelection() {
	p.Models = cleanStrings(p.Models)
	if len(p.ModelAliases) == 0 {
		p.ModelAliases = nil
		return
	}
	selected := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		selected[m] = true
	}
	aliases := make(map[string]string, len(p.ModelAliases))
	for model, alias := range p.ModelAliases {
		model = strings.TrimSpace(model)
		alias = strings.TrimSpace(alias)
		if model == "" || alias == "" || !selected[model] {
			continue
		}
		aliases[model] = alias
	}
	if len(aliases) == 0 {
		p.ModelAliases = nil
		return
	}
	p.ModelAliases = aliases
}

// ValidateModelSelection 校验勾选的模型与别名的合法性。
func (p *Provider) ValidateModelSelection() error {
	seen := make(map[string]string, len(p.Models))
	for _, m := range p.Models {
		exposed := p.ExposedModel(m)
		key := normalizeName(exposed)
		if key == "" {
			return fmt.Errorf("供应商 %s 的模型名不能为空", p.displayRef())
		}
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("供应商 %s 的模型 %q 与 %q 对外名称重复（别名冲突）", p.displayRef(), m, prev)
		}
		seen[key] = m
	}
	return nil
}

func (p *Provider) displayRef() string {
	if strings.TrimSpace(p.Name) != "" {
		return strings.TrimSpace(p.Name)
	}
	return p.ID
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

// ValidateStaticAPIKey 校验引导配置中的静态网关 Key。
//
// Phase 1 的 JSON 直连模式需要它；Phase 2 起网关 Key 由 gateway_keys 表管理，
// 因此该校验不再出现在 Normalize 中。
func (f File) ValidateStaticAPIKey() error {
	if strings.TrimSpace(f.Gateway.APIKey) == "" {
		return ErrNoAPIKey
	}
	return nil
}

// Normalize 补齐默认值并校验，返回可直接构建快照的配置。
//
// 未配置任何启用的供应商时返回 ErrNoProviders —— 这是**校验用户提供的引导配置**时
// 的期望行为：写了 providers 却全部停用，几乎一定是配错了。
// 运行时快照请用 NormalizeAllowEmpty。
func (f File) Normalize() (File, error) {
	return f.normalize(true)
}

// NormalizeAllowEmpty 与 Normalize 相同，但允许一个启用的供应商都没有。
//
// 运行时快照用这个：全新安装时数据库本来就是空的，用户必须先能启动网关，
// 才能通过 Web UI 添加第一个供应商；同理，删掉最后一个供应商也不该失败。
func (f File) NormalizeAllowEmpty() (File, error) {
	return f.normalize(false)
}

func (f File) normalize(requireProviders bool) (File, error) {
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
	seenIDs := make(map[string]bool, len(f.Providers))
	seenNames := make(map[string]string, len(f.Providers))
	enabled := 0
	for i := range f.Providers {
		p := &f.Providers[i]
		p.normalize(i)
		if seenIDs[p.ID] {
			return File{}, fmt.Errorf("供应商 id 重复: %s", p.ID)
		}
		seenIDs[p.ID] = true
		key := normalizeName(p.Name)
		if prev, dup := seenNames[key]; dup {
			return File{}, fmt.Errorf("%w：%q 与 %s 重名", ErrProviderNameConflict, p.Name, prev)
		}
		seenNames[key] = p.Name
		if err := p.validate(); err != nil {
			return File{}, err
		}
		if err := p.ValidateModelSelection(); err != nil {
			return File{}, err
		}
		if p.IsEnabled() {
			enabled++
		}
	}
	if enabled == 0 && requireProviders {
		return File{}, ErrNoProviders
	}
	return f, nil
}

// ErrProviderNameConflict 表示供应商名称重复（名称即路由命名空间，必须唯一）。
var ErrProviderNameConflict = errors.New("供应商名称重复")

// normalizeName 归一化用于比较的名称：去首尾空白 + 忽略大小写。
func normalizeName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// cleanStrings 去空白、去空项、去重，保持原有顺序。
func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
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
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if p.Enabled == nil {
		enabled := true
		p.Enabled = &enabled
	}
	// 模型名与别名去空格、去空项，保持顺序
	p.CleanSelection()
	p.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	p.OpenAIEndpointOverride = strings.TrimSpace(p.OpenAIEndpointOverride)
	p.WebsiteURL = strings.TrimSpace(p.WebsiteURL)
}

// ValidateWebsiteURL 校验供应商官网地址：为空合法，非空必须是 http/https URL。
//
// 官网地址只用于控制台展示与跳转，不参与出站请求，因此不做 SSRF（内网 IP）校验。
func (p *Provider) ValidateWebsiteURL() error {
	if strings.TrimSpace(p.WebsiteURL) == "" {
		return nil
	}
	return validateUpstreamURL(p.WebsiteURL, "providers["+p.ID+"].website_url")
}

// validate 做基础校验；SSRF 的 IP 段校验在 store 保存时补充。
func (p *Provider) validate() error {
	for _, f := range []struct {
		field string
		value string
	}{
		{"openai_base_url", p.OpenAIBaseURL},
		{"openai_endpoint_override", p.OpenAIEndpointOverride},
	} {
		if err := validateUpstreamURL(f.value, "providers["+p.ID+"]."+f.field); err != nil {
			return err
		}
	}
	if err := p.ValidateWebsiteURL(); err != nil {
		return err
	}
	if p.Endpoint() == "" {
		return fmt.Errorf("providers[%s] 必须配置 openai_base_url 或 openai_endpoint_override", p.ID)
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
