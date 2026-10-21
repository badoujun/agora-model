package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/store"
)

// gatewayKeyScopeDTO 是「网关 Key 可调用的模型」的一条：供应商 id + 上游模型名。
//
// 上游模型名（而非对外别名）才是稳定的路由目标，因此范围只存它；
// 前端展示时用 /api/models 把上游名映射回对外名。
type gatewayKeyScopeDTO struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// gatewayKeyUsageDTO 是某个网关 Key 的全部历史累计 token 用量。
//
// 口径与「使用统计」页一致：InputCached + InputUncached = 输入总量，
// TotalTokens = 输入总量 + Output。
type gatewayKeyUsageDTO struct {
	TotalTokens   int64 `json:"total_tokens"`
	InputCached   int64 `json:"input_cached"`
	InputUncached int64 `json:"input_uncached"`
	Output        int64 `json:"output"`
}

// gatewayKeyDTO 是网关 Key 的对外表示（永不含明文）。
type gatewayKeyDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Note       string `json:"note,omitempty"`
	KeyHint    string `json:"key_hint"`
	Enabled    bool   `json:"enabled"`
	Deleted    bool   `json:"deleted"`
	DeletedAt  string `json:"deleted_at,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	// Revealable 为 false 表示明文还原不了（旧版本只存哈希或主密钥不匹配），
	// 页面应提示「重置」而不是报故障。
	Revealable bool `json:"revealable"`
	// Scopes 是该 Key 的模型范围；空数组表示不限制（可调用全部模型）。
	Scopes []gatewayKeyScopeDTO `json:"scopes"`
	// Usage 是该 Key 的全部历史累计用量；没有请求过时各项为 0。
	Usage gatewayKeyUsageDTO `json:"usage"`
}

// toGatewayKeyDTO 把 store 的记录转为对外 DTO（scopes 由调用方查好传入）。
func toGatewayKeyDTO(info store.GatewayKeyInfo, scopes []store.GatewayKeyScopeRow) gatewayKeyDTO {
	dto := gatewayKeyDTO{
		ID:         info.ID,
		Name:       info.Name,
		Note:       info.Note,
		KeyHint:    info.KeyHint,
		Enabled:    info.Enabled,
		Deleted:    !info.DeletedAt.IsZero(),
		DeletedAt:  formatOptionalTime(info.DeletedAt),
		CreatedAt:  formatOptionalTime(info.CreatedAt),
		UpdatedAt:  formatOptionalTime(info.UpdatedAt),
		LastUsedAt: formatOptionalTime(info.LastUsedAt),
		Revealable: info.Revealable,
		Scopes:     make([]gatewayKeyScopeDTO, 0, len(scopes)),
	}
	for _, sc := range scopes {
		dto.Scopes = append(dto.Scopes, gatewayKeyScopeDTO{ProviderID: sc.ProviderID, Model: sc.Model})
	}
	return dto
}

// gatewayKeyUsageFrom 把 store 的累计用量转为 DTO。
//
// InputTokens 含命中缓存部分，所以未命中 = 差值（防御性 clamp 到 0）；
// 总 token = 输入总量 + 输出，与使用统计页的 rowTotal 口径一致。
func gatewayKeyUsageFrom(t store.GatewayKeyUsageTotals) gatewayKeyUsageDTO {
	uncached := t.InputTokens - t.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return gatewayKeyUsageDTO{
		TotalTokens:   t.InputTokens + t.OutputTokens,
		InputCached:   t.CachedInputTokens,
		InputUncached: uncached,
		Output:        t.OutputTokens,
	}
}

// scopeFromDTO 把请求体里的范围转为 config.ModelScope（空切片 → nil，即不限制）。
func scopeFromDTO(items []gatewayKeyScopeDTO) config.ModelScope {
	if len(items) == 0 {
		return nil
	}
	scope := config.ModelScope{}
	for _, item := range items {
		providerID := strings.TrimSpace(item.ProviderID)
		model := strings.TrimSpace(item.Model)
		if providerID == "" || model == "" {
			continue
		}
		models, ok := scope[providerID]
		if !ok {
			models = map[string]struct{}{}
			scope[providerID] = models
		}
		models[model] = struct{}{}
	}
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// validateGatewayKeyScope 拒绝会静默失效的模型范围（供应商已删除或模型已取消勾选）。
//
// 允许保存这样的范围只会让用户困惑：设置页勾着，请求却永远 403/404。
func (s *Server) validateGatewayKeyScope(scope config.ModelScope) error {
	if len(scope) == 0 {
		return nil
	}
	unknown := s.holder.Get().UnknownScopeRefs(scope)
	if len(unknown) == 0 {
		return nil
	}
	parts := make([]string, 0, len(unknown))
	for _, ref := range unknown {
		parts = append(parts, fmt.Sprintf("%s/%s", ref.ProviderID, ref.Model))
	}
	const maxShown = 5
	shown := parts
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	suffix := ""
	if len(parts) > maxShown {
		suffix = fmt.Sprintf(" 等 %d 项", len(parts))
	}
	return fmt.Errorf("模型范围包含已不可用的条目（供应商不存在或模型未勾选）：%s%s",
		strings.Join(shown, "、"), suffix)
}

// ---------------- 网关 Key ----------------

// handleListGatewayKeys 返回全量网关 Key（含已软删除的，供日志/统计下拉使用）。
func (s *Server) handleListGatewayKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	includeDeleted := isTruthy(r.URL.Query().Get("include_deleted"))
	infos, err := s.store.ListGatewayKeys(ctx, s.master, includeDeleted)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 失败", err)
		return
	}
	// 用量统计失败不应让 Key 列表本身不可用：降级为 0 并记一条 debug 日志。
	usageTotals, uerr := s.store.SumUsageByGatewayKey(ctx)
	if uerr != nil {
		s.logger.Debug("汇总网关 Key 用量失败", "err", uerr)
	}
	items := make([]gatewayKeyDTO, 0, len(infos))
	for _, info := range infos {
		scopes, serr := s.store.ListGatewayKeyScopesFor(ctx, info.ID)
		if serr != nil {
			s.logger.Debug("读取网关 Key 模型范围失败", "id", info.ID, "err", serr)
		}
		dto := toGatewayKeyDTO(info, scopes)
		dto.Usage = gatewayKeyUsageFrom(usageTotals[info.ID])
		items = append(items, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateGatewayKey 新建一个网关 Key；明文只在本次响应里返回一次。
func (s *Server) handleCreateGatewayKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string               `json:"name"`
		Note    string               `json:"note"`
		Enabled *bool                `json:"enabled"`
		Scopes  []gatewayKeyScopeDTO `json:"scopes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	scope := scopeFromDTO(body.Scopes)
	if err := s.validateGatewayKeyScope(scope); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	info, plain, err := s.store.CreateGatewayKey(ctx, s.master, body.Name, body.Note, boolOr(body.Enabled, true))
	switch {
	case errors.Is(err, store.ErrLastGatewayKey):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	case err != nil:
		// 别名/备注的校验错误属于「请求不合法」
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if len(scope) > 0 {
		if err := s.store.SetGatewayKeyScopes(ctx, info.ID, scopeRows(scope)); err != nil {
			s.fail(w, http.StatusInternalServerError, "写入网关 Key 模型范围失败", err)
			return
		}
		info, _ = s.store.GetGatewayKey(ctx, info.ID)
	}
	if err := s.applyReload(ctx); err != nil {
		s.logger.Warn("新建网关 Key 后刷新快照失败", "err", err)
	}
	scopes, _ := s.store.ListGatewayKeyScopesFor(ctx, info.ID)
	s.logger.Warn("控制台新建了网关 Key", "name", info.Name, "hint", info.KeyHint)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":         toGatewayKeyDTO(info, scopes),
		"gateway_key": plain,
		"warning":     "请立即复制该 Key：明文仅本次返回，之后仍可在本页查看",
	})
}

