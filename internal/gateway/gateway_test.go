package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agora-model/internal/config"
)

const gatewayKey = "gw-test-key"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.Level(100)}))
}

// buildGateway 构造一个注册了数据面端点的测试服务。
func buildGateway(t *testing.T, gw config.Gateway, providers ...config.Provider) *httptest.Server {
	t.Helper()
	if gw.APIKey == "" {
		gw.APIKey = gatewayKey
	}
	if gw.MaxBodyBytes == 0 {
		gw.MaxBodyBytes = 1 << 20
	}
	if gw.SSEIdleSeconds == 0 {
		gw.SSEIdleSeconds = 30
	}
	snap, err := config.NewSnapshot(config.File{Gateway: gw, Providers: providers})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	g := New(config.NewHolder(snap), testLogger())
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func providerFor(id, upstreamURL string, models ...string) config.Provider {
	return config.Provider{
		ID:               id,
		OpenAIBaseURL:    upstreamURL + "/v1",
		AnthropicBaseURL: upstreamURL + "/v1",
		APIKey:           "sk-upstream-" + id,
		Models:           models,
	}
}

func doRequest(t *testing.T, method, url, apiKey, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	return out
}

func TestAuthFailurePerProtocolStyle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	t.Run("OpenAI 入站错误体风格", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "", `{"model":"m"}`, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
		}
		got := decodeJSON(t, body)
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 error 对象: %s", body)
		}
		if errObj["code"] != "invalid_api_key" {
			t.Errorf("error.code = %v, 期望 invalid_api_key", errObj["code"])
		}
		if errObj["type"] != "authentication_error" {
			t.Errorf("error.type = %v, 期望 authentication_error", errObj["type"])
		}
	})

	t.Run("Anthropic 入站错误体风格", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/messages", "", `{"model":"m"}`, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
		}
		got := decodeJSON(t, body)
		if got["type"] != "error" {
			t.Errorf("type = %v, 期望 error", got["type"])
		}
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 error 对象: %s", body)
		}
		if errObj["type"] != "authentication_error" {
			t.Errorf("error.type = %v, 期望 authentication_error", errObj["type"])
		}
	})

	t.Run("x-api-key 亦可认证 Anthropic 入站", func(t *testing.T) {
		resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/messages", "", `{"model":"m"}`,
			map[string]string{"x-api-key": gatewayKey})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
		}
	})

	t.Run("错误 key 被拒", func(t *testing.T) {
		resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "wrong-key", `{"model":"m"}`, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	resp, body := doRequest(t, http.MethodGet, srv.URL+"/v1/chat/completions", gatewayKey, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", resp.StatusCode)
	}
	if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "not_found" {
		t.Errorf("code = %v, 期望 not_found", code)
	}
}

func TestBadRequestCases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{not json`},
		{"缺少 model", `{"messages":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
			}
			if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "invalid_request_error" {
				t.Errorf("code = %v", code)
			}
		})
	}
}

func TestModelNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "known"))

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"unknown"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", resp.StatusCode)
	}
	errObj := decodeJSON(t, body)["error"].(map[string]any)
	if errObj["code"] != "model_not_found" {
		t.Errorf("code = %v, 期望 model_not_found", errObj["code"])
	}
}

