package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

// ProviderKeyRecord 是 provider_keys 表的一行（明文以密文存放）。
type ProviderKeyRecord struct {
	ID         string
	ProviderID string
	Label      string
	// Position 是展示/轮询顺序（0 起，与保存时提交的顺序一致）。
	Position int
	// KeyCipher 是 AES-256-GCM 密文（AAD = provider_id）。
	KeyCipher []byte
	KeyHint   string
	// Enabled 是用户期望的启用状态；AutoDisabled 表示该 Key 是被网关
	// 因凭证类失败自动停用的（enabled 同时为 false）。
	Enabled        bool
	AutoDisabled   bool
	DisabledReason string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastUsedAt     time.Time
	LastFailureAt  time.Time
	FailureCount   int64
	LastError      string
}

// ProviderRecord 是 providers 表的一行（凭证在 provider_keys 表，Keys 一并装载）。
type ProviderRecord struct {
	ID                     string
	Name                   string
	OpenAIBaseURL          string
	OpenAIEndpointOverride string
	// WebsiteURL 是供应商官网地址（仅展示用）。
	WebsiteURL string
	// Keys 是该供应商的全部凭证（按 created_at 升序，即轮询/展示顺序）。
	Keys []ProviderKeyRecord
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

// EnabledKeyHint 返回首个启用 Key 的掩码（控制面兼容字段 api_key_hint 使用）。
func (r ProviderRecord) EnabledKeyHint() string {
	for _, k := range r.Keys {
		if k.Enabled {
			return k.KeyHint
		}
	}
	return ""
}

const providerColumns = `id, name, openai_base_url, openai_endpoint_override,
	models_selected_json, models_alias_json,
	timeout_seconds, extra_headers_json, extra_body_json, allow_internal, enabled,
	created_at, updated_at, last_fetch_at, last_fetch_error, website_url`

const providerKeyColumns = `id, provider_id, label, position, key_cipher, key_hint,
	enabled, auto_disabled, disabled_reason, created_at, updated_at,
	last_used_at, last_failure_at, last_error, failure_count`

// upsertProviderTx 写入或更新供应商主记录（不触碰凭证；调用方负责同一个事务）。
func upsertProviderTx(ctx context.Context, tx *sql.Tx, rec ProviderRecord) error {
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			openai_base_url=excluded.openai_base_url,
			openai_endpoint_override=excluded.openai_endpoint_override,
			models_selected_json=excluded.models_selected_json,
			models_alias_json=excluded.models_alias_json,
			timeout_seconds=excluded.timeout_seconds,
			extra_headers_json=excluded.extra_headers_json,
			extra_body_json=excluded.extra_body_json,
			allow_internal=excluded.allow_internal,
			enabled=excluded.enabled,
			website_url=excluded.website_url,
			updated_at=excluded.updated_at`

	_, err = tx.ExecContext(ctx, q,
		rec.ID, rec.Name, rec.OpenAIBaseURL, rec.OpenAIEndpointOverride,
		string(selected), string(aliases),
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

// GetProvider 读取一条供应商记录（含其全部凭证，不含明文）。
func (s *Store) GetProvider(ctx context.Context, id string) (ProviderRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = ?`, id)
	rec, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRecord{}, ErrNotFound
	}
	if err != nil {
		return ProviderRecord{}, err
	}
	keys, err := s.listProviderKeys(ctx, id)
	if err != nil {
		return ProviderRecord{}, err
	}
	rec.Keys = keys
	return rec, nil
}

// ListProviders 按名称升序返回全部供应商（含停用项）及其凭证。
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// 一次查询装载全部凭证后分组，避免 N+1。
	all, err := s.listAllProviderKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Keys = all[out[i].ID]
	}
	return out, nil
}

// ListProviderKeys 返回某供应商的全部凭证（含停用项，按创建顺序）。
func (s *Store) ListProviderKeys(ctx context.Context, providerID string) ([]ProviderKeyRecord, error) {
	return s.listProviderKeys(ctx, providerID)
}

