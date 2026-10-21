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
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/models"
	"agora-model/internal/presets"
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
	// Paths 是「数据位置」页面展示的本地落点（由 main 按实际启动参数填充）。
	Paths DataPaths
	// Presets 提供「国内常用供应商」预设列表；为空时 /api/provider-presets 返回 503。
	Presets *presets.Store
}

// DataPaths 描述本程序在磁盘上产生的数据位置。
type DataPaths struct {
	// DataDir 是数据目录（数据库、主密钥、日志都在其下）。
	DataDir string
	// DBPath 是 SQLite 数据库文件路径。
	DBPath string
	// MasterKeyPath 是 AES 主密钥文件路径（主密钥也可能来自环境变量）。
	MasterKeyPath string
	// LogFile 是日志文件路径；空表示前台运行写标准输出。
	LogFile string
	// ServiceBinary 是服务模式下运行的可执行文件副本路径。
	ServiceBinary string
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
	paths     DataPaths
	auth      *Authenticator
	client    *http.Client
	fetcher   *models.Fetcher
	presets   *presets.Store
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
		paths:     opts.Paths,
		auth:      NewAuthenticator(opts.AdminPassword),
		fetcher:   models.NewFetcher(),
		presets:   opts.Presets,
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
	// 单个凭证的连接测试（含已停用的 Key，便于判断能否重新启用）
	mux.HandleFunc("POST /api/providers/{id}/keys/{key_id}/test", s.guard(s.handleTestProviderKey))
	// 尚未保存的供应商（草稿）也可以直接拉取模型候选 / 测试凭证
	mux.HandleFunc("POST /api/providers/fetch-models", s.guard(s.handleFetchDraftModels))
	mux.HandleFunc("POST /api/providers/test-key", s.guard(s.handleTestDraftKey))

	// 模型
	mux.HandleFunc("GET /api/models", s.guard(s.handleListModels))

	// 设置与网关 Key
	mux.HandleFunc("GET /api/settings", s.guard(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.guard(s.handleUpdateSettings))
	// 多网关 Key：列表（含已删除）/ 新建 / 更新 / 软删除 / 查看明文 / 重置明文
	mux.HandleFunc("GET /api/gateway-keys", s.guard(s.handleListGatewayKeys))
	mux.HandleFunc("POST /api/gateway-keys", s.guard(s.handleCreateGatewayKey))
	mux.HandleFunc("PUT /api/gateway-keys/{id}", s.guard(s.handleUpdateGatewayKey))
	mux.HandleFunc("DELETE /api/gateway-keys/{id}", s.guard(s.handleDeleteGatewayKey))
	mux.HandleFunc("GET /api/gateway-keys/{id}/reveal", s.guard(s.handleRevealGatewayKey))
	mux.HandleFunc("POST /api/gateway-keys/{id}/reset", s.guard(s.handleResetGatewayKeyByID))
	// 兼容旧接口：指向「默认 Key」（最早创建的启用 Key）
	mux.HandleFunc("GET /api/gateway-key", s.guard(s.handleGetGatewayKey))
	mux.HandleFunc("POST /api/gateway-key/reset", s.guard(s.handleResetGatewayKey))

	// 日志与配置导入导出
	mux.HandleFunc("GET /api/logs", s.guard(s.handleListLogs))
	mux.HandleFunc("GET /api/export", s.guard(s.handleExport))
	mux.HandleFunc("POST /api/import", s.guard(s.handleImport))

	// 数据位置（本机落盘路径）
	mux.HandleFunc("GET /api/data-locations", s.guard(s.handleDataLocations))

	// 使用统计（前端「使用统计」页面读取）
	mux.HandleFunc("GET /api/usage", s.guard(s.handleUsage))

	// 预设供应商（国内常用，编译期内置；用户可在 docs/provider-presets.json 覆盖）
	mux.HandleFunc("GET /api/provider-presets", s.guard(s.handleListProviderPresets))

	// WebDAV 同步：状态探测 / 测试连接 / 推送 / 拉取
	mux.HandleFunc("GET /api/sync/webdav/status", s.guard(s.handleWebDAVStatus))
	mux.HandleFunc("GET /api/sync/webdav/test", s.guard(s.handleWebDAVTest))
	mux.HandleFunc("POST /api/sync/webdav/push", s.guard(s.handleWebDAVPush))
	mux.HandleFunc("POST /api/sync/webdav/pull", s.guard(s.handleWebDAVPull))

	// WebDAV 配置读写
	mux.HandleFunc("GET /api/settings/webdav", s.guard(s.handleGetWebDAVConfig))
	mux.HandleFunc("PUT /api/settings/webdav", s.guard(s.handleUpdateWebDAVConfig))
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

// exportPayload 是 /api/export 的响应体。
//
// 注意：供应商的 api_key 以**明文**导出（便于换机迁移），导出文件等同于凭证载体。
type exportPayload struct {
	Format     string             `json:"format"`
	ExportedAt string             `json:"exported_at"`
	Gateway    config.Gateway     `json:"gateway"`
	Providers  []exportedProvider `json:"providers"`
	Notes      map[string]string  `json:"notes,omitempty"`
}

// exportedProviderKey 是导出文件里的单个凭证（明文）。
type exportedProviderKey struct {
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Enabled bool   `json:"enabled"`
}

// exportedProvider 是导出/迁移用的稳定结构。
//
//   - 只含配置字段（不含 Key id、用量与失败统计），保证 WebDAV 指纹只随配置变化；
//   - keys 携带明文凭证；api_key 是兼容字段（首个启用 Key 的明文），
//     供旧版本导入（旧版本只知道单凭证格式）。
type exportedProvider struct {
	ID                     string                `json:"id"`
	Name                   string                `json:"name"`
	WebsiteURL             string                `json:"website_url"`
	OpenAIBaseURL          string                `json:"openai_base_url"`
	OpenAIEndpointOverride string                `json:"openai_endpoint_override"`
	TimeoutSeconds         int                   `json:"timeout_seconds"`
	ExtraHeaders           map[string]string     `json:"extra_headers,omitempty"`
	ExtraBody              map[string]any        `json:"extra_body,omitempty"`
	AllowInternal          bool                  `json:"allow_internal"`
	Enabled                bool                  `json:"enabled"`
	ModelsSelected         []string              `json:"models_selected"`
	ModelAliases           map[string]string     `json:"model_aliases"`
	Keys                   []exportedProviderKey `json:"keys"`
	APIKey                 string                `json:"api_key"`
}

// exportFormat 标识导出文件格式，便于导入时判断与后续演进。
const exportFormat = "agoramodel.providers/v1"

// exportNotes 是导出文件里的固定提示（不含时间，保持指纹稳定）。
func exportNotes() map[string]string {
	return map[string]string{
		"api_key": "导出文件包含所有供应商的 API Key 明文（keys[] 与兼容字段 api_key），请按凭证同等对待；导入时会用目标机器的密钥重新加密",
	}
}

// buildExportPayload 生成导出载荷（/api/export、WebDAV 推送与指纹共用）。
//
// 顺序完全由数据库决定（供应商按名称、Key 按创建时间），因此同一份配置
// 每次导出的字节一致；只有 exported_at 需要调用方抹平后才能用作指纹。
func (s *Server) buildExportPayload(ctx context.Context, exportedAt string) (exportPayload, error) {
	records, err := s.store.ListProviders(ctx)
	if err != nil {
		return exportPayload{}, err
	}
	out := exportPayload{
		Format:     exportFormat,
		ExportedAt: exportedAt,
		Gateway:    s.holder.Get().Gateway(),
		Providers:  make([]exportedProvider, 0, len(records)),
		Notes:      exportNotes(),
	}
	for _, rec := range records {
		ep := exportedProvider{
			ID:                     rec.ID,
			Name:                   rec.Name,
			WebsiteURL:             rec.WebsiteURL,
			OpenAIBaseURL:          rec.OpenAIBaseURL,
			OpenAIEndpointOverride: rec.OpenAIEndpointOverride,
			TimeoutSeconds:         rec.TimeoutSeconds,
			ExtraHeaders:           rec.ExtraHeaders,
			ExtraBody:              rec.ExtraBody,
			AllowInternal:          rec.AllowInternal,
			Enabled:                rec.Enabled,
			ModelsSelected:         rec.ModelsSelected,
			ModelAliases:           rec.ModelAliases,
			Keys:                   make([]exportedProviderKey, 0, len(rec.Keys)),
		}
		for _, k := range rec.Keys {
			plain, derr := crypto.Decrypt(s.master, k.KeyCipher, rec.ID)
			if derr != nil {
				return exportPayload{}, fmt.Errorf("解密供应商 %q 的凭证失败: %w", rec.Name, derr)
			}
			text := string(plain)
			ep.Keys = append(ep.Keys, exportedProviderKey{Label: k.Label, APIKey: text, Enabled: k.Enabled})
			if ep.APIKey == "" && k.Enabled {
				ep.APIKey = text
			}
		}
		if ep.APIKey == "" && len(ep.Keys) > 0 {
			ep.APIKey = ep.Keys[0].APIKey
		}
		out.Providers = append(out.Providers, ep)
	}
	return out, nil
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	out, err := s.buildExportPayload(ctx, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "导出供应商配置失败", err)
		return
	}
	s.logger.Info("已导出供应商配置（含明文凭证）", "count", len(out.Providers))
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
	if len(payload.Providers) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "导入文件里没有 providers 数组（或数组为空）"})
		return
	}

	imported := 0
	for _, in := range payload.Providers {
		if _, err := s.upsertProvider(ctx, in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("导入供应商 %q 失败：%v", in.displayName(), err),
			})
			return
		}
		imported++
	}

	if err := s.applyReload(ctx); err != nil {
		s.fail(w, http.StatusInternalServerError, "重建配置快照失败", err)
		return
	}
	s.logger.Info("已导入供应商配置", "count", imported)
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
