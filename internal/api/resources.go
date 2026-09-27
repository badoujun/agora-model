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
	"agora-model/internal/store"
)

// defaultTimeoutSeconds 与 config 包保持一致。
const defaultTimeoutSeconds = 120

// providerDTO 是对外的供应商表示：凭证只输出掩码。
type providerDTO struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	OpenAIBaseURL          string            `json:"openai_base_url"`
	OpenAIEndpointOverride string            `json:"openai_endpoint_override"`
	APIKeyHint             string            `json:"api_key_hint"`
	ModelsSelected         []string          `json:"models_selected"`
	ModelAliases           map[string]string `json:"model_aliases"`
	ExposedModels          []string          `json:"exposed_models"`
	TimeoutSeconds         int               `json:"timeout_seconds"`
	ExtraHeaders           map[string]string `json:"extra_headers,omitempty"`
	ExtraBody              map[string]any    `json:"extra_body,omitempty"`
	AllowInternal          bool              `json:"allow_internal"`
	Enabled                bool              `json:"enabled"`
	ModelCount             int               `json:"model_count"`
	// CandidateModels 是最近一次「拉取模型」得到的结果，供编辑页勾选。
	CandidateModels []string `json:"candidate_models,omitempty"`
	LastFetchAt     string   `json:"last_fetch_at,omitempty"`
	LastFetchError  string   `json:"last_fetch_error,omitempty"`
}

// providerInput 是创建/更新供应商的请求体。
//
// 指针字段用于区分「未提供」与「显式置零/置 false」。
type providerInput struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	OpenAIBaseURL          string            `json:"openai_base_url"`
	OpenAIEndpointOverride string            `json:"openai_endpoint_override"`
	APIKey                 *string           `json:"api_key"`
	ModelsSelected         []string          `json:"models_selected"`
	ModelAliases           map[string]string `json:"model_aliases"`
	TimeoutSeconds         *int              `json:"timeout_seconds"`
	ExtraHeaders           map[string]string `json:"extra_headers"`
	ExtraBody              map[string]any    `json:"extra_body"`
	AllowInternal          *bool             `json:"allow_internal"`
	Enabled                *bool             `json:"enabled"`
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

func toDTO(rec store.ProviderRecord, candidates []string) providerDTO {
	return providerDTO{
		ID:                     rec.ID,
		Name:                   rec.Name,
		OpenAIBaseURL:          rec.OpenAIBaseURL,
		OpenAIEndpointOverride: rec.OpenAIEndpointOverride,
		APIKeyHint:             rec.APIKeyHint,
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
		items = append(items, toDTO(rec, candidates))
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
	writeJSON(w, http.StatusOK, toDTO(rec, candidates))
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	apiKey := ""
	if in.APIKey != nil {
		apiKey = strings.TrimSpace(*in.APIKey)
	}
	if apiKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "创建供应商必须提供 api_key"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	rec, err := s.upsertProvider(ctx, in, apiKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_ = s.applyReload(ctx)

	refreshed, _ := s.store.GetProvider(ctx, rec.ID)
	writeJSON(w, http.StatusCreated, toDTO(refreshed, nil))
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in providerInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id

	apiKey := ""
	if in.APIKey != nil {
		apiKey = strings.TrimSpace(*in.APIKey) // 空表示保持原值
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if _, err := s.upsertProvider(ctx, in, apiKey); err != nil {
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
	writeJSON(w, http.StatusOK, toDTO(rec, candidates))
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
	writeJSON(w, http.StatusOK, toDTO(rec, candidates))
}

// upsertProvider 把请求体转换为供应商并写库（apiKey 为空且已存在时沿用原凭证）。
func (s *Server) upsertProvider(ctx context.Context, in providerInput, apiKey string) (store.ProviderRecord, error) {
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

	if apiKey == "" && hasExisting {
		plain, derr := crypto.Decrypt(s.master, existing.APIKeyCipher, existing.ID)
		if derr != nil {
			return store.ProviderRecord{}, fmt.Errorf("读取既有凭证失败: %w", derr)
		}
		apiKey = string(plain)
	}
	if apiKey == "" {
		return store.ProviderRecord{}, errors.New("首次创建供应商必须提供 api_key")
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

	base := config.Provider{
		ID:                     id,
		Name:                   name,
		OpenAIBaseURL:          strings.TrimSpace(in.OpenAIBaseURL),
		OpenAIEndpointOverride: strings.TrimSpace(in.OpenAIEndpointOverride),
		APIKey:                 apiKey,
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
	if err := base.ValidateModelSelection(); err != nil {
		return store.ProviderRecord{}, err
	}

	// SaveProvider 内部会做 SSRF 校验（allow_internal=false 时拒绝内网地址）与 AES 加密
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

// loadProvider 读出并解密单个供应商。
func (s *Server) loadProvider(ctx context.Context, id string) (config.Provider, error) {
	providers, err := s.store.LoadProviders(ctx, s.master)
	if err != nil {
		return config.Provider{}, err
	}
	for _, p := range providers {
		if p.ID == id {
			return p, nil
		}
	}
	return config.Provider{}, store.ErrNotFound
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
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":                 gw.Listen,
		"port":                   gw.Port,
		"sse_idle_seconds":       gw.SSEIdleSeconds,
		"max_body_bytes":         gw.MaxBodyBytes,
		"gateway_key_hint":       keyInfo.KeyHint,
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

	plain, err := s.store.ResetGatewayKey(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "重置网关 Key 失败", err)
		return
	}
	s.logger.Warn("Web UI 重置了网关 Key：旧 Key 已失效", "hint", crypto.Mask(plain))
	// 明文只在此响应中出现一次
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_key": plain,
		"key_hint":    crypto.Mask(plain),
		"warning":     "请立即保存：明文只显示这一次，旧 Key 已失效",
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
