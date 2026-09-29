package api

import (
	"errors"
	"net/http"

	"agora-model/internal/presets"
	"agora-model/internal/sync"
)

// handleListProviderPresets 返回「国内常用供应商」预设列表。
func (s *Server) handleListProviderPresets(w http.ResponseWriter, r *http.Request) {
	if s.presets == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "预设未启用：请用 --presets docs/provider-presets.json 启动",
		})
		return
	}
	f, err := s.presets.Load()
	if err != nil {
		if errors.Is(err, presets.ErrEmpty) {
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "source": s.presets.Source()})
			return
		}
		s.fail(w, http.StatusInternalServerError, "读取预设失败", err)
		return
	}

	type presetDTO struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		OpenAIBaseURL string `json:"openai_base_url"`
		WebsiteURL    string `json:"website_url"`
	}
	items := make([]presetDTO, 0, len(f.Presets))
	for _, p := range f.Presets {
		items = append(items, presetDTO{
			ID:            p.ID,
			Name:          p.Name,
			OpenAIBaseURL: p.OpenAIBaseURL,
			WebsiteURL:    p.WebsiteURL,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":               items,
		"source":              s.presets.Source(),
		"builtin_fingerprint": presets.BuiltinFingerprint(),
	})
}

// ---------------- WebDAV 同步 ----------------

// webdavStatusResponse 是 /api/sync/webdav/status 的响应。
type webdavStatusResponse struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
	Username   string `json:"username,omitempty"`
	// HasRemote 表示远端已存在
	HasRemote bool `json:"has_remote"`
	// LocalFingerprint 是本地当前供应商配置的 SHA256；用于与远端比对
	LocalFingerprint string `json:"local_fingerprint"`
	// RemoteFingerprint 仅在 HasRemote=true 时存在
	RemoteFingerprint string `json:"remote_fingerprint,omitempty"`
	// Match 报告本地与远端指纹是否一致（用于 UI 显示「无需同步」徽标）
	Match bool `json:"match"`
	// LastSyncedAt 记录上一次成功的同步时间（可能为空）
	LastSyncedAt string `json:"last_synced_at,omitempty"`
}

// webdavSyncRequest 是 push/pull 的请求体。
//
// 复用同一个结构：force=true 时跳过「与远端不一致」检查；direction 区分 push/pull。
type webdavSyncRequest struct {
	// Force=true 时跳过冲突检测（用于「用户已确认覆盖」场景）。
	Force bool `json:"force"`
}

// handleWebDAVStatus 返回 WebDAV 是否已配置、远端是否存在、本地与远端指纹比对。
func (s *Server) handleWebDAVStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.reqContext(r)
	defer cancel()

	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 WebDAV 配置失败", err)
		return
	}

	// 即便未配置也返回 200，由前端根据 configured 字段渲染空状态
	resp := webdavStatusResponse{Configured: cfg != nil}
	if cfg == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.URL = cfg.URL
	resp.Username = cfg.Username

	localBytes, err := s.exportProvidersFingerprintBytes(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "生成本地供应商导出失败", err)
		return
	}
	resp.LocalFingerprint = sync.HashBytes(localBytes)

	c := sync.New(*cfg, 0)
	remoteBytes, remoteSum, err := c.ReadRemote(ctx)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "探测远端 WebDAV 失败", err)
		return
	}
	if remoteBytes != nil {
		resp.HasRemote = true
		resp.RemoteFingerprint = remoteSum
		resp.Match = remoteSum == resp.LocalFingerprint
	}

	if last, _, _ := s.store.GetSetting(ctx, webdavLastSyncedKey); last != "" {
		resp.LastSyncedAt = last
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWebDAVPush(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.reqContext(r)
	defer cancel()

	var req webdavSyncRequest
	_ = decodeJSON(r, &req) // body 可选：空 body 不报错

	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 WebDAV 配置失败", err)
		return
	}
	if cfg == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "尚未配置 WebDAV：请在「网关设置」填写服务器地址、用户名、密码"})
		return
	}

	localBytes, err := s.exportProvidersFingerprintBytes(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "生成本地供应商导出失败", err)
		return
	}
	localSum := sync.HashBytes(localBytes)

	c := sync.New(*cfg, 0)
	expect := ""
	if !req.Force {
		expect = localSum
	}
	if err := c.WriteRemote(ctx, localBytes, expect); err != nil {
		var conf *sync.ErrConflict
		if errors.As(err, &conf) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":              err.Error(),
				"reason":             "remote_changed",
				"local_fingerprint":  localSum,
				"remote_fingerprint": conf.RemoteSHA256,
			})
			return
		}
		s.fail(w, http.StatusBadGateway, "推送失败", err)
		return
	}

	now := timeNow()
	_ = s.store.SetSetting(ctx, webdavLastSyncedKey, now)
	s.logger.Info("已通过 WebDAV 推送供应商配置",
		"bytes", len(localBytes), "fingerprint", localSum)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"direction":   "push",
		"bytes":       len(localBytes),
		"fingerprint": localSum,
		"synced_at":   now,
	})
}

