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
	"agora-model/internal/models"
	"agora-model/internal/provider"
	"agora-model/internal/store"
)

// 默认值（与 DESIGN §4.4 / config 包保持一致）。
const (
	defaultPriority       = 100
	defaultTimeoutSeconds = 120
)

// providerDTO 是对外的供应商表示：凭证只输出掩码。
type providerDTO struct {
	ID                        string            `json:"id"`
	Name                      string            `json:"name"`
	OpenAIBaseURL             string            `json:"openai_base_url"`
	AnthropicBaseURL          string            `json:"anthropic_base_url"`
	OpenAIEndpointOverride    string            `json:"openai_endpoint_override"`
	AnthropicEndpointOverride string            `json:"anthropic_endpoint_override"`
	APIKeyHint                string            `json:"api_key_hint"`
	ModelsManual              []string          `json:"models_manual"`
	ModelsExcluded            []string          `json:"models_excluded"`
	AutoFetchModels           bool              `json:"auto_fetch_models"`
	Priority                  int               `json:"priority"`
	TimeoutSeconds            int               `json:"timeout_seconds"`
	ExtraHeaders              map[string]string `json:"extra_headers,omitempty"`
	ExtraBody                 map[string]any    `json:"extra_body,omitempty"`
	AllowInternal             bool              `json:"allow_internal"`
	Enabled                   bool              `json:"enabled"`
	ModelCount                int               `json:"model_count"`
	AvailableModels           []string          `json:"available_models"`
	LastFetchAt               string            `json:"last_fetch_at,omitempty"`
	LastFetchError            string            `json:"last_fetch_error,omitempty"`
}

