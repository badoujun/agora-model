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
	"time"

	"agora-model/internal/crypto"
)

const (
	// GatewayKeyPrefix 是网关 Key 的固定前缀。
	GatewayKeyPrefix = "gw-"
	// gatewayKeyIDPrefix 是 gateway_keys 表主键前缀。
	gatewayKeyIDPrefix = "gk_"
)

// ErrNoGatewayKey 表示库中不存在启用的网关 Key。
var ErrNoGatewayKey = errors.New("没有启用的网关 Key")

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
	ID         string
	Name       string
	KeyHint    string
	Enabled    bool
	CreatedAt  time.Time
	LastUsedAt time.Time
	// KeyCipher 是明文的密文（AES-256-GCM，AAD 取 Key id）；旧版本创建的记录为空。
	KeyCipher []byte
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

// ActiveGatewayKey 返回当前启用的网关 Key 信息。
func (s *Store) ActiveGatewayKey(ctx context.Context) (GatewayKeyInfo, error) {
	const q = `SELECT id, name, key_hint, enabled, created_at, COALESCE(last_used_at, ''), COALESCE(key_cipher, x'')
		FROM gateway_keys WHERE enabled = 1 AND revoked_at IS NULL
		ORDER BY created_at DESC LIMIT 1`
	var (
		info              GatewayKeyInfo
		enabled           int
		created, lastUsed string
	)
	err := s.db.QueryRowContext(ctx, q).Scan(&info.ID, &info.Name, &info.KeyHint, &enabled, &created, &lastUsed, &info.KeyCipher)
	if errors.Is(err, sql.ErrNoRows) {
		return GatewayKeyInfo{}, ErrNoGatewayKey
	}
	if err != nil {
		return GatewayKeyInfo{}, fmt.Errorf("查询网关 Key 失败: %w", err)
	}
	info.Enabled = enabled != 0
	info.CreatedAt = parseTime(created)
	info.LastUsedAt = parseTime(lastUsed)
	return info, nil
}

// EnsureGatewayKey 确保库中存在启用的网关 Key；不存在时生成一个。
//
// master 是用于加密明文的 AES 主密钥：明文只在本次新建时返回一次，
// 之后仍可通过 RevealGatewayKey 解密查看（旧版本创建的 Key 除外）。
func (s *Store) EnsureGatewayKey(ctx context.Context, master []byte) (plain string, created bool, err error) {
	_, err = s.ActiveGatewayKey(ctx)
	switch {
	case err == nil:
		return "", false, nil
	case !errors.Is(err, ErrNoGatewayKey):
		return "", false, err
	}
	plain, err = s.createGatewayKey(ctx, master, "default")
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// ResetGatewayKey 吊销现有 Key 并生成新的（旧 Key 立即失效）。
func (s *Store) ResetGatewayKey(ctx context.Context, master []byte) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE gateway_keys SET enabled = 0, revoked_at = ? WHERE revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return "", fmt.Errorf("吊销旧网关 Key 失败: %w", err)
	}
	return s.createGatewayKey(ctx, master, "default")
}

// RevealGatewayKey 解密当前启用的网关 Key 明文（供控制台查看/复制）。
//
// 明文还原不了时返回 ErrGatewayKeyNotRevealable（旧版本只存了哈希）
// 或 ErrGatewayKeyDecryptFailed（主密钥被更换/密文损坏）——两者都不是系统故障，
// 调用方应按"页面提示重置"处理，用 IsGatewayKeyUnrevealable 统一判断。
func (s *Store) RevealGatewayKey(ctx context.Context, master []byte) (string, error) {
	info, err := s.ActiveGatewayKey(ctx)
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

// VerifyGatewayKey 校验入站凭证，返回命中的 Key id。比对使用常量时间比较。
func (s *Store) VerifyGatewayKey(ctx context.Context, provided string) (keyID string, ok bool, err error) {
	if provided == "" {
		return "", false, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_hash FROM gateway_keys WHERE enabled = 1 AND revoked_at IS NULL`)
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
		`UPDATE gateway_keys SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// ListGatewayKeys 返回全部网关 Key 信息（供后续 Web UI 使用）。
func (s *Store) ListGatewayKeys(ctx context.Context) ([]GatewayKeyInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_hint, enabled, created_at, COALESCE(last_used_at, '')
		 FROM gateway_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GatewayKeyInfo
	for rows.Next() {
		var (
			info              GatewayKeyInfo
			enabled           int
			created, lastUsed string
		)
		if err := rows.Scan(&info.ID, &info.Name, &info.KeyHint, &enabled, &created, &lastUsed); err != nil {
			return nil, err
		}
		info.Enabled = enabled != 0
		info.CreatedAt = parseTime(created)
		info.LastUsedAt = parseTime(lastUsed)
		out = append(out, info)
	}
	return out, rows.Err()
}

func (s *Store) createGatewayKey(ctx context.Context, master []byte, name string) (string, error) {
	plain, err := GenerateGatewayKey()
	if err != nil {
		return "", err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("生成 Key id 失败: %w", err)
	}
	id := gatewayKeyIDPrefix + hex.EncodeToString(idBytes)
	// 明文密文与供应商凭证同样用 AES-256-GCM，AAD 取 Key id。
	// key_hash 仍是校验的唯一依据，密文只用于控制台查看/复制。
	cipherText, err := crypto.Encrypt(master, []byte(plain), id)
	if err != nil {
		return "", fmt.Errorf("加密网关 Key 失败: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO gateway_keys (id, name, key_hash, key_hint, key_cipher, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?)`,
		id, name, HashGatewayKey(plain), crypto.Mask(plain), cipherText,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", fmt.Errorf("写入网关 Key 失败: %w", err)
	}
	return plain, nil
}
