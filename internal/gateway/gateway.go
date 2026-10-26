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
	"sync"
	"sync/atomic"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
	"agora-model/internal/route"
)

// errBodyTooLarge 表示入站请求体超过上限。
var errBodyTooLarge = errors.New("请求体过大")

// errNoUsableKey 表示供应商没有任何可用的 API Key（全部停用/被自动停用）。
var errNoUsableKey = errors.New("供应商没有可用的 API Key（已全部停用或自动停用，请在供应商页重新启用）")

// ProviderKeyStats 是凭证健康状态的下游接收方（由 store.ProviderKeyStatsRecorder 实现）。
//
// 两个方法都必须是非阻塞的：数据面只在内存里做原子标记，落库交给异步 recorder。
// DisableProviderKey 带 providerID：落库时若该供应商已没有任何启用的 Key，
// 会一并停用供应商（见 store.ProviderKeyStatsRecorder.flushBatch）。
type ProviderKeyStats interface {
	TouchProviderKey(keyID string)
	DisableProviderKey(providerID, keyID, reason string)
}

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
	usageSink UsageSink
	// keyRuntime 是供应商 Key 的进程内健康状态（轮询起点/自动停用），
	// 请求路径只读它和配置快照，不碰数据库（DESIGN §7.10）。
	keyRuntime *keyRuntime
	keyStats   ProviderKeyStats
	// lastSnap 记录最近一次"被请求路径观察到"的快照，用于在新快照生效时
	// 复位被用户重新启用的 Key（见 observe）。
	lastSnap atomic.Pointer[config.Snapshot]
	// keyTouch 是「网关 Key id → 上次写入 last_used_at 的 Unix 秒」的节流表。
	//
	// 必须按 Key 分开：多 Key 同时启用时，若共用一个全局时间戳，先到的 Key
	// 会把窗口占掉，其余 Key 的 last_used_at 永远不更新。
	keyTouch sync.Map // map[string]*atomic.Int64
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
		holder:     holder,
		client:     &http.Client{Transport: transport},
		logger:     logger,
		keyRuntime: newKeyRuntime(),
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

// WithProviderKeyStats 注入凭证健康状态接收器（异步落库 last_used/失败/自动停用）。
func (g *Gateway) WithProviderKeyStats(s ProviderKeyStats) *Gateway {
	g.keyStats = s
	return g
}

