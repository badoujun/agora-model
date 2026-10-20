package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/provider"
	"agora-model/internal/security"
	"agora-model/internal/store"
)

// defaultTimeoutSeconds 与 config 包保持一致。
const defaultTimeoutSeconds = 120

// providerKeyDTO 是单个凭证的对外表示（永不回传明文）。
type providerKeyDTO struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	APIKeyHint   string `json:"api_key_hint"`
	Enabled      bool   `json:"enabled"`
	AutoDisabled bool   `json:"auto_disabled"`
	// DisabledReason 仅在自动停用时非空（网关记录的原因，已脱敏）。
	DisabledReason string `json:"disabled_reason,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

// keyStatsDTO 是单个凭证的运行期统计（易变，不参与导出与 WebDAV 指纹）。
type keyStatsDTO struct {
	LastUsedAt      string `json:"last_used_at,omitempty"`
	FailureCount    int64  `json:"failure_count"`
	LastFailureAt   string `json:"last_failure_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	TodayTokens     int64  `json:"today_tokens"`
	Week7Tokens     int64  `json:"week7_tokens"`
	Month30Tokens   int64  `json:"month30_tokens"`
	Month30Requests int64  `json:"month30_requests"`
}

// providerDTO 是对外的供应商表示：凭证只输出掩码与运行期统计。
type providerDTO struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	OpenAIBaseURL          string `json:"openai_base_url"`
	OpenAIEndpointOverride string `json:"openai_endpoint_override"`
	WebsiteURL             string `json:"website_url"`
	// APIKeyHint 是兼容字段：首个启用 Key 的掩码。
	APIKeyHint     string                 `json:"api_key_hint"`
	Keys           []providerKeyDTO       `json:"keys"`
	KeyStats       map[string]keyStatsDTO `json:"key_stats,omitempty"`
	ModelsSelected []string               `json:"models_selected"`
	ModelAliases   map[string]string      `json:"model_aliases"`
	ExposedModels  []string               `json:"exposed_models"`
	TimeoutSeconds int                    `json:"timeout_seconds"`
	ExtraHeaders   map[string]string      `json:"extra_headers,omitempty"`
	ExtraBody      map[string]any         `json:"extra_body,omitempty"`
	AllowInternal  bool                   `json:"allow_internal"`
	Enabled        bool                   `json:"enabled"`
	ModelCount     int                    `json:"model_count"`
	// CandidateModels 是最近一次「拉取模型」得到的结果，供编辑页勾选。
	CandidateModels []string `json:"candidate_models,omitempty"`
	LastFetchAt     string   `json:"last_fetch_at,omitempty"`
	LastFetchError  string   `json:"last_fetch_error,omitempty"`
}

// providerKeyInput 是创建/更新请求里的单个凭证。
//
//   - ID 为空表示新增（必须给明文 api_key）；
//   - ID 非空表示沿用既有记录，api_key 留空即"不修改原密文"。
type providerKeyInput struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	APIKey  *string `json:"api_key"`
	Enabled *bool   `json:"enabled"`
}