// handleUpdateGatewayKey 更新别名 / 备注 / 启用状态，并按需替换模型范围。
//
// 字段用指针接收，以区分「没传」（沿用现值）与「传了空值」（清空）：
// 前端切换启用开关时只需回传 enabled，不必先回读整个对象。
func (s *Server) handleUpdateGatewayKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    *string               `json:"name"`
		Note    *string               `json:"note"`
		Enabled *bool                 `json:"enabled"`
		Scopes  *[]gatewayKeyScopeDTO `json:"scopes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id := r.PathValue("id")
	current, err := s.store.GetGatewayKey(ctx, id)
	if errors.Is(err, store.ErrGatewayKeyNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "网关 Key 不存在"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 失败", err)
		return
	}

	name := current.Name
	if body.Name != nil {
		name = *body.Name // 空串交由 store 校验（别名必填）
	}
	note := current.Note
	if body.Note != nil {
		note = *body.Note
	}
	enabled := current.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	var scope config.ModelScope
	if body.Scopes != nil {
		scope = scopeFromDTO(*body.Scopes)
		if err := s.validateGatewayKeyScope(scope); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}

	info, err := s.store.UpdateGatewayKey(ctx, id, name, note, enabled)
	switch {
	case errors.Is(err, store.ErrLastGatewayKey):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	case errors.Is(err, store.ErrGatewayKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "网关 Key 不存在"})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if body.Scopes != nil {
		if err := s.store.SetGatewayKeyScopes(ctx, id, scopeRows(scope)); err != nil {
			s.fail(w, http.StatusInternalServerError, "写入网关 Key 模型范围失败", err)
			return
		}
	}
	if err := s.applyReload(ctx); err != nil {
		s.logger.Warn("更新网关 Key 后刷新快照失败", "err", err)
	}
	scopes, _ := s.store.ListGatewayKeyScopesFor(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"key": toGatewayKeyDTO(info, scopes)})
}

// handleDeleteGatewayKey 软删除一个网关 Key：立即失效，但保留记录供历史日志/统计显示。
func (s *Server) handleDeleteGatewayKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id := r.PathValue("id")
	err := s.store.DeleteGatewayKey(ctx, id)
	switch {
	case errors.Is(err, store.ErrLastGatewayKey):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	case errors.Is(err, store.ErrGatewayKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "网关 Key 不存在"})
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, "删除网关 Key 失败", err)
		return
	}
	if err := s.applyReload(ctx); err != nil {
		s.logger.Warn("删除网关 Key 后刷新快照失败", "err", err)
	}
	info, _ := s.store.GetGatewayKey(ctx, id)
	s.logger.Warn("控制台软删除了网关 Key", "id", id, "name", info.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
}

// handleRevealGatewayKey 返回指定网关 Key 的明文（供控制台查看/复制）。
func (s *Server) handleRevealGatewayKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id := r.PathValue("id")
	plain, err := s.store.RevealGatewayKeyByID(ctx, s.master, id)
	switch {
	case errors.Is(err, store.ErrGatewayKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "网关 Key 不存在"})
		return
	case store.IsGatewayKeyUnrevealable(err):
		// 属于「提示重置」而不是接口故障：旧记录只存哈希，或主密钥已更换。
		if errors.Is(err, store.ErrGatewayKeyDecryptFailed) {
			s.logger.Warn("网关 Key 明文无法解密：主密钥可能与数据库不匹配", "id", id, "err", err)
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      err.Error(),
			"revealable": false,
		})
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, "读取网关 Key 明文失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_key": plain,
		"key_hint":    crypto.Mask(plain),
	})
}

// handleResetGatewayKeyByID 为指定网关 Key 换一份新明文（旧明文立即失效）。
func (s *Server) handleResetGatewayKeyByID(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id := r.PathValue("id")
	plain, err := s.store.ResetGatewayKeyByID(ctx, s.master, id)
	switch {
	case errors.Is(err, store.ErrGatewayKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "网关 Key 不存在"})
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, "重置网关 Key 失败", err)
		return
	}
	info, _ := s.store.GetGatewayKey(ctx, id)
	s.logger.Warn("控制台重置了网关 Key（旧明文已失效）", "id", id, "name", info.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_key": plain,
		"key_hint":    crypto.Mask(plain),
		"warning":     "已生成新的明文；该 Key 的别名、备注、状态与模型范围均保持不变",
	})
}

// scopeRows 把范围映射展开为可落库的行列表。
func scopeRows(scope config.ModelScope) []store.GatewayKeyScopeRow {
	rows := make([]store.GatewayKeyScopeRow, 0, len(scope))
	for providerID, models := range scope {
		for model := range models {
			rows = append(rows, store.GatewayKeyScopeRow{ProviderID: providerID, Model: model})
		}
	}
	return rows
}

// isTruthy 解析查询参数里的布尔开关（"1"/"true" 为真）。
func isTruthy(raw string) bool {
	raw = strings.ToLower(strings.TrimSpace(raw))
	return raw == "1" || raw == "true" || raw == "yes"
}
