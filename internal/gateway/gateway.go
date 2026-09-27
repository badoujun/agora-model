package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/logging"
	"agora-model/internal/route"
)

// errBodyTooLarge 表示入站请求体超过上限。
var errBodyTooLarge = errors.New("请求体过大")

// KeyStore 是网关 Key 的校验与使用记录接口（由 store 实现）。
//
// 为 nil 时退化为使用配置中的静态 Key（Phase 1 行为，便于单测与最小部署）。
type KeyStore interface {
	VerifyGatewayKey(ctx context.Context, provided string) (keyID string, ok bool, err error)
	TouchGatewayKey(ctx context.Context, id string) error
}

// Gateway 实现双协议原样透传的数据面。
type Gateway struct {
	holder    *config.Holder
	client    *http.Client
	logger    *slog.Logger
	keys      KeyStore
	recorder  *logging.Recorder
	lastTouch atomic.Int64 // 网关 Key 使用时间的节流（Unix 秒）
}

// New 创建网关。
//
// 出站 Transport 刻意禁用代理与压缩：
//   - Proxy=nil：企业环境的 HTTP_PROXY 会使实际连接目标与 SSRF 校验对象不一致（DESIGN §9）；
//   - DisableCompression=true：让上游返回未压缩内容，SSE 逐块透传的字节流更干净。
//
// 不使用 http.Client.Timeout：它会把长时间推理的流一并掐断，
// 超时改由每个请求的 context 控制（provider.timeout_seconds）。
func New(holder *config.Holder, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Gateway{
		holder: holder,
		client: &http.Client{Transport: transport},
		logger: logger,
	}
}

// WithKeyStore 注入网关 Key 存储（Phase 2 起使用）。
func (g *Gateway) WithKeyStore(ks KeyStore) *Gateway {
	g.keys = ks
	return g
}

// WithRecorder 注入请求日志记录器。
func (g *Gateway) WithRecorder(r *logging.Recorder) *Gateway {
	g.recorder = r
	return g
}

// Register 注册数据面端点。
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc(config.ChatCompletionsPath, g.handler())
	mux.HandleFunc(config.ModelsPath, g.modelsHandler())
}