// WithUsageSink 注入 token 用量接收器（前端「使用统计」页面读取）。
//
// 不传时使用 noop sink（丢弃记录），保持现有调用代码不 nil 检查。
func (g *Gateway) WithUsageSink(s UsageSink) *Gateway {
	if s == nil {
		s = noopUsageSink{}
	}
	g.usageSink = s
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
//   - 只返回命名空间形式 供应商名/模型名：每个提供该模型的供应商各列一条，
//     便于调用方显式指定；不再返回裸模型名（裸名无法用于调用）；
//   - 模型对外名即配置的别名（未配置别名时等于上游模型名）。
func (g *Gateway) modelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, codeNotFound,
				fmt.Sprintf("%s 仅支持 GET，收到 %s", config.ModelsPath, r.Method), "")
			return
		}
		keyID, ok := g.authorize(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, codeInvalidAPIKey,
				"网关 API Key 无效或缺失", "")
			return
		}

		snap := g.holder.Get()
		providers := snap.Providers()
		// /v1/models 只列出调用者 Key 范围内的模型：否则 Agent 会拿到
		// 一份包含无权模型的清单，选中后请求才被 403 拒绝。
		scope := snap.GatewayKeyScope(keyID)

		// 没有可用 Key 的供应商不对外暴露模型（含被自动停用的场景）。
		data := make([]modelEntry, 0, 16)
		for _, p := range providers {
			if !p.HasUsableKey() {
				continue
			}
			for _, exposed := range p.ExposedModels() {
				if !scope.AllowsExposed(p, exposed) {
					continue
				}
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
		g.observe(snap)
		cfg := snap.Gateway()

		// 包装 ResponseWriter：统一捕获状态码与首字节时间，供请求日志使用
		rec := &statusWriter{ResponseWriter: w, started: started}
		w = rec

		var (
			model        string
			providerID   string
			upstreamURL  string
			keyHint      string
			gatewayKeyID string
			streamServed bool
		)
		defer func() {
			g.record(logging.Entry{
				TS:              started,
				RequestID:       requestID,
				InboundProtocol: config.ProtocolName,
				Model:           model,
				ProviderID:      providerID,
				KeyHint:         keyHint,
				GatewayKeyID:    gatewayKeyID,
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

		// 1) 入站鉴权（回传命中的网关 Key id，供模型范围判定与日志归属）
		gkID, ok := g.authorize(r)
		if !ok {
			g.logger.Warn("网关鉴权失败",
				"request_id", requestID, "path", config.ChatCompletionsPath, "client_ip", clientIP(r))
			writeError(w, http.StatusUnauthorized, codeInvalidAPIKey,
				"网关 API Key 无效或缺失", "")
			return
		}
		gatewayKeyID = gkID

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
		decision, err := route.ResolveScoped(snap, model, snap.GatewayKeyScope(gatewayKeyID))
		if err != nil {
			rec.noteError(err.Error())
			switch {
			case errors.Is(err, route.ErrModelNotNamespaced):
				// 模型名格式不符合契约（缺少 供应商名/ 前缀）：客户端改请求即可，
				// 属于参数错误而不是 404。
				writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error(), "model")
			case errors.Is(err, route.ErrModelNotFound):
				writeError(w, http.StatusNotFound, codeModelNotFound, err.Error(), "model")
			case errors.Is(err, route.ErrModelNotAllowed):
				// 模型存在，但不在当前网关 Key 的模型范围内：403 权限问题，
				// 不能报 404，否则用户会以为是配置缺失而去翻供应商页。
				writeError(w, http.StatusForbidden, codeModelNotAllowed, err.Error(), "model")
			case errors.Is(err, route.ErrProviderDisabled), errors.Is(err, route.ErrProviderNoKey):
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

		g.logger.Debug("路由决策",
			"request_id", requestID, "model", model, "provider", decision.Provider.Name,
			"upstream_model", decision.Model, "upstream", decision.Upstream,
			"stream", streamServed)

		// 6) 发送并透传响应：多 Key 轮询 + 凭证类失败自动切换/自动停用（DESIGN §7.10）
		resp, usedKey, keyFailures, err := g.sendWithFailover(ctx, r, decision, upstreamBody, requestID)
		// 失败明细无论最终是否成功都写入请求日志：后续 Key 成功时客户端看到 200，
		// 但日志页仍能看到"哪个凭证因什么原因失败、网关做了什么"。
		if summary := describeKeyFailures(keyFailures); summary != "" {
			rec.noteError(logging.Redact(summary, 1000))
		}
		if err != nil {
			rec.noteError(err.Error())
			if errors.Is(err, errNoUsableKey) {
				writeError(w, http.StatusServiceUnavailable, codeNoAvailableProvider, err.Error(), "")
				return
			}
			g.logUpstreamFailure(requestID, model, decision, started, err)
			g.writeUpstreamError(w, r, err)
			return
		}
		keyHint = crypto.Mask(usedKey.Key)
		defer resp.Body.Close()

		if isEventStream(resp) {
			streamServed = true
			stats, usage, serr := streamResponse(r.Context(), w, resp, cfg.SSEIdle(), started)
			g.logCompletion(requestID, model, decision, resp.StatusCode, started, stats, serr)
			if u, ok := usage.Result(); ok {
				recordUsage(g.usageSink, gatewayKeyID, decision.Provider.ID, usedKey.ID, decision.Model, resp.StatusCode, u, started)
			}
			return
		}

		// 非流式：用 TeeReader 把字节同时给客户端和副本缓冲，解析后 recordUsage。
		var bodyBuf bytes.Buffer
		written, cerr := copyResponse(w, resp, &bodyBuf)
		if cerr == nil && bodyBuf.Len() > 0 {
			if u, ok := parseUsageNonStream(resp.Header.Get("Content-Type"), bodyBuf.Bytes()); ok {
				recordUsage(g.usageSink, gatewayKeyID, decision.Provider.ID, usedKey.ID, decision.Model, resp.StatusCode, u, started)
			}
		}
		g.logger.Info("请求完成",
			"request_id", requestID, "model", model,
			"provider", decision.Provider.Name, "status", resp.StatusCode,
			"bytes", written, "latency_ms", time.Since(started).Milliseconds(),
			"stream", false, "err", errString(cerr))
	}
}

// authorize 校验入站凭证，返回命中的网关 Key id。
//
// Phase 2 起查 gateway_keys 表（同时支持启用多个 Key），否则退化为
// 配置中的静态 Key（此时没有 Key id，返回空串）。
// 返回的 keyID 用于路由时的模型范围判定、请求日志与用量归属。
func (g *Gateway) authorize(r *http.Request) (keyID string, ok bool) {
	got := tokenFromRequest(r)
	if got == "" {
		return "", false
	}
	if g.keys != nil {
		id, ok, err := g.keys.VerifyGatewayKey(r.Context(), got)
		if err != nil {
			g.logger.Warn("网关 Key 校验失败", "err", err)
			return "", false
		}
		if ok {
			g.touchKey(id)
		}
		return id, ok
	}
	want := g.holder.Get().Gateway().APIKey
	if want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		return "", true
	}
	return "", false
}

// touchKey 记录网关 Key 的最后使用时间；按 Key 各自节流到最多每分钟一次，
// 且不阻塞请求。
//
// 节流必须按 Key 独立：多 Key 同时启用时共用一个时间戳会让先到的 Key 占掉
// 整个窗口，其余 Key 的 last_used_at 永远不更新，控制台就看不到它们在被使用。
func (g *Gateway) touchKey(keyID string) {
	if keyID == "" || g.keys == nil {
		return
	}
	now := time.Now().Unix()
	val, _ := g.keyTouch.LoadOrStore(keyID, &atomic.Int64{})
	last := val.(*atomic.Int64)
	if now-last.Load() < 60 {
		return
	}
	if !last.CompareAndSwap(last.Load(), now) {
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

// noteError 追加一条错误摘要（多次失败按出现顺序拼接，供请求日志展示）。
func (s *statusWriter) noteError(msg string) {
	if msg == "" {
		return
	}
	if s.errMsg == "" {
		s.errMsg = msg
		return
	}
	s.errMsg += "；" + msg
}

func (s *statusWriter) errorMessage() string { return s.errMsg }

// prepareBody 按需重写 JSON body：
//   - 无 extra_body、无需改写 model、且不需要注入 stream_options → 原样返回（零改写）；
//   - 否则用 map[string]json.RawMessage 做最小改写：替换 model 键 / 合并 extra_body 键 /
//     必要时追加 stream_options；其余字段的原始字节完全保留。
//
// stream_options.include_usage=true 注入：OpenAI 兼容的厂商（DeepSeek / MiniMax /
// Agnes 等）在流式响应里默认不发 usage 块——必须请求里显式带此开关才会发。
// 网关层负责 token 用量统计，没有 usage 就写不进 model_usage_daily，因此仅在
// 缺失时追加，且不覆盖调用方已有的 stream_options 键。
func prepareBody(raw []byte, decision route.Result, originalModel string) ([]byte, error) {
	extra := decision.Provider.ExtraBody
	needModelRewrite := decision.Model != originalModel
	needStreamOption := needsStreamUsageOption(raw)
	if len(extra) == 0 && !needModelRewrite && !needStreamOption {
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
	if needStreamOption {
		obj["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	return json.Marshal(obj)
}

// needsStreamUsageOption 报告 body 是否需要在出站时追加 stream_options.include_usage=true。
//
// 判定同时看三个条件：请求是流式、body 是合法 JSON 对象、对象里没有 stream_options 键。
// 任一不满足则不注入（用户/上游兼容性优先；非流式响应本就回完整 usage 也不需要）。
func needsStreamUsageOption(raw []byte) bool {
	if !isStreamRequest(raw) {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, has := probe["stream_options"]
	return !has
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
