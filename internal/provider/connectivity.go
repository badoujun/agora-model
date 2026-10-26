package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
)

// 连通性测试的默认参数。
const (
	// DefaultConnectivityMessage 是测试消息的默认文案（用户可改）。
	DefaultConnectivityMessage = "你好，请回复“连接成功”。"
	// MaxConnectivityMessageBytes 是测试消息的长度上限（避免误把长文档贴进来烧钱）。
	MaxConnectivityMessageBytes = 2000
	// ConnectivityMaxTokens 是测试调用的输出上限；足够覆盖一次简短回复，
	// 又不至于在模型失控时产生大量计费。
	ConnectivityMaxTokens = 256
	// ConnectivityBodyLimit 是回传给前端展示的响应体上限（超出部分截断）。
	ConnectivityBodyLimit = 256 << 10
)

// ConnectivityRequest 是一次真实模型调用探测的入参。
type ConnectivityRequest struct {
	// Model 是**上游模型名**（必须是供应商已勾选的模型）。
	Model string
	// Message 是用户消息；为空时使用 DefaultConnectivityMessage。
	Message string
}

// ConnectivityResult 是真实模型调用探测的结果（供 Web UI 展示原始响应）。
//
// 与 Result（轻量探测）的区别：这里一定发出了一次 chat/completions 请求，
// 会产生真实的 token 消耗，因此额外回传 usage 与原始响应体。
type ConnectivityResult struct {
	OK         bool  `json:"ok"`
	StatusCode int   `json:"status_code,omitempty"`
	LatencyMS  int64 `json:"latency_ms"`
	// FirstByteMS 是首字节时间，用于区分「连不上」与「模型太慢」。
	FirstByteMS int64 `json:"first_byte_ms"`
	// RequestID 由本服务生成，与请求日志里的 request_id 一致，便于对照排障。
	RequestID string `json:"request_id"`
	// Model 是实际发给上游的模型名。
	Model   string `json:"model"`
	Message string `json:"message"`
	// ResponseRaw 是上游原始响应体（可能被截断，见 ResponseTruncated）。
	ResponseRaw string `json:"response_raw,omitempty"`
	// ResponseTruncated 表示 response_raw 因超过上限被截断。
	ResponseTruncated bool `json:"response_truncated,omitempty"`
	// Usage 是解析出的 token 统计；上游未返回时为 nil。
	Usage     *ConnectivityUsage `json:"usage,omitempty"`
	KeyID     string             `json:"key_id,omitempty"`
	KeyHint   string             `json:"key_hint,omitempty"`
	CheckedAt time.Time          `json:"checked_at"`
}

// ConnectivityUsage 是展示用的 token 统计（与用量记录口径一致）。
type ConnectivityUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// Validate 校验一次连通性测试请求：模型必须命中已勾选的上游模型，消息长度受限。
//
// 返回的是**上游模型名**（别名会被解析回真实模型名）。
func (r ConnectivityRequest) Validate(p config.Provider) (ConnectivityRequest, error) {
	model := strings.TrimSpace(r.Model)
	if model == "" {
		return r, fmt.Errorf("请选择要测试的模型")
	}
	upstream, ok := p.RealModel(model)
	if !ok || !p.HasUpstreamModel(upstream) {
		return r, fmt.Errorf("模型 %s 不在该供应商已勾选的模型里，请先在供应商配置中勾选", model)
	}
	message := strings.TrimSpace(r.Message)
	if message == "" {
		message = DefaultConnectivityMessage
	}
	if len(message) > MaxConnectivityMessageBytes {
		return r, fmt.Errorf("测试消息过长（%d 字节，上限 %d 字节）", len(message), MaxConnectivityMessageBytes)
	}
	return ConnectivityRequest{Model: upstream, Message: message}, nil
}

// Call 用指定 Key 发起一次真实的 chat/completions 调用（非流式）。
//
// 与转发路径保持一致：沿用供应商的 extra_headers / extra_body 与超时，
// 但只发送一条 user 消息、max_tokens 固定为 ConnectivityMaxTokens。
//
// 上游返回非 2xx 时 OK=false，但仍然回传原始响应体，便于在界面上看清原因。
func Call(ctx context.Context, p config.Provider, key config.ProviderKey, req ConnectivityRequest, requestID string,
	client *http.Client) (ConnectivityResult, error) {
	if client == nil {
		client = defaultClient()
	}
	normalized, err := req.Validate(p)
	if err != nil {
		return ConnectivityResult{}, err
	}
	endpoint := p.Endpoint()
	if endpoint == "" {
		return ConnectivityResult{}, fmt.Errorf("未配置 openai_base_url 或 openai_endpoint_override")
	}
	if strings.TrimSpace(key.Key) == "" {
		return ConnectivityResult{}, fmt.Errorf("该 API Key 没有可用的明文")
	}

	payload := map[string]any{
		"model":      normalized.Model,
		"messages":   []map[string]string{{"role": "user", "content": normalized.Message}},
		"max_tokens": ConnectivityMaxTokens,
		"stream":     false,
	}
	for k, v := range p.ExtraBody {
		payload[k] = v
	}

	reqCtx, cancel := context.WithTimeout(ctx, p.Timeout())
	defer cancel()
	httpReq, err := newJSONRequest(reqCtx, endpoint, payload, p.ExtraHeaders)
	if err != nil {
		return ConnectivityResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key.Key)
	// 允许上游压缩并无妨：我们完整读取后再展示。

	started := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return ConnectivityResult{
			RequestID: requestID,
			Model:     normalized.Model,
			Message:   "连接失败: " + err.Error(),
			KeyID:     key.ID,
			KeyHint:   crypto.Mask(key.Key),
			LatencyMS: elapsedMS(started),
			CheckedAt: time.Now().UTC(),
		}, nil
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, ConnectivityBodyLimit+1))
	firstByte := elapsedMS(started)
	latency := elapsedMS(started)

	result := ConnectivityResult{
		OK:          resp.StatusCode >= 200 && resp.StatusCode < 300,
		StatusCode:  resp.StatusCode,
		LatencyMS:   latency,
		FirstByteMS: firstByte,
		RequestID:   requestID,
		Model:       normalized.Model,
		KeyID:       key.ID,
		KeyHint:     crypto.Mask(key.Key),
		CheckedAt:   time.Now().UTC(),
	}
	if len(body) > ConnectivityBodyLimit {
		body = body[:ConnectivityBodyLimit]
		result.ResponseTruncated = true
	}
	result.ResponseRaw = string(body)
	if readErr != nil {
		result.Message = "读取上游响应失败: " + readErr.Error()
		return result, nil
	}

	if result.OK {
		result.Message = "连接成功"
		if usage, ok := logging.ParseOpenAIUsage(body); ok {
			result.Usage = &ConnectivityUsage{
				InputTokens:       usage.InputTokens,
				CachedInputTokens: usage.CachedInputTokens,
				OutputTokens:      usage.OutputTokens,
			}
		} else {
			result.Message = "连接成功（上游未返回 usage）"
		}
		return result, nil
	}
	result.Message = describeStatus(resp.StatusCode, body)
	return result, nil
}
