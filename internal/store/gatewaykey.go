package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"agora-model/internal/crypto"
)

const (
	// GatewayKeyPrefix 是网关 Key 的固定前缀。
	GatewayKeyPrefix = "gw-"
	// gatewayKeyIDPrefix 是 gateway_keys 表主键前缀。
	gatewayKeyIDPrefix = "gk_"
	// MaxGatewayKeyNameLen 是网关 Key 别名的最大长度（按 rune 计）。
	MaxGatewayKeyNameLen = 64
	// MaxGatewayKeyNoteLen 是网关 Key 备注（用途/使用人）的最大长度（按 rune 计）。
	MaxGatewayKeyNoteLen = 256
)

// ErrNoGatewayKey 表示库中不存在启用的网关 Key。
var ErrNoGatewayKey = errors.New("没有启用的网关 Key")

// ErrGatewayKeyNotFound 表示指定的网关 Key 不存在（或已被软删除且未被要求包含）。
var ErrGatewayKeyNotFound = errors.New("网关 Key 不存在")

// ErrLastGatewayKey 表示这次操作会停用/删除最后一个启用的网关 Key。
//
// 属于「拒绝执行」而不是系统故障：控制台应提示用户先新建一个再停用旧的，
// 否则所有 Agent 会立刻失去访问能力。
var ErrLastGatewayKey = errors.New("不能停用或删除最后一个启用的网关 Key")

// ErrGatewayKeyNotRevealable 表示当前 Key 只保存了哈希（由不存储明文的旧版本创建），无法找回明文。
var ErrGatewayKeyNotRevealable = errors.New("该网关 Key 只保存了哈希，无法查看明文，请重置网关 Key")

// ErrGatewayKeyDecryptFailed 表示密文无法用当前主密钥解密（主密钥被更换，或密文损坏）。
//
// 与 ErrGatewayKeyNotRevealable 一样属于"明文还原不了、只能靠重置恢复"，不是系统故障。
var ErrGatewayKeyDecryptFailed = errors.New("网关 Key 明文无法用当前主密钥解密，请重置网关 Key")

// IsGatewayKeyUnrevealable 报告错误是否表示明文无法还原（两种原因都只能通过重置网关 Key 恢复）。
func IsGatewayKeyUnrevealable(err error) bool {
	return errors.Is(err, ErrGatewayKeyNotRevealable) || errors.Is(err, ErrGatewayKeyDecryptFailed)
}

// GatewayKeyInfo 是网关 Key 的可展示信息（不含明文）。
type GatewayKeyInfo struct {
	ID      string
	Name    string
	Note    string
	KeyHint string
	Enabled bool
	// DeletedAt 非零表示该 Key 已被软删除：字段仍保留以便历史日志/统计显示其名称。
	DeletedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastUsedAt 是最近一次被用于请求的时间（零值表示从未使用）。
	LastUsedAt time.Time
	// KeyCipher 是明文的密文（AES-256-GCM，AAD 取 Key id）；旧版本创建的记录为空。
	KeyCipher []byte
	// Revealable 报告明文是否可还原（旧版本只存哈希或主密钥不匹配时为 false）。
	// 由调用方在需要时计算填充，不落库。
	Revealable bool
}

// HashGatewayKey 返回用于存储与比对的 sha256 摘要（明文不落库）。
func HashGatewayKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// GenerateGatewayKey 生成一个新的网关 Key；明文只在生成时返回一次。
func GenerateGatewayKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成网关 Key 失败: %w", err)
	}
	return GatewayKeyPrefix + hex.EncodeToString(buf), nil
}

