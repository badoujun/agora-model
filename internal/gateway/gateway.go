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
	"time"

	"agora-model/internal/config"
	"agora-model/internal/route"
)

// errBodyTooLarge 表示入站请求体超过上限。
var errBodyTooLarge = errors.New("请求体过大")

// Gateway 实现双协议原样透传的数据面。
type Gateway struct {
	holder *config.Holder
	client *http.Client
	logger *slog.Logger
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

// Register 注册数据面端点。
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc(config.ProtocolOpenAI.Path(), g.handler(config.ProtocolOpenAI))
	mux.HandleFunc(config.ProtocolAnthropic.Path(), g.handler(config.ProtocolAnthropic))
}

// handler 返回指定入站协议的处理器（DESIGN §7.1 主流程）。
func (g *Gateway) handler(proto config.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newRequestID()
		snap := g.holder.Get()
		cfg := snap.Gateway()

		if r.Method != http.MethodPost {
			writeError(w, proto, http.StatusNotFound, codeNotFound,
				fmt.Sprintf("%s 仅支持 POST，收到 %s", proto.Path(), r.Method), "")
			return
		}

		// 1) 入站鉴权
		if !authorized(r, cfg.APIKey) {
			g.logger.Warn("网关鉴权失败",
				"request_id", requestID, "path", proto.Path(), "client_ip", clientIP(r))
			writeError(w, proto, http.StatusUnauthorized, codeInvalidAPIKey,
				"网关 API Key 无效或缺失", "")
			return
		}

		// 2) 读取请求体（含上限）
		body, err := readBody(r, cfg.MaxBodyBytes)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				writeError(w, proto, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
					fmt.Sprintf("请求体超过上限 %d 字节", cfg.MaxBodyBytes), "")
				return
			}
			writeError(w, proto, http.StatusBadRequest, codeInvalidRequest,
				"读取请求体失败: "+err.Error(), "")
			return
		}

		// 3) 探测 model 字段
		model, err := probeModel(body)
		if err != nil {
			writeError(w, proto, http.StatusBadRequest, codeInvalidRequest, err.Error(), "model")
			return
		}

		// 4) 路由决策
		decision, err := route.Resolve(snap, proto, model)
		if err != nil {
			switch {
			case errors.Is(err, route.ErrModelNotFound):
				writeError(w, proto, http.StatusNotFound, codeModelNotFound, err.Error(), "model")
			case errors.Is(err, route.ErrProtocolNotConfigured):
				writeError(w, proto, http.StatusBadRequest, codeProtocolNotConfigured, err.Error(), "")
			default:
				writeError(w, proto, http.StatusInternalServerError, codeInvalidRequest, err.Error(), "")
			}
			g.logger.Warn("路由失败",
				"request_id", requestID, "protocol", proto, "model", model, "err", err)
			return
		}

		// 5) 组装上游请求体（无 extra_body 且不改写 model 时零改写）
		upstreamBody, err := prepareBody(body, decision, model)
		if err != nil {
			writeError(w, proto, http.StatusBadRequest, codeInvalidRequest, err.Error(), "")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), decision.Provider.Timeout())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, decision.Upstream, bytes.NewReader(upstreamBody))
		if err != nil {
			writeError(w, proto, http.StatusBadGateway, codeUpstreamUnreachable,
				"构造上游请求失败: "+err.Error(), "")
			return
		}
		req.Header = buildUpstreamHeaders(r.Header, decision.Provider, proto)
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		g.logger.Debug("路由决策",
			"request_id", requestID, "protocol", proto, "model", model,
			"provider", decision.Provider.ID, "upstream", decision.Upstream,
			"stream", isStreamRequest(body))

		// 6) 发送并透传响应
		resp, err := g.client.Do(req)
		if err != nil {
			g.logUpstreamFailure(requestID, proto, model, decision, started, err)
			g.writeUpstreamError(w, r, proto, err)
			return
		}
		defer resp.Body.Close()

		if isEventStream(resp) {
			stats, serr := streamResponse(r.Context(), w, resp, cfg.SSEIdle())
			g.logCompletion(requestID, proto, model, decision, resp.StatusCode, started, stats, serr)
			return
		}

		written, cerr := copyResponse(w, resp)
		g.logger.Info("请求完成",
			"request_id", requestID, "protocol", proto, "model", model,
			"provider", decision.Provider.ID, "status", resp.StatusCode,
			"bytes", written, "latency_ms", time.Since(started).Milliseconds(),
			"stream", false, "err", errString(cerr))
	}
}

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

// authorized 校验入站凭证：x-api-key（Anthropic 风格）或 Authorization: Bearer（OpenAI 风格）。
func authorized(r *http.Request, want string) bool {
	if want == "" {
		return false
	}
	got := tokenFromRequest(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, r *http.Request, proto config.Protocol, err error) {
	switch {
	case r.Context().Err() != nil:
		// 客户端已断开，写响应无意义
		return
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, proto, http.StatusGatewayTimeout, codeUpstreamTimeout,
			"上游请求超时", "")
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			writeError(w, proto, http.StatusGatewayTimeout, codeUpstreamTimeout,
				"上游请求超时: "+err.Error(), "")
			return
		}
		writeError(w, proto, http.StatusBadGateway, codeUpstreamUnreachable,
			"连接上游失败: "+err.Error(), "")
	}
}

func (g *Gateway) logUpstreamFailure(requestID string, proto config.Protocol, model string, d route.Result, started time.Time, err error) {
	status := "upstream_error"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "upstream_timeout"
	}
	g.logger.Error("上游请求失败",
		"request_id", requestID, "protocol", proto, "model", model,
		"provider", d.Provider.ID, "upstream", d.Upstream, "status", status,
		"latency_ms", time.Since(started).Milliseconds(), "err", err)
}

func (g *Gateway) logCompletion(requestID string, proto config.Protocol, model string, d route.Result, status int, started time.Time, stats streamStats, err error) {
	level := slog.LevelInfo
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		level = slog.LevelWarn // 客户端断开
	default:
		level = slog.LevelWarn
	}
	g.logger.Log(context.Background(), level, "流式请求完成",
		"request_id", requestID, "protocol", proto, "model", model,
		"provider", d.Provider.ID, "status", status,
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