func (s *Store) listProviderKeys(ctx context.Context, providerID string) ([]ProviderKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+providerKeyColumns+` FROM provider_keys WHERE provider_id = ? ORDER BY position ASC, created_at ASC, id ASC`,
		providerID)
	if err != nil {
		return nil, fmt.Errorf("查询供应商凭证失败: %w", err)
	}
	defer rows.Close()

	var out []ProviderKeyRecord
	for rows.Next() {
		rec, err := scanProviderKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) listAllProviderKeys(ctx context.Context) (map[string][]ProviderKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+providerKeyColumns+` FROM provider_keys ORDER BY provider_id ASC, position ASC, created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询供应商凭证失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]ProviderKeyRecord)
	for rows.Next() {
		rec, err := scanProviderKey(rows)
		if err != nil {
			return nil, err
		}
		out[rec.ProviderID] = append(out[rec.ProviderID], rec)
	}
	return out, rows.Err()
}

// GetProviderKey 读取单个凭证记录（含密文，不含明文）。
func (s *Store) GetProviderKey(ctx context.Context, providerID, keyID string) (ProviderKeyRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+providerKeyColumns+` FROM provider_keys WHERE provider_id = ? AND id = ?`,
		providerID, keyID)
	rec, err := scanProviderKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderKeyRecord{}, ErrNotFound
	}
	return rec, err
}

// DeleteProvider 删除供应商并清理其模型候选缓存、凭证与按 Key 用量明细。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM model_cache WHERE provider_id = ?`,
		`DELETE FROM provider_key_usage_daily WHERE provider_id = ?`,
		`DELETE FROM provider_keys WHERE provider_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			_ = tx.Rollback()
			return err
		}
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

// SaveProvider 加密凭证后落库（业务层入口）。
//
// 凭证采用「全量替换」语义：p.APIKeys 就是该供应商最终的凭证集合，
// 未出现在列表中的旧 Key 会被删除。每个 Key 单独以 AES-256-GCM 加密，
// AAD 取 provider.id（迁移自旧版本的单凭证密文可直接沿用，无需主密钥）。
func (s *Store) SaveProvider(ctx context.Context, master []byte, p config.Provider) (ProviderRecord, error) {
	if p.ID == "" {
		return ProviderRecord{}, errors.New("供应商 id 不能为空")
	}
	// 统一规整模型选择、别名与凭证（API 与引导配置两条路径共用同一套规则）
	p.CleanSelection()
	if err := p.ValidateKeys(); err != nil {
		return ProviderRecord{}, err
	}
	// SSRF 防护：保存时校验一次（DESIGN §7.7），转发路径不重复校验
	for _, u := range []string{p.OpenAIBaseURL, p.OpenAIEndpointOverride} {
		if err := security.ValidateUpstreamURL(u, p.AllowInternal); err != nil {
			return ProviderRecord{}, fmt.Errorf("供应商 %s 地址校验失败: %w", p.ID, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProviderRecord{}, fmt.Errorf("保存供应商开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := existingProviderTx(ctx, tx, p.ID)
	created := time.Now().UTC()
	lastFetchAt := time.Time{}
	lastFetchError := ""
	if err == nil {
		created = existing.CreatedAt
		lastFetchAt = existing.LastFetchAt
		lastFetchError = existing.LastFetchError
	} else if !errors.Is(err, ErrNotFound) {
		return ProviderRecord{}, err
	}

	oldKeys, err := providerKeysTx(ctx, tx, p.ID)
	if err != nil {
		return ProviderRecord{}, err
	}
	oldByID := make(map[string]ProviderKeyRecord, len(oldKeys))
	for _, k := range oldKeys {
		oldByID[k.ID] = k
	}

	rec := ProviderRecord{
		ID:                     p.ID,
		Name:                   p.Name,
		OpenAIBaseURL:          p.OpenAIBaseURL,
		OpenAIEndpointOverride: p.OpenAIEndpointOverride,
		WebsiteURL:             p.WebsiteURL,
		ModelsSelected:         p.Models,
		ModelAliases:           p.ModelAliases,
		TimeoutSeconds:         p.TimeoutSeconds,
		ExtraHeaders:           p.ExtraHeaders,
		ExtraBody:              p.ExtraBody,
		AllowInternal:          p.AllowInternal,
		Enabled:                p.IsEnabled(),
		CreatedAt:              created,
		LastFetchAt:            lastFetchAt,
		LastFetchError:         lastFetchError,
	}
	if err := upsertProviderTx(ctx, tx, rec); err != nil {
		return ProviderRecord{}, err
	}

	now := time.Now().UTC()
	keep := make(map[string]bool, len(p.APIKeys))
	enabledCount := 0
	for i, in := range p.APIKeys {
		prev, hasPrev := oldByID[in.ID]
		if in.ID != "" && !hasPrev {
			return ProviderRecord{}, fmt.Errorf("供应商 %s 的第 %d 个 API Key id %q 不属于该供应商",
				p.ID, i+1, in.ID)
		}

		row := ProviderKeyRecord{
			ID:             in.ID,
			ProviderID:     p.ID,
			Label:          in.Label,
			Position:       i,
			Enabled:        in.IsEnabled(),
			CreatedAt:      prev.CreatedAt,
			AutoDisabled:   prev.AutoDisabled,
			DisabledReason: prev.DisabledReason,
		}
		if in.Key != "" {
			cipherText, cerr := crypto.Encrypt(master, []byte(in.Key), p.ID)
			if cerr != nil {
				return ProviderRecord{}, fmt.Errorf("加密供应商凭证失败: %w", cerr)
			}
			row.KeyCipher = cipherText
			row.KeyHint = crypto.Mask(in.Key)
		} else {
			// 有 id 无明文：沿用库中密文（ValidateKeys 已保证不会出现「无 id 无明文」）
			row.KeyCipher = prev.KeyCipher
			row.KeyHint = prev.KeyHint
		}
		if row.ID == "" {
			row.ID = newProviderKeyID()
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		// 用户把 Key 重新启用时，清掉「自动停用」标记与原因
		if row.Enabled {
			row.AutoDisabled = false
			row.DisabledReason = ""
			enabledCount++
		}
		if err := upsertProviderKeyTx(ctx, tx, row); err != nil {
			return ProviderRecord{}, err
		}
		keep[row.ID] = true
	}
	if enabledCount == 0 {
		return ProviderRecord{}, fmt.Errorf("供应商 %s 至少需要启用一个 API Key（如需整体停用请停用供应商）", p.ID)
	}
	for _, old := range oldKeys {
		if keep[old.ID] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM provider_keys WHERE provider_id = ? AND id = ?`, p.ID, old.ID); err != nil {
			return ProviderRecord{}, fmt.Errorf("删除旧凭证失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return ProviderRecord{}, fmt.Errorf("提交供应商保存失败: %w", err)
	}
	return s.GetProvider(ctx, p.ID)
}