// activeGatewayKeyQuery 选取「默认网关 Key」：最早创建的、仍启用且未删除的那个。
//
// 固定为最早创建的一条（而不是最新）是刻意的：设置页展示的默认 Key、CLI
// 打印的 Key、以及迁移时的历史归属都指向同一个，避免三处各说各话。
const activeGatewayKeyQuery = `SELECT id, name, note, key_hint, enabled, created_at,
		COALESCE(updated_at, ''), COALESCE(last_used_at, ''), COALESCE(deleted_at, ''),
		COALESCE(key_cipher, x'')
	FROM gateway_keys WHERE enabled = 1 AND revoked_at IS NULL AND deleted_at IS NULL
	ORDER BY created_at ASC, rowid ASC LIMIT 1`

// ActiveGatewayKey 返回默认网关 Key 的信息（最早创建的启用 Key）。
func (s *Store) ActiveGatewayKey(ctx context.Context) (GatewayKeyInfo, error) {
	info, err := scanGatewayKey(s.db.QueryRowContext(ctx, activeGatewayKeyQuery))
	if errors.Is(err, sql.ErrNoRows) {
		return GatewayKeyInfo{}, ErrNoGatewayKey
	}
	return info, err
}

// GetGatewayKey 按 id 返回网关 Key（含已软删除的记录）。
func (s *Store) GetGatewayKey(ctx context.Context, id string) (GatewayKeyInfo, error) {
	if strings.TrimSpace(id) == "" {
		return GatewayKeyInfo{}, ErrGatewayKeyNotFound
	}
	info, err := scanGatewayKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, note, key_hint, enabled, created_at,
		        COALESCE(updated_at, ''), COALESCE(last_used_at, ''), COALESCE(deleted_at, ''),
		        COALESCE(key_cipher, x'')
		 FROM gateway_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return GatewayKeyInfo{}, ErrGatewayKeyNotFound
	}
	return info, err
}

// ListGatewayKeys 返回网关 Key 列表（按创建时间升序，即稳定展示顺序）。
//
// includeDeleted 为真时一并返回软删除的记录（供日志/统计的下拉列表使用：
// 历史数据可能属于某个已删除的 Key，需要能按它筛选）。Revealable 通过
// 尝试解密判定，master 为空时一律置 false。
func (s *Store) ListGatewayKeys(ctx context.Context, master []byte, includeDeleted bool) ([]GatewayKeyInfo, error) {
	q := `SELECT id, name, note, key_hint, enabled, created_at,
	             COALESCE(updated_at, ''), COALESCE(last_used_at, ''), COALESCE(deleted_at, ''),
	             COALESCE(key_cipher, x'')
	      FROM gateway_keys`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询网关 Key 失败: %w", err)
	}
	defer rows.Close()

	out := make([]GatewayKeyInfo, 0, 8)
	for rows.Next() {
		info, err := scanGatewayKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(master) > 0 {
		for i := range out {
			if len(out[i].KeyCipher) == 0 {
				continue
			}
			if _, derr := crypto.Decrypt(master, out[i].KeyCipher, out[i].ID); derr == nil {
				out[i].Revealable = true
			}
		}
	}
	return out, nil
}

// CreateGatewayKey 新建一个网关 Key，明文只在本次返回。
//
// enabled 为 false 且库中尚无任何启用的 Key 时会被拒绝（否则等于把网关锁死）。
func (s *Store) CreateGatewayKey(ctx context.Context, master []byte, name, note string, enabled bool) (GatewayKeyInfo, string, error) {
	name, note, err := normalizeGatewayKeyMeta(name, note)
	if err != nil {
		return GatewayKeyInfo{}, "", err
	}
	if !enabled {
		enabledCount, err := s.countEnabledGatewayKeys(ctx)
		if err != nil {
			return GatewayKeyInfo{}, "", err
		}
		if enabledCount == 0 {
			return GatewayKeyInfo{}, "", fmt.Errorf("%w：请先启用至少一个网关 Key", ErrLastGatewayKey)
		}
	}
	id, plain, err := s.insertGatewayKey(ctx, master, name, note, enabled)
	if err != nil {
		return GatewayKeyInfo{}, "", err
	}
	info, err := s.GetGatewayKey(ctx, id)
	if err != nil {
		return GatewayKeyInfo{}, "", err
	}
	info.Revealable = true // 刚创建的 Key 一定带密文，可随时查看
	return info, plain, nil
}

