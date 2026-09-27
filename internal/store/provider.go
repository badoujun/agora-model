package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/security"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// ProviderRecord 是 providers 表的一行（api_key 以密文存放）。
type ProviderRecord struct {
	ID                     string
	Name                   string
	OpenAIBaseURL          string
	OpenAIEndpointOverride string
	// WebsiteURL 是供应商官网地址（仅展示用）。
	WebsiteURL   string
	APIKeyCipher []byte
	APIKeyHint   string
	// ModelsSelected 是已勾选启用的上游模型名（顺序即展示顺序）。
	ModelsSelected []string
	// ModelAliases 把上游模型名映射为对外别名。
	ModelAliases   map[string]string
	TimeoutSeconds int
	ExtraHeaders   map[string]string
	ExtraBody      map[string]any
	AllowInternal  bool
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// LastFetchAt 是最后一次模型拉取的时间（失败时也会刷新，用于展示"陈旧"）。
	LastFetchAt time.Time
	// LastFetchError 是最近一次拉取失败的原因（成功时清空）。
	LastFetchError string
}

const providerColumns = `id, name, openai_base_url, openai_endpoint_override,
	api_key_cipher, api_key_hint, models_selected_json, models_alias_json,
	timeout_seconds, extra_headers_json, extra_body_json, allow_internal, enabled,
	created_at, updated_at, last_fetch_at, last_fetch_error, website_url`

// UpsertProvider 写入或更新一条供应商记录（不做加密，调用方负责传入密文）。
func (s *Store) UpsertProvider(ctx context.Context, rec ProviderRecord) error {
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now

	selected, err := json.Marshal(nonNilStrings(rec.ModelsSelected))
	if err != nil {
		return err
	}
	aliases, err := json.Marshal(nonNilMap(rec.ModelAliases))
	if err != nil {
		return err
	}
	headers, err := json.Marshal(nonNilMap(rec.ExtraHeaders))
	if err != nil {
		return err
	}
	body, err := json.Marshal(nonNilAnyMap(rec.ExtraBody))
	if err != nil {
		return err
	}

	const q = `INSERT INTO providers (` + providerColumns + `)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			openai_base_url=excluded.openai_base_url,
			openai_endpoint_override=excluded.openai_endpoint_override,
			api_key_cipher=excluded.api_key_cipher,
			api_key_hint=excluded.api_key_hint,
			models_selected_json=excluded.models_selected_json,
			models_alias_json=excluded.models_alias_json,
			timeout_seconds=excluded.timeout_seconds,
			extra_headers_json=excluded.extra_headers_json,
			extra_body_json=excluded.extra_body_json,
			allow_internal=excluded.allow_internal,
			enabled=excluded.enabled,
			website_url=excluded.website_url,
			updated_at=excluded.updated_at`

	_, err = s.db.ExecContext(ctx, q,
		rec.ID, rec.Name, rec.OpenAIBaseURL, rec.OpenAIEndpointOverride,
		rec.APIKeyCipher, rec.APIKeyHint, string(selected), string(aliases),
		rec.TimeoutSeconds, string(headers), string(body),
		boolToInt(rec.AllowInternal), boolToInt(rec.Enabled),
		rec.CreatedAt.UTC().Format(time.RFC3339), rec.UpdatedAt.UTC().Format(time.RFC3339),
		formatTime(rec.LastFetchAt), rec.LastFetchError, rec.WebsiteURL,
	)
	if err != nil {
		return fmt.Errorf("写入供应商 %s 失败: %w", rec.ID, err)
	}
	return nil
}