// modelEntry 是 /v1/models 返回的单条模型。
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// modelsHandler 返回聚合后的模型列表（DESIGN §5.5）。
//
//   - 裸模型名：owned_by 为按名称排序后第一个提供该模型的供应商（即默认路由目标）；
//   - 命名空间形式 供应商名/模型名：每个提供该模型的供应商各列一条，便于显式指定；
//   - 模型对外名即配置的别名（未配置别名时等于上游模型名）。
func (g *Gateway) modelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, codeNotFound,
				fmt.Sprintf("%s 仅支持 GET，收到 %s", config.ModelsPath, r.Method), "")
			return
		}
		if !g.authorize(r) {
			writeError(w, http.StatusUnauthorized, codeInvalidAPIKey,
				"网关 API Key 无效或缺失", "")
			return
		}

		snap := g.holder.Get()
		providers := snap.Providers()

		// 裸名：Providers 已按供应商名称升序，首个出现者即默认供应商
		seen := make(map[string]bool)
		data := make([]modelEntry, 0, 16)
		for _, p := range providers {
			for _, exposed := range p.ExposedModels() {
				if seen[exposed] {
					continue
				}
				seen[exposed] = true
				data = append(data, modelEntry{ID: exposed, Object: "model", OwnedBy: p.Name})
			}
		}
		// 命名空间形式
		for _, p := range providers {
			for _, exposed := range p.ExposedModels() {
				data = append(data, modelEntry{ID: p.Name + "/" + exposed, Object: "model", OwnedBy: p.Name})
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

// handler 返回数据面处理器（DESIGN §7.1 主流程）。
func (g *Gateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newRequestID()
		snap := g.holder.Get()
		cfg := snap.Gateway()

		// 包装 ResponseWriter：统一捕获状态码与首字节时间，供请求日志使用
		rec := &statusWriter{ResponseWriter: w, started: started}
		w = rec

		var (
			model        string
			providerID   string
			upstreamURL  string
			streamServed bool
		)
		defer func() {
			g.record(logging.Entry{
				TS:              started,
				RequestID:       requestID,
				InboundProtocol: config.ProtocolName,
				Model:           model,
				ProviderID:      providerID,
				UpstreamURL:     upstreamURL,
				StatusCode:      rec.statusCode(),
				LatencyMS:       time.Since(started).Milliseconds(),
				FirstByteMS:     rec.firstByteMS(),
				Stream:          streamServed,
				ErrorMsg:        rec.errorMessage(),
				ClientIP:        clientIP(r),
			})
		}()

		if r.Method != http.MethodPost {
			writeError(w, http.StatusNotFound, codeNotFound,
				fmt.Sprintf("%s 仅支持 POST，收到 %s", config.ChatCompletionsPath, r.Method), "")
			return
		}

		// 1) 入站鉴权
		if !g.authorize(r) {
			g.logger.Warn("网关鉴权失败",
				"request_id", requestID, "path", config.ChatCompletionsPath, "client_ip", clientIP(r))
			writeError(w, http.StatusUnauthorized, codeInvalidAPIKey,
				"网关 API Key 无效或缺失", "")
			return
		}

		// 2) 读取请求体（含上限）
		body, err := readBody(r, cfg.MaxBodyBytes)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
					fmt.Sprintf("请求体超过上限 %d 字节", cfg.MaxBodyBytes), "")
				return
			}
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				"读取请求体失败: "+err.Error(), "")
			return
		}

		// 3) 探测 model 字段
		model, err = probeModel(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error(), "model")
			return
		}

		// 4) 路由决策
		decision, err := route.Resolve(snap, model)
		if err != nil {
			rec.noteError(err.Error())
			switch {
			case errors.Is(err, route.ErrModelNotFound):
				writeError(w, http.StatusNotFound, codeModelNotFound, err.Error(), "model")
			case errors.Is(err, route.ErrProviderDisabled):
				writeError(w, http.StatusServiceUnavailable, codeNoAvailableProvider, err.Error(), "")
			default:
				writeError(w, http.StatusInternalServerError, codeInvalidRequest, err.Error(), "")
			}
			g.logger.Warn("路由失败",
				"request_id", requestID, "model", model, "err", err)
			return
		}
		providerID = decision.Provider.ID
		upstreamURL = decision.Upstream
		streamServed = isStreamRequest(body)

		// 5) 组装上游请求体（无 extra_body 且不改写 model 时零改写）
		upstreamBody, err := prepareBody(body, decision, model)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error(), "")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), decision.Provider.Timeout())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, decision.Upstream, bytes.NewReader(upstreamBody))
		if err != nil {
			writeError(w, http.StatusBadGateway, codeUpstreamUnreachable,
				"构造上游请求失败: "+err.Error(), "")
			return
		}
		req.Header = buildUpstreamHeaders(r.Header, decision.Provider)
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		g.logger.Debug("路由决策",
			"request_id", requestID, "model", model, "provider", decision.Provider.Name,
			"upstream_model", decision.Model, "upstream", decision.Upstream,
			"stream", streamServed)

		// 6) 发送并透传响应
		resp, err := g.client.Do(req)
		if err != nil {
			rec.noteError(err.Error())
			g.logUpstreamFailure(requestID, model, decision, started, err)
			g.writeUpstreamError(w, r, err)
			return
		}
		defer resp.Body.Close()

		if isEventStream(resp) {
			streamServed = true
			stats, serr := streamResponse(r.Context(), w, resp, cfg.SSEIdle(), started)
			g.logCompletion(requestID, model, decision, resp.StatusCode, started, stats, serr)
			return
		}

		written, cerr := copyResponse(w, resp)
		g.logger.Info("请求完成",
			"request_id", requestID, "model", model,
			"provider", decision.Provider.Name, "status", resp.StatusCode,
			"bytes", written, "latency_ms", time.Since(started).Milliseconds(),
			"stream", false, "err", errString(cerr))
	}
}

// authorize 校验入站凭证：Phase 2 起查 gateway_keys 表，否则使用配置中的静态 Key。
func (g *Gateway) authorize(r *http.Request) bool {
	got := tokenFromRequest(r)
	if got == "" {
		return false
	}
	if g.keys != nil {
		keyID, ok, err := g.keys.VerifyGatewayKey(r.Context(), got)
		if err != nil {
			g.logger.Warn("网关 Key 校验失败", "err", err)
			return false
		}
		if ok {
			g.touchKey(keyID)
		}
		return ok
	}
	want := g.holder.Get().Gateway().APIKey
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// touchKey 记录网关 Key 的最后使用时间；节流到最多每分钟一次，且不阻塞请求。
func (g *Gateway) touchKey(keyID string) {
	if keyID == "" {
		return
	}
	now := time.Now().Unix()
	last := g.lastTouch.Load()
	if now-last < 60 {
		return
	}
	if !g.lastTouch.CompareAndSwap(last, now) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := g.keys.TouchGatewayKey(ctx, keyID); err != nil {
			g.logger.Debug("更新网关 Key 使用时间失败", "err", err)
		}
	}()
}