// LoadProviders 读出全部供应商并解密启用的凭证，返回可直接构建快照的配置。
//
// 只解密启用的 Key：停用的 Key 不进入运行时快照（自动停用后无需重启即生效）。
func (s *Store) LoadProviders(ctx context.Context, master []byte) ([]config.Provider, error) {
	records, err := s.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]config.Provider, 0, len(records))
	for _, rec := range records {
		p, err := toConfigProvider(rec, master)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadProvider 读出并解密单个供应商（只含启用的 Key）。
func (s *Store) LoadProvider(ctx context.Context, master []byte, id string) (config.Provider, error) {
	rec, err := s.GetProvider(ctx, id)
	if err != nil {
		return config.Provider{}, err
	}
	return toConfigProvider(rec, master)
}

// toConfigProvider 把记录解密为运行时配置。
func toConfigProvider(rec ProviderRecord, master []byte) (config.Provider, error) {
	keys := make([]config.ProviderKey, 0, len(rec.Keys))
	for _, kr := range rec.Keys {
		if !kr.Enabled {
			continue
		}
		plain, err := crypto.Decrypt(master, kr.KeyCipher, rec.ID)
		if err != nil {
			hint := kr.KeyHint
			if hint == "" {
				hint = "(无掩码)"
			}
			return config.Provider{}, fmt.Errorf("供应商 %s 的 API Key %s 解密失败: %w", rec.Name, hint, err)
		}
		enabled := true
		keys = append(keys, config.ProviderKey{
			ID:      kr.ID,
			Label:   kr.Label,
			Key:     string(plain),
			Enabled: &enabled,
		})
	}
	enabled := rec.Enabled
	p := config.Provider{
		ID:                     rec.ID,
		Name:                   rec.Name,
		OpenAIBaseURL:          rec.OpenAIBaseURL,
		OpenAIEndpointOverride: rec.OpenAIEndpointOverride,
		WebsiteURL:             rec.WebsiteURL,
		APIKeys:                keys,
		Models:                 rec.ModelsSelected,
		ModelAliases:           rec.ModelAliases,
		TimeoutSeconds:         rec.TimeoutSeconds,
		ExtraHeaders:           rec.ExtraHeaders,
		ExtraBody:              rec.ExtraBody,
		AllowInternal:          rec.AllowInternal,
		Enabled:                &enabled,
	}
	// 归一化：预计算 usableKeys（热路径零分配），并统一模型/别名规则。
	p.CleanSelection()
	return p, nil
}

func newProviderKeyID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("pk_%d", time.Now().UnixNano())
	}
	return "pk_" + hex.EncodeToString(buf)
}

