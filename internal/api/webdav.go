package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/crypto"
	"agora-model/internal/store"
	"agora-model/internal/sync"
)

// timeNow 是测试可注入的时间源；默认 RFC3339 字符串。
var timeNow = func() string { return time.Now().UTC().Format(time.RFC3339) }

// reqContext 为请求派生一个超时上下文：单接口上限 30s（与已有 handler 一致）。
func (s *Server) reqContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Second)
}

// WebDAV 配置相关设置 key（与 store.SettingLogSuccess 同方式存到 settings 表）。
//
// 出于简洁考虑，全部用 3 个独立 key：URL/Username/PasswordCipher。
// PasswordCipher 是 AES-256-GCM 密文（与供应商 API Key、网关 Key 同一套机制）。
// 远端文件名由 sync.RemoteFilename 常量固定，不再落库。
const (
	webdavURLKey        = "webdav_url"
	webdavUsernameKey   = "webdav_username"
	webdavPasswordKey   = "webdav_password_cipher" // 密文（hex 字符串）
	webdavLastSyncedKey = "webdav_last_synced_at"
)

// WebDAVConfigDTO 是「网关设置」页对 WebDAV 配置的对外表示。
//
// 注意：password 字段在响应中始终为空字符串（避免明文泄露），
// 写入时再传新密码，留空表示保持原值。
type WebDAVConfigDTO struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	// HasPassword 表示库中是否存有密码密文；用于 UI 提示「留空表示不修改」
	HasPassword  bool   `json:"has_password"`
	LastSyncedAt string `json:"last_synced_at,omitempty"`
}

// handleGetWebDAVConfig 返回当前 WebDAV 配置（不含密码明文）。
func (s *Server) handleGetWebDAVConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.reqContext(r)
	defer cancel()
	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 WebDAV 配置失败", err)
		return
	}
	dto := WebDAVConfigDTO{}
	if cfg != nil {
		dto.Configured = true
		dto.URL = cfg.URL
		dto.Username = cfg.Username
		dto.HasPassword = cfg.Password != ""
		// 不回传密码明文
	}
	if last, _, _ := s.store.GetSetting(ctx, webdavLastSyncedKey); last != "" {
		dto.LastSyncedAt = last
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleUpdateWebDAVConfig 更新 WebDAV 配置。空 password 字段表示沿用既有密码。
func (s *Server) handleUpdateWebDAVConfig(w http.ResponseWriter, r *http.Request) {
	var body WebDAVConfigDTO
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := s.reqContext(r)
	defer cancel()

	if strings.TrimSpace(body.URL) == "" {
		// 视为清空
		if err := s.saveWebDAVConfig(ctx, sync.Config{}); err != nil {
			s.fail(w, http.StatusInternalServerError, "清空 WebDAV 配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, WebDAVConfigDTO{})
		return
	}
	cfg := sync.Config{
		URL:      strings.TrimSpace(body.URL),
		Username: strings.TrimSpace(body.Username),
	}
	if body.Password != "" {
		cfg.Password = body.Password
	} else {
		// 沿用既有密码
		existing, err := s.loadWebDAVConfig(ctx)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "读取既有 WebDAV 配置失败", err)
			return
		}
		if existing == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "首次配置 WebDAV 必须填写密码",
			})
			return
		}
		cfg.Password = existing.Password
	}
	if err := s.saveWebDAVConfig(ctx, cfg); err != nil {
		s.fail(w, http.StatusInternalServerError, "写入 WebDAV 配置失败", err)
		return
	}
	s.logger.Info("已更新 WebDAV 配置", "url", cfg.URL, "username", cfg.Username)
	// 重新读出（不回传明文）
	s.handleGetWebDAVConfig(w, r)
}