func (s *Server) handleWebDAVPull(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.reqContext(r)
	defer cancel()

	var req webdavSyncRequest
	_ = decodeJSON(r, &req)

	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 WebDAV 配置失败", err)
		return
	}
	if cfg == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "尚未配置 WebDAV：请在「网关设置」填写服务器地址、用户名、密码"})
		return
	}

	c := sync.New(*cfg, 0)
	remoteBytes, remoteSum, err := c.ReadRemote(ctx)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "下载远端配置失败", err)
		return
	}
	if remoteBytes == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "远端文件不存在"})
		return
	}

	localBytes, err := s.exportProvidersFingerprintBytes(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "生成本地导出用于冲突检测失败", err)
		return
	}
	localSum := sync.HashBytes(localBytes)
	if !req.Force && localSum != remoteSum {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":              "本地与远端不同，请确认是否覆盖",
			"reason":             "local_changed",
			"local_fingerprint":  localSum,
			"remote_fingerprint": remoteSum,
		})
		return
	}

	imported, err := s.importProvidersBytes(ctx, remoteBytes)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "导入远端配置失败", err)
		return
	}
	if err := s.applyReload(ctx); err != nil {
		s.fail(w, http.StatusInternalServerError, "重建配置快照失败", err)
		return
	}
	now := timeNow()
	_ = s.store.SetSetting(ctx, webdavLastSyncedKey, now)
	s.logger.Info("已通过 WebDAV 拉取并导入供应商配置",
		"count", imported, "fingerprint", remoteSum)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"direction":   "pull",
		"imported":    imported,
		"fingerprint": remoteSum,
		"synced_at":   now,
	})
}

// handleWebDAVTest 测试 WebDAV 配置的连通性：
//
//   - 配置缺失：返回 400
//   - 远端文件存在 → HTTP 200，记录远端指纹
//   - 远端文件不存在 → HTTP 200，has_remote=false（仍是连接成功，只是首次推送前的合法情形）
//   - 凭证错误（401/403）→ 502，并附带 ErrUnauthorized 文案
//   - 其它 4xx/5xx → 502，附原始状态码
//   - 网络/DNS 错误 → 502
//
// 与 /status 的区别：本接口不计算本地指纹、不读本地数据库，只验证网络 + 凭证 + 远端可达。
// 主要用于「填完配置后想先确认能连上」的场景。
func (s *Server) handleWebDAVTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.reqContext(r)
	defer cancel()

	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 WebDAV 配置失败", err)
		return
	}
	if cfg == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "尚未配置 WebDAV：请先填写远程根目录、用户名、密码",
		})
		return
	}

	c := sync.New(*cfg, 0)
	remoteBytes, remoteSum, err := c.ReadRemote(ctx)
	if err != nil {
		// 凭据错误单独归一为 502 + 中文文案，方便前端展示
		if errors.Is(err, sync.ErrUnauthorized) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok":    false,
				"error": "认证失败：用户名或密码不正确",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	resp := map[string]any{
		"ok":       true,
		"url":      cfg.URL,
		"username": cfg.Username,
	}
	if remoteBytes != nil {
		resp["has_remote"] = true
		resp["remote_fingerprint"] = remoteSum
		resp["remote_bytes"] = len(remoteBytes)
	} else {
		resp["has_remote"] = false
	}
	writeJSON(w, http.StatusOK, resp)
}
