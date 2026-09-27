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

// Result 是连接测试结果（供 Web UI 展示）。
type Result struct {
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int64     `json:"latency_ms"`
	Message    string    `json:"message"`
	ModelCount int       `json:"model_count,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// TestTimeout 是单次探测的超时。
const TestTimeout = 15 * time.Second

// Test 对供应商执行连通性探测（DESIGN §7.6）。
//
//	优先 GET {openai_base_url}/models；若 404/405 则退化为最小 chat/completions 探测。
func Test(ctx context.Context, p config.Provider, client *http.Client) Result {
	if client == nil {
		client = &http.Client{
			Timeout: TestTimeout,
			Transport: &http.Transport{
				Proxy:              nil,
				DisableCompression: true,
			},
		}
	}
	result := testModels(ctx, p, client)
	result.CheckedAt = time.Now().UTC()
	return result
}

func testModels(ctx context.Context, p config.Provider, client *http.Client) Result {
	endpoint := p.ModelsEndpoint()
	if endpoint == "" {
		// 端点覆盖模式下无法推导 /models，只做最小对话探测
		if override := strings.TrimSpace(p.OpenAIEndpointOverride); override != "" {
			return probeChat(ctx, p, client, override)
		}
		return Result{Message: "未配置 openai_base_url"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
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
		fallback := probeChat(ctx, p, client, strings.TrimRight(p.OpenAIBaseURL, "/")+"/chat/completions")
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
