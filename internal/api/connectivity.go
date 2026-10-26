package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
	"agora-model/internal/provider"
	"agora-model/internal/store"
)

// connectivityProtocol 是连通性测试写进请求日志的「协议」值。
//
// 与网关转发的 openai 区分开，便于在日志页一眼看出这是控制台发起的一次性诊断调用。
const connectivityProtocol = "connectivity"

// LogSink 是请求日志的投递接口（生产实现是 logging.Recorder）。
//
// 单独抽接口而不是直接依赖 *logging.Recorder：api 包只需要「投递一条」的能力。
type LogSink interface {
	Enqueue(e logging.Entry)
}

// connectivityInput 是连通性测试的请求体。
//
// 已保存的供应商只需要 Model / Message；草稿（尚未保存）额外带表单里的凭证与地址，
// 复用 draftProviderInput 以保持与「拉取模型 / 草稿测试」完全一致的校验。
type connectivityInput struct {
	draftProviderInput
	Model   string `json:"model"`
	Message string `json:"message"`
}

// handleTestProviderConnectivity 用某个已保存的 Key 发起一次真实模型调用。
//
// 与 /keys/{key_id}/test 的区别：那个只探测 /models（不产生 token 费用），
// 本接口会真的调用 chat/completions，并把用量与请求日志记入统计。
func (s *Server) handleTestProviderConnectivity(w http.ResponseWriter, r *http.Request) {
	var in connectivityInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	providerID := r.PathValue("id")
	keyID := r.PathValue("key_id")

	p, err := s.loadProvider(ctx, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不存在"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "读取供应商失败", err)
		return
	}
	rec, err := s.store.GetProviderKey(ctx, providerID, keyID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "该供应商没有这个 API Key"})
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 API Key 失败", err)
		return
	}
	plain, derr := crypto.Decrypt(s.master, rec.KeyCipher, providerID)
	if derr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("该 API Key 的密文无法用当前主密钥解密（%s）：请重新填写该 Key", rec.KeyHint),
		})
		return
	}
	// 停用的 Key 也允许测试：界面上的用途之一就是判断能否重新启用。
	enabled := true
	key := config.ProviderKey{ID: rec.ID, Label: rec.Label, Key: string(plain), Enabled: &enabled}

	s.runConnectivity(ctx, w, r, p, key, provider.ConnectivityRequest{Model: in.Model, Message: in.Message})
}

// handleTestDraftConnectivity 用尚未保存的草稿凭证发起一次真实模型调用。
func (s *Server) handleTestDraftConnectivity(w http.ResponseWriter, r *http.Request) {
	var in connectivityInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	draft, err := in.draftProvider()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	keys := draft.UsableKeys()
	if len(keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先填写至少一个 API Key"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	s.runConnectivity(ctx, w, r, draft, keys[0], provider.ConnectivityRequest{Model: in.Model, Message: in.Message})
}

// runConnectivity 执行一次真实调用，并按需要写入请求日志与 token 用量。
func (s *Server) runConnectivity(ctx context.Context, w http.ResponseWriter, r *http.Request, p config.Provider,
	key config.ProviderKey, req provider.ConnectivityRequest) {
	requestID := newControlRequestID()
	started := time.Now()
	upstreamURL := p.Endpoint()

	result, err := provider.Call(ctx, p, key, req, requestID, s.client)
	if err != nil {
		// 参数类错误（模型未勾选 / 消息过长 / 未配置地址）：直接 400，不写日志。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 一次诊断调用也记入请求日志：日志页能看到它的耗时与失败原因。
	// 归属网关 Key 为空（不是网关转发的请求），协议标记为 connectivity 便于区分。
	if s.logSink != nil {
		s.logSink.Enqueue(logging.Entry{
			TS:              started,
			RequestID:       requestID,
			InboundProtocol: connectivityProtocol,
			Model:           result.Model,
			ProviderID:      p.ID,
			KeyHint:         result.KeyHint,
			UpstreamURL:     upstreamURL,
			StatusCode:      result.StatusCode,
			LatencyMS:       result.LatencyMS,
			FirstByteMS:     result.FirstByteMS,
			Stream:          false,
			ErrorMsg:        connectivityError(result),
			ClientIP:        requestHost(r),
		})
	}

	// 成功调用才计用量（失败没有可信的 usage，且与网关口径一致）。
	if s.usageSink != nil && result.OK && result.Usage != nil {
		s.usageSink.Record("", p.ID, key.ID, result.Model, result.StatusCode,
			logging.Usage{
				InputTokens:       result.Usage.InputTokens,
				CachedInputTokens: result.Usage.CachedInputTokens,
				OutputTokens:      result.Usage.OutputTokens,
			}, result.CheckedAt)
	}

	if result.Usage != nil {
		s.logger.Info("连通性测试完成",
			"provider", p.ID, "key", key.ID, "model", result.Model,
			"status", result.StatusCode, "latency_ms", result.LatencyMS,
			"request_id", requestID)
	}
	writeJSON(w, http.StatusOK, result)
}

// connectivityError 把失败结果转成日志里的错误摘要（成功时为空）。
func connectivityError(result provider.ConnectivityResult) string {
	if result.OK {
		return ""
	}
	return result.Message
}

// newControlRequestID 生成控制面请求 id（与网关同形的 16 位十六进制）。
func newControlRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("ctl-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// requestHost 取请求来源主机（不带端口），用于日志的 client_ip 列。
func requestHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