// providerInput 是创建/更新供应商的请求体。
//
// 指针字段用于区分「未提供」与「显式置零/置 false」。
type providerInput struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	OpenAIBaseURL          string `json:"openai_base_url"`
	OpenAIEndpointOverride string `json:"openai_endpoint_override"`
	// APIKey 是兼容字段：单凭证的旧请求/旧导出文件仍然可用。
	APIKey *string `json:"api_key"`
	// Keys 为 nil 表示"不改动凭证集合"；非 nil（含空数组）表示全量替换。
	Keys []providerKeyInput `json:"keys"`
	// WebsiteURL 使用指针：nil 表示未提供（沿用既有值），空串表示显式清空。
	WebsiteURL     *string           `json:"website_url"`
	ModelsSelected []string          `json:"models_selected"`
	ModelAliases   map[string]string `json:"model_aliases"`
	TimeoutSeconds *int              `json:"timeout_seconds"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	ExtraBody      map[string]any    `json:"extra_body"`
	AllowInternal  *bool             `json:"allow_internal"`
	Enabled        *bool             `json:"enabled"`
}

func (in providerInput) displayName() string {
	if strings.TrimSpace(in.Name) != "" {
		return strings.TrimSpace(in.Name)
	}
	if strings.TrimSpace(in.ID) != "" {
		return strings.TrimSpace(in.ID)
	}
	return "(未命名)"
}

// exposedModels 返回记录对外暴露的模型名（别名优先）。
func exposedModels(rec store.ProviderRecord) []string {
	out := make([]string, 0, len(rec.ModelsSelected))
	for _, m := range rec.ModelsSelected {
		if alias := strings.TrimSpace(rec.ModelAliases[m]); alias != "" {
			out = append(out, alias)
			continue
		}
		out = append(out, m)
	}
	return out
}

func toKeyDTO(k store.ProviderKeyRecord) providerKeyDTO {
	return providerKeyDTO{
		ID:             k.ID,
		Label:          k.Label,
		APIKeyHint:     k.KeyHint,
		Enabled:        k.Enabled,
		AutoDisabled:   k.AutoDisabled,
		DisabledReason: k.DisabledReason,
		CreatedAt:      formatOptionalTime(k.CreatedAt),
		UpdatedAt:      formatOptionalTime(k.UpdatedAt),
	}
}

// toDTO 把记录转换为对外 DTO；usages 为空表示不填充按 Key 用量（导出路径）。
func toDTO(rec store.ProviderRecord, candidates []string, usages map[string]store.KeyUsageTotals) providerDTO {
	keys := make([]providerKeyDTO, 0, len(rec.Keys))
	stats := make(map[string]keyStatsDTO, len(rec.Keys))
	for _, k := range rec.Keys {
		keys = append(keys, toKeyDTO(k))
		u := usages[k.ID]
		stats[k.ID] = keyStatsDTO{
			LastUsedAt:      formatOptionalTime(k.LastUsedAt),
			FailureCount:    k.FailureCount,
			LastFailureAt:   formatOptionalTime(k.LastFailureAt),
			LastError:       k.LastError,
			TodayTokens:     u.TodayTokens,
			Week7Tokens:     u.Week7Tokens,
			Month30Tokens:   u.Month30Tokens,
			Month30Requests: u.Month30Requests,
		}
	}
	dto := providerDTO{
		ID:                     rec.ID,
		Name:                   rec.Name,
		OpenAIBaseURL:          rec.OpenAIBaseURL,
		OpenAIEndpointOverride: rec.OpenAIEndpointOverride,
		WebsiteURL:             rec.WebsiteURL,
		APIKeyHint:             rec.EnabledKeyHint(),
		Keys:                   keys,
		ModelsSelected:         rec.ModelsSelected,
		ModelAliases:           rec.ModelAliases,
		ExposedModels:          exposedModels(rec),
		TimeoutSeconds:         rec.TimeoutSeconds,
		ExtraHeaders:           rec.ExtraHeaders,
		ExtraBody:              rec.ExtraBody,
		AllowInternal:          rec.AllowInternal,
		Enabled:                rec.Enabled,
		ModelCount:             len(rec.ModelsSelected),
		CandidateModels:        candidates,
		LastFetchAt:            formatOptionalTime(rec.LastFetchAt),
		LastFetchError:         rec.LastFetchError,
	}
	if len(stats) > 0 {
		dto.KeyStats = stats
	}
	return dto
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------- 供应商 ----------------

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	records, err := s.store.ListProviders(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	items := make([]providerDTO, 0, len(records))
	for _, rec := range records {
		candidates, _, cerr := s.store.LoadCandidateModels(ctx, rec.ID)
		if cerr != nil {
			s.logger.Debug("读取候选模型失败", "provider", rec.ID, "err", cerr)
		}
		items = append(items, toDTO(rec, candidates, s.keyUsage(ctx, rec.ID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rec, err := s.store.GetProvider(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	candidates, _, _ := s.store.LoadCandidateModels(ctx, rec.ID)
	writeJSON(w, http.StatusOK, toDTO(rec, candidates, s.keyUsage(ctx, rec.ID)))
}

// keyUsage 读取某供应商按 Key 的用量汇总；失败只记 debug（不影响主响应）。
func (s *Server) keyUsage(ctx context.Context, providerID string) map[string]store.KeyUsageTotals {
	usages, err := s.store.ProviderKeyUsageTotals(ctx, providerID)
	if err != nil {
		s.logger.Debug("读取按 Key 用量失败", "provider", providerID, "err", err)
		return nil
	}
	return usages
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	rec, err := s.upsertProvider(ctx, in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_ = s.applyReload(ctx)

	refreshed, _ := s.store.GetProvider(ctx, rec.ID)
	writeJSON(w, http.StatusCreated, toDTO(refreshed, nil, nil))
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in providerInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if _, err := s.upsertProvider(ctx, in); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_ = s.applyReload(ctx)

	rec, err := s.store.GetProvider(ctx, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	candidates, _, _ := s.store.LoadCandidateModels(ctx, id)
	writeJSON(w, http.StatusOK, toDTO(rec, candidates, s.keyUsage(ctx, id)))
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	err := s.store.DeleteProvider(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "删除供应商失败", err)
		return
	}
	_ = s.applyReload(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	p, err := s.loadProvider(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}

	writeJSON(w, http.StatusOK, provider.Test(ctx, p, s.client))
}

// handleFetchModels 拉取上游模型候选列表并缓存（不影响已勾选的模型）。
func (s *Server) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	p, err := s.loadProvider(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}

	candidates, err := s.fetcher.Fetch(ctx, p)
	if err != nil {
		if serr := s.store.SetProviderFetchError(ctx, p.ID, err.Error()); serr != nil {
			s.logger.Debug("记录拉取失败原因失败", "provider", p.ID, "err", serr)
		}
		rec, _ := s.store.GetProvider(ctx, p.ID)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            err.Error(),
			"last_fetch_error": rec.LastFetchError,
		})
		return
	}
	if err := s.store.ReplaceCandidateModels(ctx, p.ID, candidates, time.Now().UTC()); err != nil {
		s.fail(w, http.StatusInternalServerError, "写入模型候选缓存失败", err)
		return
	}

	rec, err := s.store.GetProvider(ctx, p.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(rec, candidates, s.keyUsage(ctx, p.ID)))
}

// draftKeyInput 是草稿（尚未保存）供应商的一个凭证。
type draftKeyInput struct {
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
}

// draftProviderInput 是「尚未保存的供应商」拉取模型 / 测试连接的请求体。
//
// 与保存接口分开：明文凭证只用于这一次探测，不落库。
type draftProviderInput struct {
	OpenAIBaseURL          string            `json:"openai_base_url"`
	OpenAIEndpointOverride string            `json:"openai_endpoint_override"`
	APIKey                 string            `json:"api_key"` // 兼容旧表单：单 Key
	Keys                   []draftKeyInput   `json:"keys"`
	ExtraHeaders           map[string]string `json:"extra_headers"`
	TimeoutSeconds         int               `json:"timeout_seconds"`
	AllowInternal          bool              `json:"allow_internal"`
	// ModelsSelected 只用于连接测试的最小对话探测（/models 不可用时）。
	ModelsSelected []string `json:"models_selected"`
}

// draftProvider 把草稿表单转换为运行时配置（按顺序尝试所有有明文的启用 Key）。
func (in draftProviderInput) draftProvider() (config.Provider, error) {
	base := strings.TrimSpace(in.OpenAIBaseURL)
	override := strings.TrimSpace(in.OpenAIEndpointOverride)
	if base == "" && override == "" {
		return config.Provider{}, errors.New("请先填写 OpenAI Base URL，再继续")
	}
	// 与保存路径一致：SSRF 校验在发出请求前完成
	for _, u := range []string{base, override} {
		if err := security.ValidateUpstreamURL(u, in.AllowInternal); err != nil {
			return config.Provider{}, err
		}
	}

	var keys []config.ProviderKey
	for i, k := range in.Keys {
		key := strings.TrimSpace(k.APIKey)
		if key == "" || (k.Enabled != nil && !*k.Enabled) {
			continue
		}
		enabled := true
		label := strings.TrimSpace(k.Label)
		if label == "" {
			label = fmt.Sprintf("Key %d", i+1)
		}
		keys = append(keys, config.ProviderKey{Label: label, Key: key, Enabled: &enabled})
	}
	if len(keys) == 0 && strings.TrimSpace(in.APIKey) != "" {
		enabled := true
		keys = append(keys, config.ProviderKey{Label: "默认", Key: strings.TrimSpace(in.APIKey), Enabled: &enabled})
	}
	if len(keys) == 0 {
		return config.Provider{}, errors.New("请先填写至少一个 API Key")
	}

	timeout := in.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}
	draft := config.Provider{
		ID:                     "draft",
		Name:                   "draft",
		OpenAIBaseURL:          base,
		OpenAIEndpointOverride: override,
		APIKeys:                keys,
		Models:                 cleanList(in.ModelsSelected),
		ExtraHeaders:           in.ExtraHeaders,
		TimeoutSeconds:         timeout,
		AllowInternal:          in.AllowInternal,
	}
	draft.CleanSelection()
	return draft, nil
}

// handleFetchDraftModels 在供应商尚未保存时，用表单里填写的信息直接拉取上游模型候选。
//
// 结果不落库：用户在编辑页勾选后再点保存即可（保存接口负责持久化勾选结果）。
func (s *Server) handleFetchDraftModels(w http.ResponseWriter, r *http.Request) {
	var in draftProviderInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	draft, err := in.draftProvider()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if draft.ModelsEndpoint() == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "无法推导 /models 地址：请填写 OpenAI Base URL（端点覆盖模式下不支持拉取模型）",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	candidates, err := s.fetcher.Fetch(ctx, draft)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidate_models": candidates,
		"fetched_at":       time.Now().UTC().Format(time.RFC3339),
	})
}

// handleTestDraftKey 在供应商尚未保存时测试表单里填写的 Key（逐个尝试，首个成功即返回）。
func (s *Server) handleTestDraftKey(w http.ResponseWriter, r *http.Request) {
	var in draftProviderInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	draft, err := in.draftProvider()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, provider.Test(ctx, draft, s.client))
}

// handleTestProviderKey 只测试某个已保存的 Key（含已停用的 Key，便于判断能否重新启用）。
func (s *Server) handleTestProviderKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	providerID := r.PathValue("id")
	keyID := r.PathValue("key_id")

	p, err := s.loadProvider(ctx, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	rec, err := s.store.GetProviderKey(ctx, providerID, keyID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "该供应商没有这个 API Key"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 API Key 失败", err)
		return
	}
	plain, derr := crypto.Decrypt(s.master, rec.KeyCipher, providerID)
	if derr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("该 API Key 的密文无法用当前主密钥解密（%s）：请重新填写该 Key", rec.KeyHint),
		})
		return
	}
	enabled := true
	key := config.ProviderKey{ID: rec.ID, Label: rec.Label, Key: string(plain), Enabled: &enabled}
	writeJSON(w, http.StatusOK, provider.TestKey(ctx, p, key, s.client))
}

// defaultProviderKeyLabel 是旧单凭证请求转换出来的 Key 的默认备注名。
const defaultProviderKeyLabel = "默认"

// resolveProviderKeys 把请求体解析为最终要保存的凭证列表。
//
//   - Keys 非 nil：全量替换（新增必须给明文；带 id 且明文为空表示沿用库中密文）；
//   - Keys 为 nil 且给了 api_key：等价于"替换为单个凭证"（兼容旧客户端/旧文件）；
//   - 都没有且供应商已存在：沿用现有凭证（不触碰密文）；
//   - 都没有且是新建：报错。
func resolveProviderKeys(in providerInput, existing store.ProviderRecord, hasExisting bool) ([]config.ProviderKey, error) {
	switch {
	case in.Keys != nil:
		out := make([]config.ProviderKey, 0, len(in.Keys))
		for i, k := range in.Keys {
			id := strings.TrimSpace(k.ID)
			key := ""
			if k.APIKey != nil {
				key = strings.TrimSpace(*k.APIKey)
			}
			enabled := k.Enabled == nil || *k.Enabled
			if id == "" && key == "" {
				return nil, fmt.Errorf("第 %d 个 API Key 为空：新增的 Key 必须填写明文", i+1)
			}
			out = append(out, config.ProviderKey{
				ID:      id,
				Label:   strings.TrimSpace(k.Label),
				Key:     key,
				Enabled: &enabled,
			})
		}
		if len(out) == 0 {
			return nil, errors.New("至少需要一个 API Key")
		}
		return out, nil
	case in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "":
		enabled := true
		return []config.ProviderKey{{
			Label:   defaultProviderKeyLabel,
			Key:     strings.TrimSpace(*in.APIKey),
			Enabled: &enabled,
		}}, nil
	case hasExisting:
		out := make([]config.ProviderKey, 0, len(existing.Keys))
		for _, k := range existing.Keys {
			enabled := k.Enabled
			out = append(out, config.ProviderKey{ID: k.ID, Label: k.Label, Enabled: &enabled})
		}
		if len(out) == 0 {
			return nil, errors.New("该供应商没有 API Key，请至少添加一个")
		}
		return out, nil
	default:
		return nil, errors.New("创建供应商必须提供至少一个 API Key")
	}
}

// upsertProvider 把请求体转换为供应商并写库（未提供凭证时沿用既有凭证）。
func (s *Server) upsertProvider(ctx context.Context, in providerInput) (store.ProviderRecord, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = newProviderID()
	}
	in.ID = id

	existing, err := s.store.GetProvider(ctx, id)
	hasExisting := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.ProviderRecord{}, err
	}

	keys, err := resolveProviderKeys(in, existing, hasExisting)
	if err != nil {
		return store.ProviderRecord{}, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" && hasExisting {
		name = existing.Name
	}
	if name == "" {
		return store.ProviderRecord{}, errors.New("请填写供应商名称")
	}
	if err := s.ensureNameAvailable(ctx, id, name); err != nil {
		return store.ProviderRecord{}, err
	}

	website := ""
	if in.WebsiteURL != nil {
		website = strings.TrimSpace(*in.WebsiteURL)
	} else if hasExisting {
		website = existing.WebsiteURL
	}

	base := config.Provider{
		ID:                     id,
		Name:                   name,
		OpenAIBaseURL:          strings.TrimSpace(in.OpenAIBaseURL),
		OpenAIEndpointOverride: strings.TrimSpace(in.OpenAIEndpointOverride),
		WebsiteURL:             website,
		APIKeys:                keys,
		Models:                 cleanList(in.ModelsSelected),
		ModelAliases:           in.ModelAliases,
		TimeoutSeconds:         valueOr(in.TimeoutSeconds, defaultTimeoutSeconds),
		ExtraHeaders:           in.ExtraHeaders,
		ExtraBody:              in.ExtraBody,
		AllowInternal:          boolOr(in.AllowInternal, false),
		Enabled:                in.Enabled,
	}
	if hasExisting {
		// 未提供的字段沿用既有值（PUT 语义贴近 PATCH，降低 Web UI 表单漏传导致配置丢失的风险）
		if base.OpenAIBaseURL == "" {
			base.OpenAIBaseURL = existing.OpenAIBaseURL
		}
		if base.OpenAIEndpointOverride == "" {
			base.OpenAIEndpointOverride = existing.OpenAIEndpointOverride
		}
		if in.ModelsSelected == nil {
			base.Models = existing.ModelsSelected
		}
		if in.ModelAliases == nil {
			base.ModelAliases = existing.ModelAliases
		}
		if in.AllowInternal == nil {
			base.AllowInternal = existing.AllowInternal
		}
		if in.Enabled == nil {
			enabled := existing.Enabled
			base.Enabled = &enabled
		}
	}
	if base.Endpoint() == "" {
		return store.ProviderRecord{}, errors.New("请填写 OpenAI Base URL（或 OpenAI 端点覆盖）")
	}
	if err := base.ValidateWebsiteURL(); err != nil {
		return store.ProviderRecord{}, err
	}
	if err := base.ValidateModelSelection(); err != nil {
		return store.ProviderRecord{}, err
	}

	// SaveProvider 内部会做 SSRF 校验（allow_internal=false 时拒绝内网地址）、
	// 凭证结构校验与 AES 加密，并在同一事务里全量替换凭证集合。
	return s.store.SaveProvider(ctx, s.master, base)
}

// ensureNameAvailable 保证供应商名称唯一：名称即对外路由命名空间（忽略大小写）。
func (s *Server) ensureNameAvailable(ctx context.Context, id, name string) error {
	records, err := s.store.ListProviders(ctx)
	if err != nil {
		return err
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for _, rec := range records {
		if rec.ID == id {
			continue
		}
		if strings.ToLower(strings.TrimSpace(rec.Name)) == want {
			return fmt.Errorf("供应商名称 %q 已被使用，请换一个（名称用于 %s 形式的模型命名空间）", name, name+"/模型名")
		}
	}
	return nil
}

// loadProvider 读出并解密单个供应商（只含启用的 Key）。
func (s *Server) loadProvider(ctx context.Context, id string) (config.Provider, error) {
	return s.store.LoadProvider(ctx, s.master, id)
}

// ---------------- 模型 ----------------

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Model        string `json:"model"`
		Upstream     string `json:"upstream_model"`
		Alias        string `json:"alias,omitempty"`
		ProviderID   string `json:"provider_id"`
		ProviderName string `json:"provider_name"`
		Default      bool   `json:"default"`
	}

	snap := s.holder.Get()
	defaults := map[string]bool{} // 对外模型名 -> 是否已确定默认供应商
	items := make([]entry, 0, 32)
	for _, p := range snap.Providers() {
		for _, model := range p.Models {
			exposed := p.ExposedModel(model)
			item := entry{
				Model:        exposed,
				Upstream:     model,
				ProviderID:   p.ID,
				ProviderName: p.Name,
			}
			if !defaults[exposed] {
				defaults[exposed] = true
				item.Default = true
			}
			if exposed != model {
				item.Alias = exposed
			}
			items = append(items, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---------------- 设置与网关 Key ----------------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	settings, err := s.store.AllSettings(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取设置失败", err)
		return
	}
	gw := s.holder.Get().Gateway()
	keyInfo, err := s.store.ActiveGatewayKey(ctx)
	if err != nil && !errors.Is(err, store.ErrNoGatewayKey) {
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 失败", err)
		return
	}
	// 明文用于控制台的「查看/复制」：库中以 AES 密文保存，明文只在同源控制台响应里出现。
	plainKey, revealErr := s.store.RevealGatewayKey(ctx, s.master)
	revealable := revealErr == nil
	switch {
	case revealErr == nil, errors.Is(revealErr, store.ErrNoGatewayKey):
		// 正常：要么取到明文，要么尚未生成网关 Key
	case store.IsGatewayKeyUnrevealable(revealErr):
		// 旧记录只存哈希，或密文与当前主密钥不匹配：页面提示「重置」即可，不算接口故障
		if errors.Is(revealErr, store.ErrGatewayKeyDecryptFailed) {
			s.logger.Warn("网关 Key 明文无法解密：主密钥可能与数据库不匹配，控制台会提示重置", "err", revealErr)
		}
	default:
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 失败", revealErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":                 gw.Listen,
		"port":                   gw.Port,
		"sse_idle_seconds":       gw.SSEIdleSeconds,
		"max_body_bytes":         gw.MaxBodyBytes,
		"gateway_key_hint":       keyInfo.KeyHint,
		"gateway_key":            plainKey,
		"gateway_key_revealable": revealable,
		"gateway_key_created_at": formatOptionalTime(keyInfo.CreatedAt),
		"gateway_key_last_used":  formatOptionalTime(keyInfo.LastUsedAt),
		"log_success":            settings[store.SettingLogSuccess],
	})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LogSuccess *bool `json:"log_success"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if body.LogSuccess != nil {
		value := "false"
		if *body.LogSuccess {
			value = "true"
		}
		if err := s.store.SetSetting(ctx, store.SettingLogSuccess, value); err != nil {
			s.fail(w, http.StatusInternalServerError, "写入设置失败", err)
			return
		}
	}
	s.handleGetSettings(w, r)
}