// record 投递一条请求日志（失败必记由调用方保证：所有分支都经过 defer）。
func (g *Gateway) record(e logging.Entry) {
	if g.recorder == nil {
		return
	}
	g.recorder.Enqueue(e)
}

// statusWriter 包装 ResponseWriter，捕获状态码、首字节时间与错误摘要。
//
// Unwrap 让 http.ResponseController 能找到底层实现（Flush / SetWriteDeadline）。
type statusWriter struct {
	http.ResponseWriter
	started   time.Time
	status    int
	firstByte time.Duration
	errMsg    string
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		s.markFirstByte()
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
		s.markFirstByte()
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap 暴露底层 ResponseWriter（http.ResponseController 依赖）。
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) markFirstByte() {
	if s.firstByte == 0 {
		s.firstByte = time.Since(s.started)
	}
}

// statusCode 返回最终状态码；未写出任何响应（如客户端断开）记为 499。
func (s *statusWriter) statusCode() int {
	if s.status == 0 {
		return 499
	}
	return s.status
}

func (s *statusWriter) firstByteMS() int64 { return s.firstByte.Milliseconds() }

func (s *statusWriter) noteError(msg string) { s.errMsg = msg }

func (s *statusWriter) errorMessage() string { return s.errMsg }

// prepareBody 按需重写 JSON body：
//   - 无 extra_body 且无需改写 model → 原样返回（零改写，保真度最高）
//   - 否则用 map[string]json.RawMessage 做最小改写：只替换 model 键、合并 extra_body 键，
//     其余字段的原始字节完全保留
func prepareBody(raw []byte, decision route.Result, originalModel string) ([]byte, error) {
	extra := decision.Provider.ExtraBody
	needModelRewrite := decision.Model != originalModel
	if len(extra) == 0 && !needModelRewrite {
		return raw, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("请求体不是 JSON 对象")
	}
	if needModelRewrite {
		encoded, err := json.Marshal(decision.Model)
		if err != nil {
			return nil, err
		}
		obj["model"] = encoded
	}
	for key, value := range extra {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("extra_body[%s] 无法序列化: %w", key, err)
		}
		obj[key] = encoded
	}
	return json.Marshal(obj)
}

// isStreamRequest 报告请求体是否要求流式响应（仅用于日志）。
func isStreamRequest(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

func tokenFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return ""
}

// readBody 读取入站请求体并施加大小上限。
func readBody(r *http.Request, max int64) ([]byte, error) {
	if max <= 0 {
		max = config.DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// probeModel 只解析出 model 字段，避免为大 body 构造完整结构体。
func probeModel(body []byte) (string, error) {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", errors.New("请求体不是合法 JSON")
	}
	model := strings.TrimSpace(probe.Model)
	if model == "" {
		return "", errors.New("请求体缺少 model 字段")
	}
	return model, nil
}

// writeUpstreamError 把出站错误按类型映射为响应；客户端断开时只记日志、不写响应。
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, codeUpstreamTimeout,
			"上游请求超时", "")
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			writeError(w, http.StatusGatewayTimeout, codeUpstreamTimeout,
				"上游请求超时: "+err.Error(), "")
			return
		}
		writeError(w, http.StatusBadGateway, codeUpstreamUnreachable,
			"连接上游失败: "+err.Error(), "")
	}
}

func (g *Gateway) logUpstreamFailure(requestID, model string, d route.Result, started time.Time, err error) {
	status := "upstream_error"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "upstream_timeout"
	}
	g.logger.Error("上游请求失败",
		"request_id", requestID, "model", model,
		"provider", d.Provider.Name, "upstream_model", d.Model, "upstream", d.Upstream,
		"status", status, "latency_ms", time.Since(started).Milliseconds(), "err", err)
}

func (g *Gateway) logCompletion(requestID, model string, d route.Result, status int, started time.Time, stats streamStats, err error) {
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	g.logger.Log(context.Background(), level, "流式请求完成",
		"request_id", requestID, "model", model,
		"provider", d.Provider.Name, "status", status,
		"bytes", stats.bytes, "first_byte_ms", stats.firstByteIn.Milliseconds(),
		"latency_ms", time.Since(started).Milliseconds(), "stream", true,
		"err", errString(err))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