// UpdateGatewayKey 更新别名的备注与启用状态（模型范围由 SetGatewayKeyScopes 单独维护）。
//
// 停用最后一个启用的 Key 会被拒绝，避免所有 Agent 立刻失去访问能力。
func (s *Store) UpdateGatewayKey(ctx context.Context, id, name, note string, enabled bool) (GatewayKeyInfo, error) {
	current, err := s.GetGatewayKey(ctx, id)
	if err != nil {
		return GatewayKeyInfo{}, err
	}
	if !current.DeletedAt.IsZero() {
		return GatewayKeyInfo{}, ErrGatewayKeyNotFound
	}
	name, note, err = normalizeGatewayKeyMeta(name, note)
	if err != nil {
		return GatewayKeyInfo{}, err
	}
	if !enabled && current.Enabled {
		enabledCount, err := s.countEnabledGatewayKeys(ctx)
		if err != nil {
			return GatewayKeyInfo{}, err
		}
		if enabledCount <= 1 {
			return GatewayKeyInfo{}, fmt.Errorf("%w：可以先新建一个再停用这个", ErrLastGatewayKey)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET name = ?, note = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		name, note, boolToInt(enabled), nowRFC3339(), id); err != nil {
		return GatewayKeyInfo{}, fmt.Errorf("更新网关 Key 失败: %w", err)
	}
	return s.GetGatewayKey(ctx, id)
}

// DeleteGatewayKey 软删除一个网关 Key：立即失效、从常规列表消失，
// 但记录保留，使历史日志与统计仍能显示它的别名。
//
// 删除最后一个启用的 Key 会被拒绝。
func (s *Store) DeleteGatewayKey(ctx context.Context, id string) error {
	current, err := s.GetGatewayKey(ctx, id)
	if err != nil {
		return err
	}
	if !current.DeletedAt.IsZero() {
		return ErrGatewayKeyNotFound
	}
	if current.Enabled {
		enabledCount, err := s.countEnabledGatewayKeys(ctx)
		if err != nil {
			return err
		}
		if enabledCount <= 1 {
			return fmt.Errorf("%w：可以先新建一个再删除这个", ErrLastGatewayKey)
		}
	}
	now := nowRFC3339()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET enabled = 0, deleted_at = ?, revoked_at = ?, updated_at = ?
		 WHERE id = ?`, now, now, now, id); err != nil {
		return fmt.Errorf("删除网关 Key 失败: %w", err)
	}
	return nil
}

// RevealGatewayKeyByID 解密指定网关 Key 的明文（供控制台查看/复制）。
//
// 明文还原不了时返回 ErrGatewayKeyNotRevealable（旧版本只存了哈希）
// 或 ErrGatewayKeyDecryptFailed（主密钥被更换/密文损坏）——两者都不是系统故障，
// 调用方应按"页面提示重置"处理，用 IsGatewayKeyUnrevealable 统一判断。
func (s *Store) RevealGatewayKeyByID(ctx context.Context, master []byte, id string) (string, error) {
	info, err := s.GetGatewayKey(ctx, id)
	if err != nil {
		return "", err
	}
	if len(info.KeyCipher) == 0 {
		return "", ErrGatewayKeyNotRevealable
	}
	plain, err := crypto.Decrypt(master, info.KeyCipher, info.ID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGatewayKeyDecryptFailed, err)
	}
	return string(plain), nil
}

// RevealGatewayKey 解密默认网关 Key 的明文（兼容旧调用方）。
func (s *Store) RevealGatewayKey(ctx context.Context, master []byte) (string, error) {
	info, err := s.ActiveGatewayKey(ctx)
	if err != nil {
		return "", err
	}
	return s.RevealGatewayKeyByID(ctx, master, info.ID)
}

// ResetGatewayKeyByID 为某个网关 Key 换一份新明文（旧明文立即失效）。
//
// id、别名、备注、启用状态与模型范围全部保留：重置只换凭证，
// 因此历史日志/统计的归属与权限范围都不受影响。
func (s *Store) ResetGatewayKeyByID(ctx context.Context, master []byte, id string) (string, error) {
	info, err := s.GetGatewayKey(ctx, id)
	if err != nil {
		return "", err
	}
	if !info.DeletedAt.IsZero() {
		return "", ErrGatewayKeyNotFound
	}
	plain, err := GenerateGatewayKey()
	if err != nil {
		return "", err
	}
	cipherText, err := crypto.Encrypt(master, []byte(plain), id)
	if err != nil {
		return "", fmt.Errorf("加密网关 Key 失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET key_hash = ?, key_hint = ?, key_cipher = ?, updated_at = ?
		 WHERE id = ?`,
		HashGatewayKey(plain), crypto.Mask(plain), cipherText, nowRFC3339(), id); err != nil {
		return "", fmt.Errorf("重置网关 Key 失败: %w", err)
	}
	return plain, nil
}