func (s *Server) handleGetGatewayKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	info, err := s.store.ActiveGatewayKey(ctx)
	if errors.Is(err, store.ErrNoGatewayKey) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "尚未生成网关 Key"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           info.ID,
		"key_hint":     info.KeyHint,
		"created_at":   formatOptionalTime(info.CreatedAt),
		"last_used_at": formatOptionalTime(info.LastUsedAt),
	})
}

func (s *Server) handleResetGatewayKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	plain, err := s.store.ResetGatewayKey(ctx, s.master)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "重置网关 Key 失败", err)
		return
	}
	s.logger.Warn("Web UI 重置了网关 Key：旧 Key 已失效", "hint", crypto.Mask(plain))
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_key": plain,
		"key_hint":    crypto.Mask(plain),
		"warning":     "已生成新的网关 Key；可在本页随时查看或复制当前 Key",
	})
}

// ---------------- 日志 ----------------

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	q := r.URL.Query()
	filter := store.LogFilter{
		Model:      strings.TrimSpace(q.Get("model")),
		ProviderID: strings.TrimSpace(q.Get("provider_id")),
		FailedOnly: q.Get("failed") == "true" || q.Get("failed") == "1",
		StatusMin:  atoiOr(q.Get("status_min"), 0),
		StatusMax:  atoiOr(q.Get("status_max"), 0),
		Limit:      atoiOr(q.Get("limit"), 200),
		Offset:     atoiOr(q.Get("offset"), 0),
	}
	if from := strings.TrimSpace(q.Get("from")); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			filter.From = t
		}
	}
	if to := strings.TrimSpace(q.Get("to")); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			filter.To = t
		}
	}

	items, err := s.store.QueryLogs(ctx, filter)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "查询日志失败", err)
		return
	}
	total, err := s.store.CountLogs(ctx)
	if err != nil {
		s.logger.Debug("统计日志条数失败", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// ---------------- 工具函数 ----------------

func cleanList(in []string) []string {
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

func valueOr(v *int, fallback int) int {
	if v == nil || *v == 0 {
		return fallback
	}
	return *v
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func atoiOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}