// loadWebDAVConfig 读取 WebDAV 配置（已解密）。未配置任何字段返回 (nil, nil)。
func (s *Server) loadWebDAVConfig(ctx context.Context) (*sync.Config, error) {
	settings, err := s.store.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	url := settings[webdavURLKey]
	username := settings[webdavUsernameKey]
	if url == "" {
		return nil, nil
	}

	// 密码密文单独存：以 hex 字符串形式落库（避免 BLOB 与 SQL 驱动兼容问题）。
	cipherText, _, _ := s.store.GetSetting(ctx, webdavPasswordKey)
	plain := ""
	if cipherText != "" {
		cipherBytes, err := hex.DecodeString(cipherText)
		if err != nil {
			return nil, err
		}
		p, derr := crypto.Decrypt(s.master, cipherBytes, "webdav")
		if derr != nil {
			// 主密钥不匹配时仍允许读取 URL/用户名，密码置为空
			s.logger.Warn("WebDAV 密码解密失败：请确认主密钥或重新填写", "err", derr)
		} else {
			plain = string(p)
		}
	}
	return &sync.Config{
		URL:      url,
		Username: username,
		Password: plain,
	}, nil
}

// saveWebDAVConfig 写入 WebDAV 配置（密码加密）。URL 为空时清空所有相关字段。
func (s *Server) saveWebDAVConfig(ctx context.Context, cfg sync.Config) error {
	if strings.TrimSpace(cfg.URL) == "" {
		// 清空：删掉相关 key（含老版本的 webdav_filename，避免遗留脏数据）
		for _, key := range []string{webdavURLKey, webdavUsernameKey, webdavPasswordKey, "webdav_filename"} {
			if _, err := s.store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
				return err
			}
		}
		return nil
	}
	cipher, err := crypto.Encrypt(s.master, []byte(cfg.Password), "webdav")
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, webdavURLKey, cfg.URL); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, webdavUsernameKey, cfg.Username); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, webdavPasswordKey, hex.EncodeToString(cipher)); err != nil {
		return err
	}
	// 清理历史遗留的 webdav_filename（升级到本版本后不应再存在）
	_, _ = s.store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, "webdav_filename")
	return nil
}

// exportProvidersBytes 复用 /api/export 的逻辑得到导出字节。
func (s *Server) exportProvidersBytes(ctx context.Context) ([]byte, error) {
	records, err := s.store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := exportPayload{
		Format:     exportFormat,
		ExportedAt: timeNow(),
		Gateway:    s.holder.Get().Gateway(),
		Providers:  make([]exportedProvider, 0, len(records)),
	}
	for _, rec := range records {
		plain, derr := crypto.Decrypt(s.master, rec.APIKeyCipher, rec.ID)
		if derr != nil {
			return nil, derr
		}
		out.Providers = append(out.Providers, exportedProvider{
			providerDTO: toDTO(rec, nil),
			APIKey:      string(plain),
		})
	}
	return json.Marshal(out)
}

// exportProvidersFingerprintBytes 返回用于 WebDAV 冲突检测的稳定字节流。
//
// 与 exportProvidersBytes 的区别：ExportedAt 字段被替换为一个固定常量。同一份供应商配置
// 在不改动的前提下，无论何时调用指纹都相同——否则「刷新状态」就会刷新指纹、永远显示
// 与远端 mismatch，把 UI 推向无法收敛的死锁。
func (s *Server) exportProvidersFingerprintBytes(ctx context.Context) ([]byte, error) {
	data, err := s.exportProvidersBytes(ctx)
	if err != nil {
		return nil, err
	}
	// 用 map 重写 exported_at 后重新序列化：比在结构体里维护两个分支更不容易出错。
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("解析导出 JSON 用于指纹: %w", err)
	}
	generic["exported_at"] = "0001-01-01T00:00:00Z"
	return json.Marshal(generic)
}

// importProvidersBytes 复用 /api/import 的逻辑：把 exportPayload 形式的字节流写入数据库。
func (s *Server) importProvidersBytes(ctx context.Context, data []byte) (int, error) {
	var payload struct {
		Providers []providerInput `json:"providers"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, err
	}
	if len(payload.Providers) == 0 {
		return 0, errors.New("远端文件里没有 providers 数组")
	}
	count := 0
	for _, in := range payload.Providers {
		in := in
		apiKey := ""
		if in.APIKey != nil {
			apiKey = strings.TrimSpace(*in.APIKey)
		}
		if apiKey == "" {
			return count, errors.New("远端供应商缺少 api_key")
		}
		if _, err := s.upsertProvider(ctx, in, apiKey); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// compile-time guard：保证 settings key 与 store 现有定义一致。
var _ = store.SettingLogSuccess
