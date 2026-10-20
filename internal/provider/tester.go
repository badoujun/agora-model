package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
)

// KeyResult 是单个 API Key 的探测结果。
type KeyResult struct {
	KeyID      string `json:"key_id"`
	Label      string `json:"label,omitempty"`
	Hint       string `json:"hint,omitempty"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Message    string `json:"message"`
	ModelCount int    `json:"model_count,omitempty"`
}

// Result 是连接测试结果（供 Web UI 展示）。
//
// 多 Key 语义：OK 表示"至少有一个 Key 可用"，Keys 是实际尝试过的 Key 明细
// （按轮询顺序，遇到第一个成功的 Key 即停止，避免对上游做无谓的探测）。
type Result struct {
	OK         bool        `json:"ok"`
	StatusCode int         `json:"status_code"`
	LatencyMS  int64       `json:"latency_ms"`
	Message    string      `json:"message"`
	ModelCount int         `json:"model_count,omitempty"`
	CheckedAt  time.Time   `json:"checked_at"`
	Keys       []KeyResult `json:"keys,omitempty"`
}

// TestTimeout 是单次探测的超时。
const TestTimeout = 15 * time.Second

// Test 对供应商执行连通性探测（DESIGN §7.6）。
//
//	优先 GET {openai_base_url}/models；若 404/405 则退化为最小 chat/completions 探测。
//
// 多 Key：按顺序逐个尝试，遇到第一个成功的 Key 即返回；全部失败时返回
// 最后一次的失败详情，并附上每个 Key 的探测结果。
func Test(ctx context.Context, p config.Provider, client *http.Client) Result {
	if client == nil {
		client = defaultClient()
	}
	keys := p.UsableKeys()
	if len(keys) == 0 {
		return Result{Message: "没有可用的 API Key（可能已全部被自动停用，请在供应商页重新启用）",
			CheckedAt: time.Now().UTC()}
	}

	results := make([]KeyResult, 0, len(keys))
	for i, key := range keys {
		r := testWithKey(ctx, p, key.Key, client)
		results = append(results, KeyResult{
			KeyID: key.ID, Label: key.Label, Hint: crypto.Mask(key.Key),
			OK: r.OK, StatusCode: r.StatusCode, LatencyMS: r.LatencyMS,
			Message: r.Message, ModelCount: r.ModelCount,
		})
		if r.OK {
			r.Keys = results
			r.CheckedAt = time.Now().UTC()
			if i > 0 {
				// 保留首个成功 Key 的具体提示（例如 /models 不可用、已退化为对话探测）
				r.Message = fmt.Sprintf("%s（前 %d 个 Key 失败，已切换到第 %d 个）", r.Message, i, i+1)
			}
			return r
		}
	}

	last := results[len(results)-1]
	// 单个 Key 时保持与旧版本完全一致的消息；多 Key 时补一句汇总。
	message := last.Message
	if len(results) > 1 {
		message = fmt.Sprintf("全部 %d 个 Key 均探测失败；最后一个：%s", len(results), last.Message)
	}
	return Result{
		OK:         false,
		StatusCode: last.StatusCode,
		LatencyMS:  last.LatencyMS,
		Message:    message,
		CheckedAt:  time.Now().UTC(),
		Keys:       results,
	}
}

// TestKey 只探测指定的单个 Key（控制台「单独测试」用，含已停用的 Key）。
func TestKey(ctx context.Context, p config.Provider, key config.ProviderKey, client *http.Client) Result {
	if client == nil {
		client = defaultClient()
	}
	r := testWithKey(ctx, p, key.Key, client)
	r.CheckedAt = time.Now().UTC()
	r.Keys = []KeyResult{{
		KeyID: key.ID, Label: key.Label, Hint: crypto.Mask(key.Key),
		OK: r.OK, StatusCode: r.StatusCode, LatencyMS: r.LatencyMS,
		Message: r.Message, ModelCount: r.ModelCount,
	}}
	return r
}

func defaultClient() *http.Client {
	return &http.Client{
		Timeout: TestTimeout,
		Transport: &http.Transport{
			Proxy:              nil,
			DisableCompression: true,
		},
	}
}

// testWithKey 用指定的明文凭证执行一次探测。
func testWithKey(ctx context.Context, p config.Provider, apiKey string, client *http.Client) Result {
	result := testModels(ctx, p, apiKey, client)
	result.CheckedAt = time.Now().UTC()
	return result
}

func testModels(ctx context.Context, p config.Provider, apiKey string, client *http.Client) Result {
	endpoint := p.ModelsEndpoint()
	if endpoint == "" {
		// 端点覆盖模式下无法推导 /models，只做最小对话探测
		if override := strings.TrimSpace(p.OpenAIEndpointOverride); override != "" {
			return probeChat(ctx, p, apiKey, client, override)
		}
		return Result{Message: "未配置 openai_base_url"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range p.ExtraHeaders {
		req.Header.Set(k, v)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{LatencyMS: elapsedMS(started), Message: "连接失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := elapsedMS(started)

	if resp.StatusCode == http.StatusOK {
		count := 0
		var parsed struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			count = len(parsed.Data)
		}
		return Result{OK: true, StatusCode: resp.StatusCode, LatencyMS: latency,
			Message: "OK", ModelCount: count}
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		fallback := probeChat(ctx, p, apiKey, client, strings.TrimRight(p.OpenAIBaseURL, "/")+"/chat/completions")
		if fallback.OK {
			fallback.Message = "OK（/models 不可用，已用最小对话请求探测）"
		}
		return fallback
	}
	return Result{StatusCode: resp.StatusCode, LatencyMS: latency,
		Message: describeStatus(resp.StatusCode, body)}
}

func probeChat(ctx context.Context, p config.Provider, apiKey string, client *http.Client, endpoint string) Result {
	model := firstModel(p)
	if model == "" {
		return Result{Message: "未勾选任何模型，无法做最小对话探测"}
	}
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	reqCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	req, err := newJSONRequest(reqCtx, endpoint, payload, p.ExtraHeaders)
	if err != nil {
		return Result{Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{LatencyMS: elapsedMS(started), Message: "连接失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	latency := elapsedMS(started)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Result{OK: true, StatusCode: resp.StatusCode, LatencyMS: latency, Message: "OK"}
	}
	return Result{StatusCode: resp.StatusCode, LatencyMS: latency,
		Message: describeStatus(resp.StatusCode, body)}
}

func newJSONRequest(ctx context.Context, endpoint string, payload any, extraHeaders map[string]string) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// firstModel 返回供应商首个已勾选模型的上游名称（探测用）。
func firstModel(p config.Provider) string {
	if len(p.Models) == 0 {
		return ""
	}
	return p.Models[0]
}

func describeStatus(status int, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return fmt.Sprintf("上游返回 HTTP %d", status)
	}
	if len(trimmed) > 300 {
		trimmed = trimmed[:300]
	}
	return fmt.Sprintf("上游返回 HTTP %d: %s", status, trimmed)
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