// existingProviderTx 读取供应商的创建时间与拉取状态（不含凭证）。
func existingProviderTx(ctx context.Context, tx *sql.Tx, id string) (ProviderRecord, error) {
	var (
		rec                  ProviderRecord
		createdAt, updatedAt string
		lastFetchAt          string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, created_at, updated_at, last_fetch_at, last_fetch_error FROM providers WHERE id = ?`, id,
	).Scan(&rec.ID, &createdAt, &updatedAt, &lastFetchAt, &rec.LastFetchError)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRecord{}, ErrNotFound
	}
	if err != nil {
		return ProviderRecord{}, err
	}
	rec.CreatedAt = parseTime(createdAt)
	rec.UpdatedAt = parseTime(updatedAt)
	rec.LastFetchAt = parseTime(lastFetchAt)
	return rec, nil
}

// providerKeysTx 读取供应商已有凭证（事务内，含密文）。
func providerKeysTx(ctx context.Context, tx *sql.Tx, providerID string) ([]ProviderKeyRecord, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+providerKeyColumns+` FROM provider_keys WHERE provider_id = ? ORDER BY position ASC, created_at ASC, id ASC`,
		providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderKeyRecord
	for rows.Next() {
		rec, err := scanProviderKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func upsertProviderKeyTx(ctx context.Context, tx *sql.Tx, rec ProviderKeyRecord) error {
	now := time.Now().UTC()
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO provider_keys (`+providerKeyColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   label=excluded.label,
		   position=excluded.position,
		   key_cipher=excluded.key_cipher,
		   key_hint=excluded.key_hint,
		   enabled=excluded.enabled,
		   auto_disabled=excluded.auto_disabled,
		   disabled_reason=excluded.disabled_reason,
		   updated_at=excluded.updated_at`,
		rec.ID, rec.ProviderID, rec.Label, rec.Position, rec.KeyCipher, rec.KeyHint,
		boolToInt(rec.Enabled), boolToInt(rec.AutoDisabled), rec.DisabledReason,
		rec.CreatedAt.UTC().Format(time.RFC3339), now.Format(time.RFC3339),
		formatTime(rec.LastUsedAt), formatTime(rec.LastFailureAt), rec.LastError, rec.FailureCount,
	)
	if err != nil {
		return fmt.Errorf("写入供应商凭证 %s 失败: %w", rec.ID, err)
	}
	return nil
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
		&selected, &aliases,
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

func scanProviderKey(row rowScanner) (ProviderKeyRecord, error) {
	var (
		rec                   ProviderKeyRecord
		enabled, autoDisabled int
		createdAt, updatedAt  string
		lastUsed, lastFailure string
	)
	err := row.Scan(
		&rec.ID, &rec.ProviderID, &rec.Label, &rec.Position, &rec.KeyCipher, &rec.KeyHint,
		&enabled, &autoDisabled, &rec.DisabledReason, &createdAt, &updatedAt,
		&lastUsed, &lastFailure, &rec.LastError, &rec.FailureCount,
	)
	if err != nil {
		return ProviderKeyRecord{}, err
	}
	rec.Enabled = enabled != 0
	rec.AutoDisabled = autoDisabled != 0
	rec.CreatedAt = parseTime(createdAt)
	rec.UpdatedAt = parseTime(updatedAt)
	rec.LastUsedAt = parseTime(lastUsed)
	rec.LastFailureAt = parseTime(lastFailure)
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
