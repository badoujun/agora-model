package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize 是 AES-256 主密钥的字节长度。
const KeySize = 32

var (
	// ErrInvalidKey 表示主密钥长度或编码不合法。
	ErrInvalidKey = errors.New("主密钥必须是 32 字节（64 位 hex 或 base64）")
	// ErrCiphertextTooShort 表示密文短于 nonce+tag。
	ErrCiphertextTooShort = errors.New("密文长度不足")
)

// Encrypt 以 AES-256-GCM 加密明文，输出布局为 nonce || ciphertext || tag。
//
// aad 建议传入 provider.id：即使密文被复制到其他记录上也无法解密使用。
func Encrypt(master, plaintext []byte, aad string) ([]byte, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, []byte(aad)), nil
}

// Decrypt 解密 Encrypt 的输出。
func Decrypt(master, ciphertext []byte, aad string) ([]byte, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrCiphertextTooShort
	}
	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("解密失败（主密钥是否被更换？）: %w", err)
	}
	return plain, nil
}

func newGCM(master []byte) (cipher.AEAD, error) {
	if len(master) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ParseMasterKey 解析 64 位 hex 、标准 base64 或 URL-safe base64 形式的 32 字节主密钥。
func ParseMasterKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, ErrInvalidKey
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == KeySize {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == KeySize {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == KeySize {
		return b, nil
	}
	return nil, ErrInvalidKey
}

// Mask 把密钥渲染为 sk-****abcd 形式用于展示，任何出口都不应回传明文。
func Mask(secret string) string {
	s := strings.TrimSpace(secret)
	switch {
	case s == "":
		return ""
	case len(s) <= 8:
		return strings.Repeat("*", len(s))
	default:
		return s[:3] + "****" + s[len(s)-4:]
	}
}
