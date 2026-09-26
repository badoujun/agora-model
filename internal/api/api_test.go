package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
	"agora-model/internal/store"
)

const testAdminPassword = "s3cret-password"

type fakeAggregator struct {
	refreshProvider int
	refreshAll      int
	err             error
}

func (f *fakeAggregator) RefreshAll(ctx context.Context) { f.refreshAll++ }

func (f *fakeAggregator) RefreshProvider(ctx context.Context, p config.Provider) error {
	f.refreshProvider++
	return f.err
}

type harness struct {
	t           *testing.T
	srv         *httptest.Server
	store       *store.Store
	master      []byte
	holder      *config.Holder
	aggregator  *fakeAggregator
	reloadCount int
}

// newHarness 构造一个带真实 SQLite 的控制面测试服务。
func newHarness(t *testing.T, adminPassword, upstreamURL string) *harness {
	t.Helper()

	dir, err := os.MkdirTemp("", "agora-api-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	st, err := store.Open(context.Background(), filepath.Join(dir, "agora.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	master := make([]byte, crypto.KeySize)
	for i := range master {
		master[i] = byte(i + 1)
	}

	// 至少需要一个启用的供应商，否则 NewSnapshot 会拒绝（与运行期约束一致）
	if _, err := st.SaveProvider(context.Background(), master, config.Provider{
		ID:               "seed",
		Name:             "seed",
		OpenAIBaseURL:    upstreamURL + "/v1",
		AnthropicBaseURL: upstreamURL + "/v1",
		APIKey:           "sk-seed",
		Models:           []string{"seed-model"},
		AllowInternal:    true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	snap, err := buildSnapshot(context.Background(), st, master)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	holder := config.NewHolder(snap)

	h := &harness{t: t, store: st, master: master, holder: holder, aggregator: &fakeAggregator{}}
	mux := http.NewServeMux()
	New(Options{
		Store:      st,
		Master:     master,
		Holder:     holder,
		Aggregator: h.aggregator,
		Reload: func(ctx context.Context) (*config.Snapshot, error) {
			h.reloadCount++
			return buildSnapshot(ctx, st, master)
		},
		Version:       "test-version",
		AdminPassword: adminPassword,
		LocalOnly:     true,
	}).Register(mux)

	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func buildSnapshot(ctx context.Context, st *store.Store, master []byte) (*config.Snapshot, error) {
	providers, err := st.LoadProviders(ctx, master)
	if err != nil {
		return nil, err
	}
	return config.NewSnapshot(config.File{
		Gateway:   config.Gateway{Listen: "127.0.0.1", Port: 9090},
		Providers: providers,
	})
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func (h *harness) do(client *http.Client, method, path string, body any) (int, map[string]any, string) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed, string(raw)
}

func TestHealthAndSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, body, _ := h.do(client, http.MethodGet, "/api/health", nil)
	if code != http.StatusOK {
		t.Fatalf("health 状态码 = %d", code)
	}
	if body["status"] != "ok" || body["version"] != "test-version" {
		t.Fatalf("health 内容异常: %v", body)
	}
	if body["providers"].(float64) < 1 {
		t.Fatalf("providers = %v", body["providers"])
	}

	code, body, _ = h.do(client, http.MethodGet, "/api/auth/session", nil)
	if code != http.StatusOK {
		t.Fatalf("session 状态码 = %d", code)
	}
	if body["login_required"] != false || body["authenticated"] != true {
		t.Fatalf("免登录模式下的会话状态异常: %v", body)
	}
}

func TestAuthRequiredWhenPasswordConfigured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, testAdminPassword, upstream.URL)
	client := newClient(t)

	code, _, _ := h.do(client, http.MethodGet, "/api/providers", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d, 期望 401", code)
	}

	code, _, _ = h.do(client, http.MethodPost, "/api/auth/login", map[string]any{"password": "wrong"})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误密码状态码 = %d, 期望 401", code)
	}

	code, _, _ = h.do(client, http.MethodPost, "/api/auth/login", map[string]any{"password": testAdminPassword})
	if code != http.StatusOK {
		t.Fatalf("正确密码状态码 = %d, 期望 200", code)
	}

	code, _, _ = h.do(client, http.MethodGet, "/api/providers", nil)
	if code != http.StatusOK {
		t.Fatalf("登录后状态码 = %d, 期望 200", code)
	}

	if code, _, _ = h.do(client, http.MethodPost, "/api/auth/logout", nil); code != http.StatusOK {
		t.Fatalf("登出状态码 = %d", code)
	}
	if code, _, _ = h.do(client, http.MethodGet, "/api/providers", nil); code != http.StatusUnauthorized {
		t.Fatalf("登出后状态码 = %d, 期望 401", code)
	}
}