// EnsureGatewayKey 确保库中存在启用的网关 Key；不存在时生成一个默认 Key。
//
// master 是用于加密明文的 AES 主密钥：明文只在本次新建时返回一次，
// 之后仍可通过 RevealGatewayKeyByID 解密查看（旧版本创建的 Key 除外）。
func (s *Store) EnsureGatewayKey(ctx context.Context, master []byte) (plain string, created bool, err error) {
	_, err = s.ActiveGatewayKey(ctx)
	switch {
	case err == nil:
		return "", false, nil
	case !errors.Is(err, ErrNoGatewayKey):
		return "", false, err
	}
	_, plain, err = s.insertGatewayKey(ctx, master, "default", "", true)
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// ResetGatewayKey 软删全部网关 Key 并新建一个不限制模型范围的默认 Key。
//
// 供 CLI --reset-gateway-key 与旧接口使用：重置后所有旧 Key 立即失效。
// 控制台应优先用 ResetGatewayKeyByID（只换某一个 Key 的明文，不动其他 Key）。
func (s *Store) ResetGatewayKey(ctx context.Context, master []byte) (string, error) {
	now := nowRFC3339()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET enabled = 0, revoked_at = ?, deleted_at = ?, updated_at = ?
		 WHERE deleted_at IS NULL`, now, now, now); err != nil {
		return "", fmt.Errorf("吊销旧网关 Key 失败: %w", err)
	}
	_, plain, err := s.insertGatewayKey(ctx, master, "default", "", true)
	return plain, err
}

// VerifyGatewayKey 校验入站凭证，返回命中的 Key id。比对使用常量时间比较。
//
// 只有启用、未吊销、未软删除的 Key 参与校验；多个 Key 可同时启用。
func (s *Store) VerifyGatewayKey(ctx context.Context, provided string) (keyID string, ok bool, err error) {
	if provided == "" {
		return "", false, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_hash FROM gateway_keys
		 WHERE enabled = 1 AND revoked_at IS NULL AND deleted_at IS NULL`)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	want := HashGatewayKey(provided)
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return "", false, err
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(want)) == 1 {
			keyID, ok = id, true
		}
	}
	return keyID, ok, rows.Err()
}

// TouchGatewayKey 记录网关 Key 的最后使用时间（调用方需自行节流）。
func (s *Store) TouchGatewayKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET last_used_at = ? WHERE id = ?`, nowRFC3339(), id)
	return err
}

// GatewayKeyNames 返回全部网关 Key（含已删除）的 id → 展示名映射。
//
// 请求日志按网关 Key 筛选时要展示可读名称，已删除的 Key 也必须能显示，
// 否则历史日志会变成一串无意义的 id。
func (s *Store) GatewayKeyNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, note FROM gateway_keys`)
	if err != nil {
		return nil, fmt.Errorf("查询网关 Key 名称失败: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, name, note string
		if err := rows.Scan(&id, &name, &note); err != nil {
			return nil, err
		}
		out[id] = gatewayKeyDisplayName(name, note)
	}
	return out, rows.Err()
}