func TestProtocolNotConfigured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	t.Run("OpenAI 入站但只配了 Anthropic 地址", func(t *testing.T) {
		onlyAnthropic := config.Provider{
			ID:               "p",
			AnthropicBaseURL: upstream.URL + "/v1",
			APIKey:           "sk-up",
			Models:           []string{"m"},
		}
		srv := buildGateway(t, config.Gateway{}, onlyAnthropic)

		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"m"}`, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
		}
		errObj := decodeJSON(t, body)["error"].(map[string]any)
		if errObj["code"] != "upstream_protocol_not_configured" {
			t.Errorf("code = %v, 期望 upstream_protocol_not_configured", errObj["code"])
		}
	})

	t.Run("Anthropic 入站但只配了 OpenAI 地址（错误体应为 Anthropic 风格）", func(t *testing.T) {
		onlyOpenAI := config.Provider{
			ID:            "p",
			OpenAIBaseURL: upstream.URL + "/v1",
			APIKey:        "sk-up",
			Models:        []string{"m"},
		}
		srv := buildGateway(t, config.Gateway{}, onlyOpenAI)

		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/messages", "", `{"model":"m"}`,
			map[string]string{"x-api-key": gatewayKey})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
		}
		errObj := decodeJSON(t, body)["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("error.type = %v, 期望 invalid_request_error", errObj["type"])
		}
	})
}

// captured 记录上游收到的请求，用于断言头改写与请求体保真。
type captured struct {
	mu     sync.Mutex
	method string
	path   string
	header http.Header
	body   []byte
}

func (c *captured) set(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.path = r.URL.Path
	c.header = r.Header.Clone()
	c.body = body
}

func (c *captured) snapshot() captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return captured{method: c.method, path: c.path, header: c.header, body: c.body}
}

func TestPassthroughHeaderRewriteAndByteFidelity(t *testing.T) {
	var cap captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.set(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"up-1","choices":[{"message":{"content":"你好"}}]}`))
	}))
	defer upstream.Close()

	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	payload := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.25}`
	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, payload,
		map[string]string{"anthropic-version": "2023-06-01", "x-custom": "kept"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream-Marker") != "yes" {
		t.Error("上游自定义响应头未被透传")
	}
	if body != `{"id":"up-1","choices":[{"message":{"content":"你好"}}]}` {
		t.Errorf("响应体未被原样透传: %s", body)
	}

	got := cap.snapshot()
	if got.method != http.MethodPost || got.path != "/v1/chat/completions" {
		t.Errorf("上游收到 %s %s", got.method, got.path)
	}
	if string(got.body) != payload {
		t.Errorf("请求体应零改写\n实际: %s\n期望: %s", got.body, payload)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer sk-upstream-p" {
		t.Errorf("Authorization = %q, 期望供应商凭证", auth)
	}
	if v := got.header.Get("x-api-key"); v != "" {
		t.Errorf("x-api-key 应被剥离，得到 %q", v)
	}
	if v := got.header.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version 应透传，得到 %q", v)
	}
	if v := got.header.Get("x-custom"); v != "kept" {
		t.Errorf("自定义头应透传，得到 %q", v)
	}
	if v := got.header.Get("Accept-Encoding"); v != "" {
		t.Errorf("Accept-Encoding 应被剥离，得到 %q", v)
	}
}

func TestAnthropicInboundUsesAPIKeyHeader(t *testing.T) {
	var cap captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.set(r, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/messages", "", `{"model":"m","max_tokens":16}`,
		map[string]string{"x-api-key": gatewayKey, "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	got := cap.snapshot()
	if got.path != "/v1/messages" {
		t.Errorf("上游路径 = %q, 期望 /v1/messages", got.path)
	}
	if v := got.header.Get("x-api-key"); v != "sk-upstream-p" {
		t.Errorf("x-api-key = %q, 期望供应商凭证", v)
	}
	if v := got.header.Get("Authorization"); v != "" {
		t.Errorf("Authorization 不应出现在 Anthropic 上游请求中，得到 %q", v)
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited by upstream"}}`))
	}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{}, providerFor("p", upstream.URL, "m"))

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"m"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, 期望 429（原样透传）", resp.StatusCode)
	}
	if body != `{"error":{"message":"rate limited by upstream"}}` {
		t.Errorf("上游错误体应原样回传: %s", body)
	}
}

func TestBodyTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	srv := buildGateway(t, config.Gateway{MaxBodyBytes: 1024}, providerFor("p", upstream.URL, "m"))

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"m","pad":"`+strings.Repeat("x", 2048)+`"}`, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413", resp.StatusCode)
	}
	if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "payload_too_large" {
		t.Errorf("code = %v", code)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := upstream.URL
	upstream.Close() // 关闭后端口不可达

	srv := buildGateway(t, config.Gateway{}, providerFor("p", deadURL, "m"))
	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"m"}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", resp.StatusCode)
	}
	if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "upstream_unreachable" {
		t.Errorf("code = %v", code)
	}
}

func TestExtraBodyAndHeadersMerged(t *testing.T) {
	var cap captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.set(r, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	provider := providerFor("p", upstream.URL, "m")
	provider.ExtraHeaders = map[string]string{"x-tenant": "agora"}
	provider.ExtraBody = map[string]any{"temperature": 0.1, "top_p": 0.9}
	srv := buildGateway(t, config.Gateway{}, provider)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"m","messages":[],"temperature":0.9}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	got := cap.snapshot()
	if v := got.header.Get("x-tenant"); v != "agora" {
		t.Errorf("extra_headers 未生效: %q", v)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if payload["temperature"] != 0.1 {
		t.Errorf("extra_body 应覆盖同名键，temperature = %v", payload["temperature"])
	}
	if payload["top_p"] != 0.9 {
		t.Errorf("extra_body 新增键缺失，top_p = %v", payload["top_p"])
	}
	if payload["model"] != "m" {
		t.Errorf("model 字段应保留，得到 %v", payload["model"])
	}
	if _, ok := payload["messages"]; !ok {
		t.Error("原有字段应保留")
	}
}

func TestSSEKeepAliveAndByteFidelity(t *testing.T) {
	const upstreamPayload = "data: first\n\ndata: second\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		time.Sleep(1300 * time.Millisecond) // 超过 idle=1s，应触发心跳注入
		_, _ = w.Write([]byte("data: second\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	srv := buildGateway(t, config.Gateway{SSEIdleSeconds: 1}, providerFor("p", upstream.URL, "m"))

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"m","stream":true}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if !strings.Contains(body, keepAliveChunk) {
		t.Errorf("空闲期间应注入心跳注释行，实际响应: %q", body)
	}
	cleaned := strings.ReplaceAll(body, keepAliveChunk, "")
	if cleaned != upstreamPayload {
		t.Errorf("去除心跳后应与上游字节一致\n实际: %q\n期望: %q", cleaned, upstreamPayload)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		select {
		case <-r.Context().Done():
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()

	srv := buildGateway(t, config.Gateway{SSEIdleSeconds: 30}, providerFor("p", upstream.URL, "m"))

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("读取首个数据块失败: %v", err)
	}

	cancel() // 模拟客户端断开
	_ = resp.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端断开后，上游请求未被取消（存在泄漏风险）")
	}
}

func TestModelsEndpointAggregatesAndNamespaces(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	first := providerFor("a", upstream.URL, "shared", "only-a")
	first.Priority = 1
	second := providerFor("b", upstream.URL, "shared", "only-b")
	second.Priority = 2
	srv := buildGateway(t, config.Gateway{}, first, second)

	resp, _ := doRequest(t, http.MethodGet, srv.URL+"/v1/models", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权状态码 = %d, 期望 401", resp.StatusCode)
	}

	resp, body := doRequest(t, http.MethodGet, srv.URL+"/v1/models", gatewayKey, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, 期望 no-store", cc)
	}

	items, ok := decodeJSON(t, body)["data"].([]any)
	if !ok {
		t.Fatalf("响应缺少 data 数组: %s", body)
	}
	ownedBy := make(map[string]string, len(items))
	for _, raw := range items {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("data 元素不是对象: %v", raw)
		}
		id, _ := entry["id"].(string)
		owner, _ := entry["owned_by"].(string)
		if entry["object"] != "model" {
			t.Errorf("object 字段 = %v, 期望 model", entry["object"])
		}
		ownedBy[id] = owner
	}

	if ownedBy["shared"] != "a" {
		t.Errorf("裸模型名 shared 的 owned_by = %q, 期望 priority 最小的 a", ownedBy["shared"])
	}
	if ownedBy["only-b"] != "b" {
		t.Errorf("only-b 的 owned_by = %q, 期望 b", ownedBy["only-b"])
	}
	if ownedBy["a/shared"] != "a" || ownedBy["b/shared"] != "b" {
		t.Errorf("命名空间条目缺失或归属错误: %v", ownedBy)
	}

	resp, _ = doRequest(t, http.MethodPost, srv.URL+"/v1/models", gatewayKey, `{}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /v1/models 状态码 = %d, 期望 404", resp.StatusCode)
	}
}

func TestNamespaceRoutingRewritesModelBeforeForwarding(t *testing.T) {
	var cap captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.set(r, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	first := providerFor("a", upstream.URL, "shared")
	first.Priority = 1
	second := providerFor("b", upstream.URL, "shared")
	second.Priority = 2
	srv := buildGateway(t, config.Gateway{}, first, second)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"b/shared","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.Unmarshal(cap.snapshot().body, &payload); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if payload["model"] != "shared" {
		t.Fatalf("上游收到的 model 应为 shared（已剥离命名空间前缀），得到 %v", payload["model"])
	}
}

func TestNamespaceRoutingDisabledProviderReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	disabled := providerFor("off", upstream.URL, "m")
	disabled.Enabled = boolPtr(false)
	enabled := providerFor("on", upstream.URL, "m")
	srv := buildGateway(t, config.Gateway{}, enabled, disabled)

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"off/m","messages":[]}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", resp.StatusCode)
	}
	if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "no_available_provider" {
		t.Errorf("code = %v, 期望 no_available_provider", code)
	}
}

func TestSlashModelNameIsNotTreatedAsNamespace(t *testing.T) {
	var cap captured
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.set(r, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	p := providerFor("p", upstream.URL, "meta-llama/Llama-3-70B")
	srv := buildGateway(t, config.Gateway{}, p)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey,
		`{"model":"meta-llama/Llama-3-70B","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("含斜杠的真实模型名应正常转发，状态码 = %d", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.Unmarshal(cap.snapshot().body, &payload)
	if payload["model"] != "meta-llama/Llama-3-70B" {
		t.Fatalf("模型名不应被改写，得到 %v", payload["model"])
	}
}

func boolPtr(b bool) *bool { return &b }