// providerInput 是创建/更新供应商的请求体。
//
// 指针字段用于区分「未提供」与「显式置零/置 false」。
type providerInput struct {
	ID                        string            `json:"id"`
	Name                      string            `json:"name"`
	OpenAIBaseURL             string            `json:"openai_base_url"`
	AnthropicBaseURL          string            `json:"anthropic_base_url"`
	OpenAIEndpointOverride    string            `json:"openai_endpoint_override"`
	AnthropicEndpointOverride string            `json:"anthropic_endpoint_override"`
	APIKey                    *string           `json:"api_key"`
	ModelsManual              []string          `json:"models_manual"`
	ModelsExcluded            []string          `json:"models_excluded"`
	AutoFetchModels           *bool             `json:"auto_fetch_models"`
	Priority                  *int              `json:"priority"`
	TimeoutSeconds            *int              `json:"timeout_seconds"`
	ExtraHeaders              map[string]string `json:"extra_headers"`
	ExtraBody                 map[string]any    `json:"extra_body"`
	AllowInternal             *bool             `json:"allow_internal"`
	Enabled                   *bool             `json:"enabled"`
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

func toDTO(rec store.ProviderRecord, available []string) providerDTO {
	return providerDTO{
		ID:                        rec.ID,
		Name:                      rec.Name,
		OpenAIBaseURL:             rec.OpenAIBaseURL,
		AnthropicBaseURL:          rec.AnthropicBaseURL,
		OpenAIEndpointOverride:    rec.OpenAIEndpointOverride,
		AnthropicEndpointOverride: rec.AnthropicEndpointOverride,
		APIKeyHint:                rec.APIKeyHint,
		ModelsManual:              rec.ModelsManual,
		ModelsExcluded:            rec.ModelsExcluded,
		AutoFetchModels:           rec.AutoFetchModels,
		Priority:                  rec.Priority,
		TimeoutSeconds:            rec.TimeoutSeconds,
		ExtraHeaders:              rec.ExtraHeaders,
		ExtraBody:                 rec.ExtraBody,
		AllowInternal:             rec.AllowInternal,
		Enabled:                   rec.Enabled,
		ModelCount:                len(available),
		AvailableModels:           available,
		LastFetchAt:               formatOptionalTime(rec.LastFetchAt),
		LastFetchError:            rec.LastFetchError,
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
	available := s.availableModels()
	items := make([]providerDTO, 0, len(records))
	for _, rec := range records {
		items = append(items, toDTO(rec, available[rec.ID]))
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
	writeJSON(w, http.StatusOK, toDTO(rec, s.availableModels()[rec.ID]))
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
	// 新供应商自动拉一次模型（失败不影响创建结果）
	if s.aggregator != nil && rec.AutoFetchModels {
		p, perr := s.loadProvider(ctx, rec.ID)
		if perr == nil {
			if ferr := s.aggregator.RefreshProvider(ctx, p); ferr != nil {
				s.logger.Warn("新建供应商后拉取模型失败", "provider", rec.ID, "err", ferr)
			}
		}
	}
	_ = s.applyReload(ctx)

	refreshed, _ := s.store.GetProvider(ctx, rec.ID)
	writeJSON(w, http.StatusCreated, toDTO(refreshed, s.availableModels()[rec.ID]))
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
	writeJSON(w, http.StatusOK, toDTO(rec, s.availableModels()[id]))
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

	result := provider.Test(ctx, p, s.client)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         result.OpenAI.OK || result.Anthropic.OK,
		"openai":     result.OpenAI,
		"anthropic":  result.Anthropic,
		"checked_at": result.CheckedAt.UTC().Format(time.RFC3339),
	})
}

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
	if s.aggregator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "聚合器不可用"})
		return
	}
	if err := s.aggregator.RefreshProvider(ctx, p); err != nil {
		// 失败原因已由聚合器写入 last_fetch_error
		rec, _ := s.store.GetProvider(ctx, p.ID)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            err.Error(),
			"last_fetch_error": rec.LastFetchError,
		})
		return
	}
	_ = s.applyReload(ctx)

	rec, err := s.store.GetProvider(ctx, p.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(rec, s.availableModels()[p.ID]))
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

	base := config.Provider{
		ID:                        id,
		Name:                      strings.TrimSpace(in.Name),
		OpenAIBaseURL:             strings.TrimSpace(in.OpenAIBaseURL),
		AnthropicBaseURL:          strings.TrimSpace(in.AnthropicBaseURL),
		OpenAIEndpointOverride:    strings.TrimSpace(in.OpenAIEndpointOverride),
		AnthropicEndpointOverride: strings.TrimSpace(in.AnthropicEndpointOverride),
		APIKey:                    apiKey,
		Models:                    cleanList(in.ModelsManual),
		ModelsExcluded:            cleanList(in.ModelsExcluded),
		AutoFetchModels:           in.AutoFetchModels,
		Priority:                  valueOr(in.Priority, defaultPriority),
		TimeoutSeconds:            valueOr(in.TimeoutSeconds, defaultTimeoutSeconds),
		ExtraHeaders:              in.ExtraHeaders,
		ExtraBody:                 in.ExtraBody,
		AllowInternal:             boolOr(in.AllowInternal, false),
		Enabled:                   in.Enabled,
	}
	if hasExisting {
		// 未提供的字段沿用既有值（PUT 语义贴近 PATCH，降低 Web UI 表单漏传导致配置丢失的风险）
		if base.Name == "" {
			base.Name = existing.Name
		}
		if base.OpenAIBaseURL == "" {
			base.OpenAIBaseURL = existing.OpenAIBaseURL
		}
		if base.AnthropicBaseURL == "" {
			base.AnthropicBaseURL = existing.AnthropicBaseURL
		}
		if len(in.ModelsManual) == 0 {
			base.Models = existing.ModelsManual
		}
		if in.AutoFetchModels == nil {
			auto := existing.AutoFetchModels
			base.AutoFetchModels = &auto
		}
		if in.AllowInternal == nil {
			base.AllowInternal = existing.AllowInternal
		}
		if in.Enabled == nil {
			enabled := existing.Enabled
			base.Enabled = &enabled
		}
	}
	if base.Endpoint(config.ProtocolOpenAI) == "" && base.Endpoint(config.ProtocolAnthropic) == "" {
		return store.ProviderRecord{}, errors.New("至少需要配置一个协议地址（openai_base_url 或 anthropic_base_url）")
	}

	// SaveProvider 内部会做 SSRF 校验（allow_internal=false 时拒绝内网地址）与 AES 加密
	return s.store.SaveProvider(ctx, s.master, base)
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
	snap := s.holder.Get()
	type entry struct {
		Model    string `json:"model"`
		Provider string `json:"provider_id"`
		Default  bool   `json:"default"`
		Source   string `json:"source"`
	}
	original := map[string]string{} // 裸名 -> 默认供应商
	items := make([]entry, 0, 32)
	for _, p := range snap.ProvidersByPriority() {
		for _, m := range p.Models {
			if _, ok := original[m]; !ok {
				original[m] = p.ID
			}
		}
	}
	for _, p := range snap.ProvidersByPriority() {
		manual := map[string]bool{}
		for _, m := range p.Models {
			manual[m] = true
		}
		for _, m := range p.Models {
			items = append(items, entry{
				Model:    m,
				Provider: p.ID,
				Default:  original[m] == p.ID,
				Source:   "manual_or_auto",
			})
		}
		_ = manual
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleRefreshModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if s.aggregator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "聚合器不可用"})
		return
	}
	s.aggregator.RefreshAll(ctx)
	_ = s.applyReload(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAddManualModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
		Model      string `json:"model"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	model := strings.TrimSpace(body.Model)
	if model == "" || strings.TrimSpace(body.ProviderID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider_id 与 model 均必填"})
		return
	}
	if err := s.mutateManualModels(ctx, body.ProviderID, func(list []string) []string {
		return append(list, model)
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_ = s.applyReload(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveManualModel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerID := strings.TrimSpace(q.Get("provider_id"))
	model := strings.TrimSpace(q.Get("model"))
	if providerID == "" || model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider_id 与 model 均必填"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := s.mutateManualModels(ctx, providerID, func(list []string) []string {
		out := make([]string, 0, len(list))
		for _, m := range list {
			if m != model {
				out = append(out, m)
			}
		}
		return out
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_ = s.applyReload(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mutateManualModels 读取供应商、应用变更函数后写回（凭证保持不变）。
func (s *Server) mutateManualModels(ctx context.Context, providerID string, mutate func([]string) []string) error {
	rec, err := s.store.GetProvider(ctx, providerID)
	if err != nil {
		return err
	}
	plain, err := crypto.Decrypt(s.master, rec.APIKeyCipher, rec.ID)
	if err != nil {
		return fmt.Errorf("读取凭证失败: %w", err)
	}
	auto := rec.AutoFetchModels
	enabled := rec.Enabled
	p := config.Provider{
		ID:                        rec.ID,
		Name:                      rec.Name,
		OpenAIBaseURL:             rec.OpenAIBaseURL,
		AnthropicBaseURL:          rec.AnthropicBaseURL,
		OpenAIEndpointOverride:    rec.OpenAIEndpointOverride,
		AnthropicEndpointOverride: rec.AnthropicEndpointOverride,
		APIKey:                    string(plain),
		Models:                    mutate(rec.ModelsManual),
		ModelsExcluded:            rec.ModelsExcluded,
		AutoFetchModels:           &auto,
		Priority:                  rec.Priority,
		TimeoutSeconds:            rec.TimeoutSeconds,
		ExtraHeaders:              rec.ExtraHeaders,
		ExtraBody:                 rec.ExtraBody,
		AllowInternal:             rec.AllowInternal,
		Enabled:                   &enabled,
	}
	_, err = s.store.SaveProvider(ctx, s.master, p)
	return err
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
		"model_refresh_seconds":  settings[models.SettingRefreshSeconds],
		"log_success":            settings[store.SettingLogSuccess],
	})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelRefreshSeconds *int  `json:"model_refresh_seconds"`
		LogSuccess          *bool `json:"log_success"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if body.ModelRefreshSeconds != nil {
		if *body.ModelRefreshSeconds < 30 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model_refresh_seconds 不能小于 30 秒"})
			return
		}
		if err := s.store.SetSetting(ctx, models.SettingRefreshSeconds, strconv.Itoa(*body.ModelRefreshSeconds)); err != nil {
			s.fail(w, http.StatusInternalServerError, "写入设置失败", err)
			return
		}
	}
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