// gatewayKeyDisplayName 拼出下拉与日志列使用的展示名：优先别名，附备注。
func gatewayKeyDisplayName(name, note string) string {
	name = strings.TrimSpace(name)
	note = strings.TrimSpace(note)
	switch {
	case name == "" && note == "":
		return ""
	case note == "":
		return name
	case name == "":
		return note
	default:
		return name + "（" + note + "）"
	}
}

// countEnabledGatewayKeys 统计仍可用的网关 Key 数量（用于「最后一个」保护）。
func (s *Store) countEnabledGatewayKeys(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gateway_keys
		 WHERE enabled = 1 AND revoked_at IS NULL AND deleted_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计启用的网关 Key 失败: %w", err)
	}
	return n, nil
}

// insertGatewayKey 写入一条新 Key 并返回 (id, 明文)。
func (s *Store) insertGatewayKey(ctx context.Context, master []byte, name, note string, enabled bool) (string, string, error) {
	plain, err := GenerateGatewayKey()
	if err != nil {
		return "", "", err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("生成 Key id 失败: %w", err)
	}
	id := gatewayKeyIDPrefix + hex.EncodeToString(idBytes)
	// 明文密文与供应商凭证同样用 AES-256-GCM，AAD 取 Key id。
	// key_hash 仍是校验的唯一依据，密文只用于控制台查看/复制。
	cipherText, err := crypto.Encrypt(master, []byte(plain), id)
	if err != nil {
		return "", "", fmt.Errorf("加密网关 Key 失败: %w", err)
	}
	now := nowRFC3339()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_keys (id, name, note, key_hash, key_hint, key_cipher, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, note, HashGatewayKey(plain), crypto.Mask(plain), cipherText, boolToInt(enabled), now, now); err != nil {
		return "", "", fmt.Errorf("写入网关 Key 失败: %w", err)
	}
	return id, plain, nil
}

// normalizeGatewayKeyMeta 校验并规整别名与备注。
func normalizeGatewayKeyMeta(name, note string) (string, string, error) {
	name = strings.TrimSpace(name)
	note = strings.TrimSpace(note)
	if name == "" {
		return "", "", errors.New("网关 Key 别名不能为空")
	}
	if n := len([]rune(name)); n > MaxGatewayKeyNameLen {
		return "", "", fmt.Errorf("网关 Key 别名过长（%d 字符，上限 %d）", n, MaxGatewayKeyNameLen)
	}
	if n := len([]rune(note)); n > MaxGatewayKeyNoteLen {
		return "", "", fmt.Errorf("网关 Key 备注过长（%d 字符，上限 %d）", n, MaxGatewayKeyNoteLen)
	}
	return name, note, nil
}

// scanGatewayKey 把一行 gateway_keys 记录扫描为 GatewayKeyInfo。
func scanGatewayKey(row interface {
	Scan(dest ...any) error
}) (GatewayKeyInfo, error) {
	var (
		info                            GatewayKeyInfo
		enabled                         int
		created, updated, lastUsed, del string
	)
	if err := row.Scan(&info.ID, &info.Name, &info.Note, &info.KeyHint, &enabled,
		&created, &updated, &lastUsed, &del, &info.KeyCipher); err != nil {
		return GatewayKeyInfo{}, err
	}
	info.Enabled = enabled != 0
	info.CreatedAt = parseTime(created)
	info.UpdatedAt = parseTime(updated)
	info.LastUsedAt = parseTime(lastUsed)
	info.DeletedAt = parseTime(del)
	info.Revealable = len(info.KeyCipher) > 0
	return info, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
