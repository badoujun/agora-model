package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/models"
	"agora-model/internal/store"
)

// Options 描述控制面的依赖。
type Options struct {
	Store         *store.Store
	Master        []byte
	Holder        *config.Holder
	Reload        func(ctx context.Context) (*config.Snapshot, error)
	Logger        *slog.Logger
	Version       string
	AdminPassword string
	// LocalOnly 报告网关是否只监听本机回环（决定未设密码时是否免登录）。
	LocalOnly bool
}

// Server 提供 Web UI 使用的管理接口（DESIGN §6）。
type Server struct {
	store     *store.Store
	master    []byte
	holder    *config.Holder
	reload    func(ctx context.Context) (*config.Snapshot, error)
	logger    *slog.Logger
	version   string
	localOnly bool
	auth      *Authenticator
	client    *http.Client
	fetcher   *models.Fetcher
}

// New 创建控制面服务。
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:     opts.Store,
		master:    opts.Master,
		holder:    opts.Holder,
		reload:    opts.Reload,
		logger:    logger,
		version:   opts.Version,
		localOnly: opts.LocalOnly,
		auth:      NewAuthenticator(opts.AdminPassword),
		fetcher:   models.NewFetcher(),
		client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:              nil,
				DisableCompression: true,
			},
		},
	}
}

// Register 注册全部 /api 路由。
func (s *Server) Register(mux *http.ServeMux) {
	// 鉴权与健康检查（无需会话）
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/session", s.handleSession)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// 供应商
	mux.HandleFunc("GET /api/providers", s.guard(s.handleListProviders))
	mux.HandleFunc("POST /api/providers", s.guard(s.handleCreateProvider))
	mux.HandleFunc("GET /api/providers/{id}", s.guard(s.handleGetProvider))
	mux.HandleFunc("PUT /api/providers/{id}", s.guard(s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", s.guard(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", s.guard(s.handleTestProvider))
	mux.HandleFunc("POST /api/providers/{id}/fetch-models", s.guard(s.handleFetchModels))

	// 模型
	mux.HandleFunc("GET /api/models", s.guard(s.handleListModels))

	// 设置与网关 Key
	mux.HandleFunc("GET /api/settings", s.guard(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.guard(s.handleUpdateSettings))
	mux.HandleFunc("GET /api/gateway-key", s.guard(s.handleGetGatewayKey))
	mux.HandleFunc("POST /api/gateway-key/reset", s.guard(s.handleResetGatewayKey))

	// 日志与配置导入导出
	mux.HandleFunc("GET /api/logs", s.guard(s.handleListLogs))
	mux.HandleFunc("GET /api/export", s.guard(s.handleExport))
	mux.HandleFunc("POST /api/import", s.guard(s.handleImport))
}

// guard 包装需要授权的处理器。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.authorized(r, isLoopbackRemote(r)) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未授权：请先登录"})
			return
		}
		next(w, r)
	}
}

// ---------------- 鉴权 ----------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.auth.LoginEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "未启用登录（仅本机回环使用时不设管理密码）",
		})
		return
	}
	token, expires, ok := s.auth.Login(body.Password)
	if !ok {
		s.logger.Warn("Web UI 登录失败", "client_ip", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "管理密码不正确"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires.UTC().Format(time.RFC3339)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(sessionToken(r))
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"login_required": s.auth.LoginEnabled(),
		"authenticated":  s.auth.authorized(r, isLoopbackRemote(r)),
		"local_only":     s.localOnly,
	})
}

// ---------------- 健康检查 ----------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	providers := len(s.holder.Get().Providers())
	modelSet := map[string]bool{}
	for _, p := range s.holder.Get().Providers() {
		for _, m := range p.ExposedModels() {
			modelSet[m] = true
		}
	}
	logs, err := s.store.CountLogs(ctx)
	if err != nil {
		s.logger.Debug("统计日志条数失败", "err", err)
	}
	written, dropped := int64(0), int64(0)
	if rec := s.logRecorderStats(); rec != nil {
		written, dropped = rec()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version,
		"providers":      providers,
		"models":         len(modelSet),
		"logs":           logs,
		"logs_written":   written,
		"logs_dropped":   dropped,
		"login_required": s.auth.LoginEnabled(),
	})
}

// ---------------- 导出 / 导入 ----------------

// exportPayload 是 /api/export 的响应体（不含任何明文凭证）。
type exportPayload struct {
	Gateway   config.Gateway    `json:"gateway"`
	Providers []providerDTO     `json:"providers"`
	Notes     map[string]string `json:"notes,omitempty"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	records, err := s.store.ListProviders(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}

	out := exportPayload{
		Gateway:   s.holder.Get().Gateway(),
		Providers: make([]providerDTO, 0, len(records)),
		Notes: map[string]string{
			"api_key": "出于安全考虑未导出供应商凭证；导入后需重新填写（或用 GW_MASTER_KEY + 数据库文件迁移）",
		},
	}
	for _, rec := range records {
		out.Providers = append(out.Providers, toDTO(rec, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var payload struct {
		Providers []providerInput `json:"providers"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	imported := 0
	for _, in := range payload.Providers {
		in := in
		apiKey := ""
		if in.APIKey != nil {
			apiKey = strings.TrimSpace(*in.APIKey)
		}
		if apiKey == "" {
			// 导入时凭证必填（导出不含明文）
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("供应商 %q 缺少 api_key：导出文件不含明文凭证，导入时需补齐", in.displayName()),
			})
			return
		}
		if _, err := s.upsertProvider(ctx, in, apiKey); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		imported++
	}

	if err := s.applyReload(ctx); err != nil {
		s.fail(w, http.StatusInternalServerError, "重建配置快照失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported})
}

// ---------------- 内部辅助 ----------------

// applyReload 重建配置快照（供应商/模型变更后调用）。
func (s *Server) applyReload(ctx context.Context) error {
	if s.reload == nil {
		return nil
	}
	snap, err := s.reload(ctx)
	if err != nil {
		return err
	}
	s.holder.Store(snap)
	return nil
}

// logRecorderStats 允许外部注入日志统计（main 侧设置）。
func (s *Server) logRecorderStats() func() (int64, int64) {
	return statsFunc
}

// SetLogStatsFunc 由 main 注入请求日志计数器的读取函数。
func SetLogStatsFunc(fn func() (int64, int64)) { statsFunc = fn }

var statsFunc func() (int64, int64)

func (s *Server) fail(w http.ResponseWriter, status int, message string, err error) {
	s.logger.Error(message, "err", err)
	writeJSON(w, status, map[string]any{"error": message + ": " + err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("请求体为空")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

// newProviderID 生成供应商 id。
func newProviderID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "p_" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))[:12]
	}
	return "p_" + hex.EncodeToString(buf)
}

// maskKey 供 DTO 使用的掩码函数。
func maskKey(plain string) string { return crypto.Mask(plain) }