func TestProviderCRUDAndMasking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name":               "供应商A",
		"openai_base_url":    upstream.URL + "/v1",
		"anthropic_base_url": upstream.URL + "/v1",
		"api_key":            "sk-live-abcdefghijkl",
		"models_manual":      []string{"m1", "m1", " m2 "},
		"allow_internal":     true,
		"priority":           5,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d, 期望 201（%s）", code, raw)
	}
	if strings.Contains(raw, "sk-live-abcdefghijkl") {
		t.Fatalf("响应中不得出现明文凭证: %s", raw)
	}
	if body["api_key_hint"] != "sk-****ijkl" {
		t.Errorf("api_key_hint = %v", body["api_key_hint"])
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("响应缺少 id")
	}
	models, _ := body["models_manual"].([]any)
	if len(models) != 2 {
		t.Errorf("models_manual 应去重去空白，得到 %v", models)
	}

	code, body, _ = h.do(client, http.MethodGet, "/api/providers/"+id, nil)
	if code != http.StatusOK || body["name"] != "供应商A" {
		t.Fatalf("读取失败: %d %v", code, body)
	}

	// 未提供 api_key → 保持原凭证（解密后应仍能使用）
	code, body, raw = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{
		"name": "供应商A-改名",
	})
	if code != http.StatusOK {
		t.Fatalf("更新状态码 = %d（%s）", code, raw)
	}
	if body["name"] != "供应商A-改名" {
		t.Errorf("名称未更新: %v", body["name"])
	}
	if body["api_key_hint"] != "sk-****ijkl" {
		t.Errorf("凭证掩码不应变化: %v", body["api_key_hint"])
	}
	providers, err := h.store.LoadProviders(context.Background(), h.master)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range providers {
		if p.ID == id {
			found = true
			if p.APIKey != "sk-live-abcdefghijkl" {
				t.Errorf("未提供凭证时应沿用原值，得到 %q", p.APIKey)
			}
		}
	}
	if !found {
		t.Fatal("更新后找不到供应商")
	}

	code, _, _ = h.do(client, http.MethodDelete, "/api/providers/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("删除状态码 = %d", code)
	}
	if code, _, _ = h.do(client, http.MethodGet, "/api/providers/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("删除后读取状态码 = %d, 期望 404", code)
	}
}

func TestCreateProviderValidations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺少 api_key", map[string]any{"name": "x", "openai_base_url": upstream.URL + "/v1"}},
		{"缺少协议地址", map[string]any{"name": "x", "api_key": "sk-aaaaaaaaaaaa"}},
		{"内网地址未放行", map[string]any{
			"name": "x", "openai_base_url": "http://127.0.0.1:11434/v1", "api_key": "sk-aaaaaaaaaaaa",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := h.do(client, http.MethodPost, "/api/providers", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400（%s）", code, raw)
			}
		})
	}
}

func TestTestProviderEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/v1/messages":
			_, _ = w.Write([]byte(`{"type":"message"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers/seed/test", nil)
	if code != http.StatusOK {
		t.Fatalf("连接测试状态码 = %d（%s）", code, raw)
	}
	if body["ok"] != true {
		t.Fatalf("连接测试应通过: %v", body)
	}
	openai, _ := body["openai"].(map[string]any)
	if openai["ok"] != true || openai["model_count"].(float64) != 1 {
		t.Errorf("OpenAI 探测结果异常: %v", openai)
	}
	anthropic, _ := body["anthropic"].(map[string]any)
	if anthropic["ok"] != true {
		t.Errorf("Anthropic 探测结果异常: %v", anthropic)
	}
	if _, ok := body["checked_at"].(string); !ok {
		t.Error("缺少 checked_at")
	}
}

func TestFetchModelsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, _, _ := h.do(client, http.MethodPost, "/api/providers/seed/fetch-models", nil)
	if code != http.StatusOK {
		t.Fatalf("拉取模型状态码 = %d", code)
	}
	if h.aggregator.refreshProvider != 1 {
		t.Errorf("应调用一次 RefreshProvider，实际 %d", h.aggregator.refreshProvider)
	}

	h.aggregator.err = context.DeadlineExceeded
	code, body, _ := h.do(client, http.MethodPost, "/api/providers/seed/fetch-models", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("失败时状态码 = %d, 期望 502", code)
	}
	if _, ok := body["error"]; !ok {
		t.Error("失败响应应包含 error")
	}
}

func TestManualModelsAndList(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, _, raw := h.do(client, http.MethodPost, "/api/models/manual", map[string]any{
		"provider_id": "seed", "model": "manual-1",
	})
	if code != http.StatusOK {
		t.Fatalf("新增手动模型状态码 = %d（%s）", code, raw)
	}

	code, body, _ := h.do(client, http.MethodGet, "/api/models", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	found := false
	for _, raw := range items {
		entry := raw.(map[string]any)
		if entry["model"] == "manual-1" && entry["provider_id"] == "seed" {
			found = true
			if entry["default"] != true {
				t.Errorf("唯一供应商的模型应标记 default: %v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("列表中缺少 manual-1: %v", items)
	}

	code, _, _ = h.do(client, http.MethodDelete, "/api/models/manual?provider_id=seed&model=manual-1", nil)
	if code != http.StatusOK {
		t.Fatalf("删除手动模型状态码 = %d", code)
	}
	_, body, _ = h.do(client, http.MethodGet, "/api/models", nil)
	items, _ = body["items"].([]any)
	for _, raw := range items {
		if raw.(map[string]any)["model"] == "manual-1" {
			t.Fatal("删除后仍出现在列表中")
		}
	}
}

func TestSettingsAndGatewayKeyReset(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	if _, _, _ = h.do(client, http.MethodPost, "/api/gateway-key/reset", nil); true {
		// 先确保存在一个 Key
	}
	code, body, raw := h.do(client, http.MethodGet, "/api/gateway-key", nil)
	if code != http.StatusOK {
		t.Fatalf("读取网关 Key 状态码 = %d（%s）", code, raw)
	}
	if hint, _ := body["key_hint"].(string); hint == "" {
		t.Fatalf("网关 Key 掩码为空: %v", body)
	}

	code, body, raw = h.do(client, http.MethodPost, "/api/gateway-key/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("重置状态码 = %d（%s）", code, raw)
	}
	plain, _ := body["gateway_key"].(string)
	if !strings.HasPrefix(plain, store.GatewayKeyPrefix) {
		t.Fatalf("重置响应应包含一次性的明文 Key: %v", body)
	}
	if body["key_hint"] != crypto.Mask(plain) {
		t.Errorf("key_hint 与明文不匹配: %v", body)
	}

	// 明文只出现在该响应：后续列表里只有掩码
	_, body, raw = h.do(client, http.MethodGet, "/api/gateway-key", nil)
	if strings.Contains(raw, plain) {
		t.Fatal("明文不应出现在后续响应中")
	}
	if body["key_hint"] != crypto.Mask(plain) {
		t.Errorf("掩码应为新 Key 的掩码: %v", body["key_hint"])
	}

	code, body, raw = h.do(client, http.MethodPut, "/api/settings", map[string]any{
		"model_refresh_seconds": 300,
		"log_success":           true,
	})
	if code != http.StatusOK {
		t.Fatalf("写入设置状态码 = %d（%s）", code, raw)
	}
	if body["model_refresh_seconds"] != "300" {
		t.Errorf("model_refresh_seconds = %v", body["model_refresh_seconds"])
	}
	if body["log_success"] != "true" {
		t.Errorf("log_success = %v", body["log_success"])
	}

	// 非法值应被拒绝
	code, _, _ = h.do(client, http.MethodPut, "/api/settings", map[string]any{"model_refresh_seconds": 5})
	if code != http.StatusBadRequest {
		t.Fatalf("间隔过小应返回 400，实际 %d", code)
	}
}

func TestLogsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	entries := []logging.Entry{
		{TS: time.Now().UTC(), RequestID: "r1", InboundProtocol: "openai", Model: "m1", ProviderID: "seed", StatusCode: 200},
		{TS: time.Now().UTC(), RequestID: "r2", InboundProtocol: "openai", Model: "m2", ProviderID: "seed", StatusCode: 502,
			ErrorMsg: "上游失败 sk-should-be-redacted"},
	}
	if err := h.store.InsertLogs(context.Background(), entries); err != nil {
		t.Fatal(err)
	}

	code, body, raw := h.do(client, http.MethodGet, "/api/logs?failed=true", nil)
	if code != http.StatusOK {
		t.Fatalf("日志查询状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("failed=true 应只返回失败记录，实际 %d", len(items))
	}
	entry := items[0].(map[string]any)
	if entry["status_code"].(float64) != 502 {
		t.Errorf("状态码 = %v", entry["status_code"])
	}
	if strings.Contains(raw, "sk-should-be-redacted") {
		t.Fatal("日志中的疑似密钥必须脱敏")
	}
	if body["total"].(float64) != 2 {
		t.Errorf("total = %v", body["total"])
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodGet, "/api/export", nil)
	if code != http.StatusOK {
		t.Fatalf("导出状态码 = %d", code)
	}
	if strings.Contains(raw, "sk-seed") {
		t.Fatal("导出不得包含明文凭证")
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("导出供应商数 = %d", len(providers))
	}
	exported := providers[0].(map[string]any)
	exported["api_key"] = "sk-rotated-key-123456"
	exported["id"] = "imported"

	code, body, raw = h.do(client, http.MethodPost, "/api/import", map[string]any{
		"providers": []any{exported},
	})
	if code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（%s）", code, raw)
	}
	if body["imported"].(float64) != 1 {
		t.Errorf("imported = %v", body["imported"])
	}

	providers2, err := h.store.LoadProviders(context.Background(), h.master)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range providers2 {
		if p.ID == "imported" {
			found = true
			if p.APIKey != "sk-rotated-key-123456" {
				t.Errorf("导入的凭证 = %q", p.APIKey)
			}
		}
	}
	if !found {
		t.Fatal("导入后未找到供应商")
	}

	// 缺少 api_key 的导入应被拒绝（导出文件不含明文）
	exported["id"] = "no-key"
	delete(exported, "api_key")
	code, _, _ = h.do(client, http.MethodPost, "/api/import", map[string]any{"providers": []any{exported}})
	if code != http.StatusBadRequest {
		t.Fatalf("缺少凭证的导入状态码 = %d, 期望 400", code)
	}
}

func TestProviderListIncludesFetchStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	h := newHarness(t, "", upstream.URL)
	client := newClient(t)

	if err := h.store.ReplaceModelCache(context.Background(), "seed", []string{"auto-1", "auto-2"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetProviderFetchError(context.Background(), "seed", "上游返回 401"); err != nil {
		t.Fatal(err)
	}

	code, body, _ := h.do(client, http.MethodGet, "/api/providers", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	seed := items[0].(map[string]any)
	if seed["last_fetch_error"] != "上游返回 401" {
		t.Errorf("last_fetch_error = %v", seed["last_fetch_error"])
	}
	if seed["last_fetch_at"] == "" {
		t.Errorf("last_fetch_at 应被设置: %v", seed)
	}
}
