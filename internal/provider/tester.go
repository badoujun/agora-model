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
)

// Result 是单协议的连接测试结果（供 Web UI 展示）。
type Result struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMS  int64  `json:"latency_ms"`
	Message    string `json:"message"`
	ModelCount int    `json:"model_count,omitempty"`
}

// TestResult 是双协议连接测试结果。
type TestResult struct {
	OpenAI    Result    `json:"openai"`
	Anthropic Result    `json:"anthropic"`
	CheckedAt time.Time `json:"checked_at"`
}

// TestTimeout 是单次探测的超时。
const TestTimeout = 15 * time.Second

// Test 对供应商执行双协议连通性探测（DESIGN §7.6）。
//
//   - OpenAI 侧：优先 GET {openai_base}/models；若 404/405 则退化为最小 chat/completions 探测；
//   - Anthropic 侧：POST {anthropic_base}/messages，max_tokens=1（计费最小）。
//
// 未配置某协议地址时，该项返回 ok=false 并说明原因，不视为整体失败。
func Test(ctx context.Context, p config.Provider, client *http.Client) TestResult {
	if client == nil {
		client = &http.Client{
			Timeout: TestTimeout,
			Transport: &http.Transport{
				Proxy:              nil,
				DisableCompression: true,
			},
		}
	}
	return TestResult{
		OpenAI:    testOpenAI(ctx, p, client),
		Anthropic: testAnthropic(ctx, p, client),
		CheckedAt: time.Now().UTC(),
	}
}

func testOpenAI(ctx context.Context, p config.Provider, client *http.Client) Result {
	base := strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	if p.OpenAIEndpointOverride != "" {
		// 覆盖了完整端点时，只做最小 chat 探测（无法可靠推导 /models 地址）
		return probeChat(ctx, p, client, p.OpenAIEndpointOverride)
	}
	if base == "" {
		return Result{Message: "未配置 OpenAI 协议地址"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return Result{Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
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
		fallback := probeChat(ctx, p, client, base+"/chat/completions")
		if fallback.OK {
			fallback.Message = "OK（/models 不可用，已用最小对话请求探测）"
		}
		return fallback
	}
	return Result{StatusCode: resp.StatusCode, LatencyMS: latency,
		Message: describeStatus(resp.StatusCode, body)}
}

func probeChat(ctx context.Context, p config.Provider, client *http.Client, endpoint string) Result {
	model := firstModel(p)
	if model == "" {
		return Result{Message: "未配置任何模型，无法做最小对话探测"}
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
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

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

func testAnthropic(ctx context.Context, p config.Provider, client *http.Client) Result {
	endpoint := strings.TrimSpace(p.AnthropicEndpointOverride)
	if endpoint == "" {
		if base := strings.TrimRight(strings.TrimSpace(p.AnthropicBaseURL), "/"); base != "" {
			endpoint = base + "/messages"
		}
	}
	if endpoint == "" {
		return Result{Message: "未配置 Anthropic 协议地址"}
	}
	model := firstModel(p)
	if model == "" {
		return Result{Message: "未配置任何模型，无法做最小对话探测"}
	}

	payload := map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	reqCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	req, err := newJSONRequest(reqCtx, endpoint, payload, p.ExtraHeaders)
	if err != nil {
		return Result{Message: err.Error()}
	}
	req.Header.Set("x-api-key", p.APIKey)
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

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
