package crypto

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey() []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey()
	secret := "sk-live-abcdefghijklmnop"

	cipherText, err := Encrypt(key, []byte(secret), "provider-a")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(string(cipherText), secret) {
		t.Fatal("密文中不应包含明文")
	}

	plain, err := Decrypt(key, cipherText, "provider-a")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plain) != secret {
		t.Fatalf("解密结果 = %q, 期望 %q", plain, secret)
	}
}

func TestDecryptRejectsWrongAADAndKey(t *testing.T) {
	key := testKey()
	cipherText, err := Encrypt(key, []byte("sk-x"), "provider-a")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Decrypt(key, cipherText, "provider-b"); err == nil {
		t.Fatal("AAD 不同应解密失败（防止密文被搬到其他记录）")
	}

	other := testKey()
	other[0] ^= 0xFF
	if _, err := Decrypt(other, cipherText, "provider-a"); err == nil {
		t.Fatal("主密钥不同应解密失败")
	}
}

func TestEncryptUsesRandomNonce(t *testing.T) {
	key := testKey()
	first, err := Encrypt(key, []byte("same"), "aad")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encrypt(key, []byte("same"), "aad")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("两次加密结果不应相同（nonce 需随机）")
	}
}

func TestDecryptTooShort(t *testing.T) {
	if _, err := Decrypt(testKey(), []byte("short"), "aad"); !errors.Is(err, ErrCiphertextTooShort) {
		t.Fatalf("期望 ErrCiphertextTooShort，得到 %v", err)
	}
}

func TestParseMasterKey(t *testing.T) {
	raw := testKey()

	if got, err := ParseMasterKey(hex.EncodeToString(raw)); err != nil || string(got) != string(raw) {
		t.Fatalf("hex 解析失败: %v", err)
	}
	if got, err := ParseMasterKey(base64.StdEncoding.EncodeToString(raw)); err != nil || string(got) != string(raw) {
		t.Fatalf("base64 解析失败: %v", err)
	}
	if got, err := ParseMasterKey(base64.RawURLEncoding.EncodeToString(raw)); err != nil || string(got) != string(raw) {
		t.Fatalf("raw-url base64 解析失败: %v", err)
	}
	for _, bad := range []string{"", "   ", "not-a-key", "abcd"} {
		if _, err := ParseMasterKey(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseMasterKey(%q) 期望 ErrInvalidKey，得到 %v", bad, err)
		}
	}
}

func TestEncryptRejectsBadKeyLength(t *testing.T) {
	if _, err := Encrypt(make([]byte, 16), []byte("x"), "aad"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("非 32 字节密钥应报 ErrInvalidKey，得到 %v", err)
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"short":               "*****",
		"sk-abcdefghijklmnop": "sk-****mnop",
		"gw-12345678":         "gw-****5678",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestLoadMasterKeyFromEnv(t *testing.T) {
	key := testKey()
	t.Setenv(EnvMasterKey, hex.EncodeToString(key))

	got, err := LoadMasterKey(MasterKeyOptions{FilePath: filepath.Join(t.TempDir(), "master.key")})
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("环境变量应优先于文件")
	}
}

func TestLoadMasterKeyGeneratesFile(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	path := filepath.Join(t.TempDir(), "sub", "master.key")

	first, err := LoadMasterKey(MasterKeyOptions{FilePath: path})
	if err != nil {
		t.Fatalf("首次加载应生成主密钥: %v", err)
	}
	if len(first) != KeySize {
		t.Fatalf("主密钥长度 = %d", len(first))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("主密钥文件未生成: %v", err)
	}

	second, err := LoadMasterKey(MasterKeyOptions{FilePath: path})
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("二次加载应读取同一主密钥")
	}
}

func TestLoadMasterKeyRejectsCorruptFile(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(MasterKeyOptions{FilePath: path}); err == nil {
		t.Fatal("损坏的主密钥文件应报错，不能静默降级")
	}
}