// GetProvider 读取一条供应商记录。
func (s *Store) GetProvider(ctx context.Context, id string) (ProviderRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = ?`, id)
	rec, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRecord{}, ErrNotFound
	}
	return rec, err
}

// ListProviders 按名称升序返回全部供应商（含停用项）。
//
// 名称即对外路由顺序：LoadProviders 与快照排序都依赖它保持稳定。
func (s *Store) ListProviders(ctx context.Context) ([]ProviderRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY name ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询供应商失败: %w", err)
	}
	defer rows.Close()

	var out []ProviderRecord
	for rows.Next() {
		rec, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteProvider 删除供应商并清理其模型候选缓存。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_cache WHERE provider_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	return tx.Commit()
}

// ProviderCount 返回供应商数量（用于判断是否需要从引导配置导入）。
func (s *Store) ProviderCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM providers`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// SaveProvider 加密 API Key 后落库（业务层入口）。
//
// 加密使用 AES-256-GCM，AAD 取 provider.id：密文不可被搬到其他记录上复用。
func (s *Store) SaveProvider(ctx context.Context, master []byte, p config.Provider) (ProviderRecord, error) {
	if p.ID == "" {
		return ProviderRecord{}, errors.New("供应商 id 不能为空")
	}
	// 统一规整模型选择与别名（API 与引导配置两条路径共用同一套规则）
	p.CleanSelection()
	// SSRF 防护：保存时校验一次（DESIGN §7.7），转发路径不重复校验
	for _, u := range []string{p.OpenAIBaseURL, p.OpenAIEndpointOverride} {
		if err := security.ValidateUpstreamURL(u, p.AllowInternal); err != nil {
			return ProviderRecord{}, fmt.Errorf("供应商 %s 地址校验失败: %w", p.ID, err)
		}
	}
	cipherText, err := crypto.Encrypt(master, []byte(p.APIKey), p.ID)
	if err != nil {
		return ProviderRecord{}, fmt.Errorf("加密供应商凭证失败: %w", err)
	}

	existing, err := s.GetProvider(ctx, p.ID)
	created := time.Now().UTC()
	if err == nil {
		created = existing.CreatedAt
	} else if !errors.Is(err, ErrNotFound) {
		return ProviderRecord{}, err
	}

	rec := ProviderRecord{
		ID:                     p.ID,
		Name:                   p.Name,
		OpenAIBaseURL:          p.OpenAIBaseURL,
		OpenAIEndpointOverride: p.OpenAIEndpointOverride,
		WebsiteURL:             p.WebsiteURL,
		APIKeyCipher:           cipherText,
		APIKeyHint:             crypto.Mask(p.APIKey),
		ModelsSelected:         p.Models,
		ModelAliases:           p.ModelAliases,
		TimeoutSeconds:         p.TimeoutSeconds,
		ExtraHeaders:           p.ExtraHeaders,
		ExtraBody:              p.ExtraBody,
		AllowInternal:          p.AllowInternal,
		Enabled:                p.IsEnabled(),
		CreatedAt:              created,
		LastFetchAt:            existing.LastFetchAt,
		LastFetchError:         existing.LastFetchError,
	}
	if err := s.UpsertProvider(ctx, rec); err != nil {
		return ProviderRecord{}, err
	}
	return s.GetProvider(ctx, p.ID)
}

// LoadProviders 读出全部供应商并解密凭证，返回可直接构建快照的配置。
func (s *Store) LoadProviders(ctx context.Context, master []byte) ([]config.Provider, error) {
	records, err := s.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]config.Provider, 0, len(records))
	for _, rec := range records {
		plain, err := crypto.Decrypt(master, rec.APIKeyCipher, rec.ID)
		if err != nil {
			return nil, fmt.Errorf("供应商 %s 的凭证解密失败: %w", rec.ID, err)
		}
		enabled := rec.Enabled
		out = append(out, config.Provider{
			ID:                     rec.ID,
			Name:                   rec.Name,
			OpenAIBaseURL:          rec.OpenAIBaseURL,
			OpenAIEndpointOverride: rec.OpenAIEndpointOverride,
			WebsiteURL:             rec.WebsiteURL,
			APIKey:                 string(plain),
			Models:                 rec.ModelsSelected,
			ModelAliases:           rec.ModelAliases,
			TimeoutSeconds:         rec.TimeoutSeconds,
			ExtraHeaders:           rec.ExtraHeaders,
			ExtraBody:              rec.ExtraBody,
			AllowInternal:          rec.AllowInternal,
			Enabled:                &enabled,
		})
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProvider(row rowScanner) (ProviderRecord, error) {
	var (
		rec                    ProviderRecord
		selected, aliases      string
		headers, body          string
		allowInternal, enabled int
		createdAt, updatedAt   string
		lastFetchAt            string
	)
	err := row.Scan(
		&rec.ID, &rec.Name, &rec.OpenAIBaseURL, &rec.OpenAIEndpointOverride,
		&rec.APIKeyCipher, &rec.APIKeyHint, &selected, &aliases,
		&rec.TimeoutSeconds, &headers, &body, &allowInternal, &enabled,
		&createdAt, &updatedAt, &lastFetchAt, &rec.LastFetchError, &rec.WebsiteURL,
	)
	if err != nil {
		return ProviderRecord{}, err
	}
	rec.ModelsSelected = decodeStrings(selected)
	rec.ModelAliases = decodeStringMap(aliases)
	rec.ExtraHeaders = decodeStringMap(headers)
	rec.ExtraBody = decodeAnyMap(body)
	rec.AllowInternal = allowInternal != 0
	rec.Enabled = enabled != 0
	rec.CreatedAt = parseTime(createdAt)
	rec.UpdatedAt = parseTime(updatedAt)
	rec.LastFetchAt = parseTime(lastFetchAt)
	return rec, nil
}

// formatTime 把时间格式化为 RFC3339；零值写空串（便于判断"从未拉取"）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func decodeStrings(raw string) []string {
	var out []string
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func decodeStringMap(raw string) map[string]string {
	var out map[string]string
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func decodeAnyMap(raw string) map[string]any {
	var out map[string]any
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func nonNilAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
