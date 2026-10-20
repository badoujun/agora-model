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

// MaxProviderKeyLabelLen 是单个 API Key 备注名的最大长度（按 rune 计）。
//
// Key 数量刻意**不设上限**：一个供应商可以配置任意多个凭证。
const MaxProviderKeyLabelLen = 64

// ProviderKey 是供应商的一个上游凭证。
//
// 一个供应商可以配置任意多个 Key：数据面按顺序轮询，凭证类失败时自动切换，
// 并把失败的 Key 自动停用（见 docs/DESIGN.md §7.10）。
type ProviderKey struct {
	// ID 是已有 Key 的记录 id；新增时留空，由存储层生成（pk_<hex>）。
	ID string `json:"id,omitempty"`
	// Label 是备注名（如「主账号」「备用1」），仅用于展示。
	Label string `json:"label"`
	// Key 是明文凭证。编辑既有 Key 时留空表示"沿用原密文"。
	Key string `json:"api_key"`
	// Enabled 为 nil 时视为启用。
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled 报告该 Key 是否启用（缺省视为启用）。
func (k ProviderKey) IsEnabled() bool { return k.Enabled == nil || *k.Enabled }

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
	// APIKey 是**兼容字段**：只用于读取旧的引导配置 / 旧导出文件。
	// 归一化时会折叠进 APIKeys，运行期（路由、转发、连接测试）一律不再读取它。
	APIKey string `json:"api_key"`
	// APIKeys 是该供应商的凭证列表；顺序即轮询顺序与展示顺序。
	APIKeys []ProviderKey `json:"keys"`
	// Models 是**已启用**的上游模型名（在供应商编辑页从拉取结果中勾选），顺序即展示顺序。
	Models []string `json:"models"`
	// ModelAliases 把上游模型名映射为对外别名；别名即 Agent 请求时使用的模型名。
	ModelAliases   map[string]string `json:"model_aliases"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	ExtraBody      map[string]any    `json:"extra_body"`
	AllowInternal  bool              `json:"allow_internal"`
	Enabled        *bool             `json:"enabled"`

	// usableKeys 是 CleanKeys 预计算的「启用且有明文」的 Key 切片。
	//
	// 数据面热路径直接读它，避免每个请求都重新过滤/分配（DESIGN §7.10）。
	// 它随 Provider 值一起进入不可变快照，运行期不再变化。
	usableKeys []ProviderKey
	// keysReady 表示 usableKeys 已经算过（CleanKeys 之后为 true）。
	// 未归一化的临时 Provider（测试/草稿）在 UsableKeys 里走冷路径现算。
	keysReady bool
}

// IsEnabled 报告该供应商是否启用（缺省视为启用）。
func (p *Provider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// UsableKeys 返回该供应商可用于转发的 Key（顺序即轮询顺序）。
//
// 归一化过的 Provider 返回预计算切片（热路径零分配，调用方**不得修改**）；
// 未归一化的临时对象（测试/草稿）现算一次，不写接收者以免并发读写竞争。
func (p *Provider) UsableKeys() []ProviderKey {
	if p == nil {
		return nil
	}
	if !p.keysReady {
		// 未归一化的临时对象：现算一次，并兼容只填了旧 api_key 字段的调用方。
		keys := p.APIKeys
		if len(keys) == 0 && strings.TrimSpace(p.APIKey) != "" {
			enabled := true
			keys = []ProviderKey{{Label: "默认", Key: strings.TrimSpace(p.APIKey), Enabled: &enabled}}
		}
		return enabledKeys(keys)
	}
	return p.usableKeys
}

// HasUsableKey 报告该供应商是否有至少一个可用于转发的 Key。
func (p *Provider) HasUsableKey() bool { return len(p.UsableKeys()) > 0 }

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
	p.CleanKeys()
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

// CleanKeys 规整凭证列表：
//   - 旧版单凭证字段 api_key 折叠为唯一的一个 Key（label「默认」），之后清空该字段；
//   - 去首尾空白；丢弃「既没有 id 也没有明文」的空占位行（UI 新增后未填写）；
//   - 重算热路径使用的 usableKeys（启用且有明文）。
func (p *Provider) CleanKeys() {
	in := p.APIKeys
	if len(in) == 0 && strings.TrimSpace(p.APIKey) != "" {
		enabled := true
		in = []ProviderKey{{Label: "默认", Key: strings.TrimSpace(p.APIKey), Enabled: &enabled}}
	}
	// 运行期不再读取旧字段：归一化后它一定是空的。
	p.APIKey = ""

	out := make([]ProviderKey, 0, len(in))
	for _, k := range in {
		k.ID = strings.TrimSpace(k.ID)
		k.Label = strings.TrimSpace(k.Label)
		k.Key = strings.TrimSpace(k.Key)
		if k.ID == "" && k.Key == "" {
			continue
		}
		out = append(out, k)
	}
	p.APIKeys = out
	p.recomputeUsableKeys()
}

// recomputeUsableKeys 重算数据面热路径使用的 Key 切片（顺序即轮询顺序）。
//
// 返回的切片由 CleanKeys 一次性构建、之后不再修改；请求路径只读，零分配。
func (p *Provider) recomputeUsableKeys() {
	p.usableKeys = enabledKeys(p.APIKeys)
	p.keysReady = true
}

// enabledKeys 过滤出「启用且有明文」的 Key（保持顺序）。
func enabledKeys(keys []ProviderKey) []ProviderKey {
	var out []ProviderKey
	for _, k := range keys {
		if k.IsEnabled() && k.Key != "" {
			out = append(out, k)
		}
	}
	return out
}

// ValidateKeys 校验凭证列表的结构合法性（不检查「至少一个启用」——
// 那条规则需要知道库里已有凭证，见 store.SaveProvider）。
func (p *Provider) ValidateKeys() error {
	if len(p.APIKeys) == 0 {
		return fmt.Errorf("供应商 %s 至少需要一个 API Key", p.displayRef())
	}
	seen := make(map[string]int, len(p.APIKeys))
	for i, k := range p.APIKeys {
		if n := len([]rune(k.Label)); n > MaxProviderKeyLabelLen {
			return fmt.Errorf("供应商 %s 的第 %d 个 API Key 备注名过长（%d 个字符，最多 %d 个）",
				p.displayRef(), i+1, n, MaxProviderKeyLabelLen)
		}
		if k.Key == "" {
			if k.ID == "" {
				return fmt.Errorf("供应商 %s 的第 %d 个 API Key 为空", p.displayRef(), i+1)
			}
			continue // 有 id 无明文：表示沿用库中密文，由存储层校验归属
		}
		if prev, dup := seen[k.Key]; dup {
			return fmt.Errorf("供应商 %s 的第 %d 个 API Key 与第 %d 个重复", p.displayRef(), i+1, prev)
		}
		seen[k.Key] = i + 1
	}
	return nil
}

// ValidateEnabledKeys 校验至少存在一个启用的 Key。
//
// 仅在「凭证明文都在手上的场景」使用（引导配置、运行时快照）；
// 保存路径需要先由存储层把「有 id 无明文」解析为库中密文，再判断。
func (p *Provider) ValidateEnabledKeys() error {
	for _, k := range p.APIKeys {
		if k.IsEnabled() {
			return nil
		}
	}
	return fmt.Errorf("供应商 %s 至少需要启用一个 API Key（如需整体停用请停用供应商）", p.displayRef())
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
		if err := p.ValidateKeys(); err != nil {
			return File{}, err
		}
		if requireProviders {
			// 引导配置由人手写：要求每个供应商确实有可用凭证，避免把空配置带进库。
			// 运行时快照不强校验（自动停用可能让某供应商暂时没有启用 Key，见 DESIGN §7.10）。
			if err := p.ValidateEnabledKeys(); err != nil {
				return File{}, err
			}
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
