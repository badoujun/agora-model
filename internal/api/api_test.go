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
	"sync"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
	"agora-model/internal/store"
)

const testAdminPassword = "s3cret-password"

// upstreamStub 模拟供应商上游：/v1/models 与 /v1/chat/completions。
type upstreamStub struct {
	mu     sync.Mutex
	fail   bool
	models []string
	chat   bool
}

func (u *upstreamStub) setFail(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fail = v
}

func (u *upstreamStub) chatCalled() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chat
}

func (u *upstreamStub) handler(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	fail := u.fail
	models := append([]string(nil), u.models...)
	u.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
		return
	}

	switch r.URL.Path {
	case "/v1/models":
		items := make([]map[string]any, 0, len(models))
		for _, m := range models {
			items = append(items, map[string]any{"id": m})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
	case "/v1/chat/completions":
		u.mu.Lock()
		u.chat = true
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type harness struct {
	t           *testing.T
	srv         *httptest.Server
	store       *store.Store
	master      []byte
	holder      *config.Holder
	upstream    *upstreamStub
	upstreamURL string
	dataDir     string
	reloadCount int
	// apiServer 仅在新 harnessWithPresets 中设置（用于直接调用 Server 上的私有方法做测试）
	apiServer *Server
}

// newHarness 构造一个带真实 SQLite 的控制面测试服务（含 mock 上游）。
func newHarness(t *testing.T, adminPassword string) *harness {
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

	upstream := &upstreamStub{models: []string{"extra-model", "seed-model"}}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(upstreamSrv.Close)

	// 至少需要一个启用的供应商，否则 NewSnapshot 会拒绝（与运行期约束一致）
	if _, err := st.SaveProvider(context.Background(), master, config.Provider{
		ID:            "seed",
		Name:          "seed",
		OpenAIBaseURL: upstreamSrv.URL + "/v1",
		APIKey:        "sk-seed",
		Models:        []string{"seed-model"},
		AllowInternal: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	snap, err := buildSnapshot(context.Background(), st, master)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	holder := config.NewHolder(snap)

	h := &harness{t: t, store: st, master: master, holder: holder, upstream: upstream, upstreamURL: upstreamSrv.URL, dataDir: dir}
	mux := http.NewServeMux()
	New(Options{
		Store:  st,
		Master: master,
		Holder: holder,
		Reload: func(ctx context.Context) (*config.Snapshot, error) {
			h.reloadCount++
			return buildSnapshot(ctx, st, master)
		},
		Version:       "test-version",
		AdminPassword: adminPassword,
		LocalOnly:     true,
		Paths: DataPaths{
			DataDir:       dir,
			DBPath:        filepath.Join(dir, "agora.db"),
			MasterKeyPath: filepath.Join(dir, "master.key"),
		},
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
	h := newHarness(t, "")
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
	if body["models"].(float64) != 1 {
		t.Fatalf("models = %v, 期望 1（seed 勾选了一个模型）", body["models"])
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
	h := newHarness(t, testAdminPassword)
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
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name":            "供应商A",
		"openai_base_url": h.upstreamURL + "/v1",
		"api_key":         "sk-live-abcdefghijkl",
		"models_selected": []string{"m1", "m1", " m2 "},
		"model_aliases":   map[string]string{"m1": " 别名一 "},
		"allow_internal":  true,
		"timeout_seconds": 30,
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
	models, _ := body["models_selected"].([]any)
	if len(models) != 2 {
		t.Errorf("models_selected 应去重去空白，得到 %v", models)
	}
	exposed, _ := body["exposed_models"].([]any)
	if len(exposed) != 2 || exposed[0] != "别名一" {
		t.Errorf("exposed_models 应使用别名，得到 %v", exposed)
	}
	if body["model_count"].(float64) != 2 {
		t.Errorf("model_count = %v", body["model_count"])
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
	if body["model_count"].(float64) != 2 {
		t.Errorf("未提供 models_selected 时应沿用原值: %v", body["model_count"])
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
			if p.ModelAliases["m1"] != "别名一" {
				t.Errorf("别名应被保存: %#v", p.ModelAliases)
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
	h := newHarness(t, "")
	client := newClient(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺少 api_key", map[string]any{"name": "x", "openai_base_url": h.upstreamURL + "/v1"}},
		{"缺少名称", map[string]any{"openai_base_url": h.upstreamURL + "/v1", "api_key": "sk-aaaaaaaaaaaa"}},
		{"缺少上游地址", map[string]any{"name": "x", "api_key": "sk-aaaaaaaaaaaa"}},
		{"名称重复", map[string]any{"name": "seed", "openai_base_url": h.upstreamURL + "/v1", "api_key": "sk-aaaaaaaaaaaa"}},
		{"别名冲突", map[string]any{
			"name": "y", "openai_base_url": h.upstreamURL + "/v1", "api_key": "sk-aaaaaaaaaaaa",
			"models_selected": []string{"m1", "m2"}, "model_aliases": map[string]string{"m1": "同名", "m2": "同名"},
		}},
		{"内网地址未放行", map[string]any{
			"name": "z", "openai_base_url": "http://127.0.0.1:11434/v1", "api_key": "sk-aaaaaaaaaaaa",
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

func TestRenameToExistingNameIsRejected(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name": "另一个", "openai_base_url": h.upstreamURL + "/v1", "api_key": "sk-aaaaaaaaaaaa",
		"allow_internal": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d（%s）", code, raw)
	}
	id, _ := body["id"].(string)

	code, _, _ = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{"name": "SEED"})
	if code != http.StatusBadRequest {
		t.Fatalf("重名应被拒绝（忽略大小写），状态码 = %d", code)
	}
}

func TestTestProviderEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers/seed/test", nil)
	if code != http.StatusOK {
		t.Fatalf("连接测试状态码 = %d（%s）", code, raw)
	}
	if body["ok"] != true {
		t.Fatalf("连接测试应通过: %v", body)
	}
	if body["model_count"].(float64) != 2 {
		t.Errorf("model_count = %v", body["model_count"])
	}
	if _, ok := body["checked_at"].(string); !ok {
		t.Error("缺少 checked_at")
	}
}

func TestFetchModelsEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers/seed/fetch-models", nil)
	if code != http.StatusOK {
		t.Fatalf("拉取模型状态码 = %d（%s）", code, raw)
	}
	candidates, _ := body["candidate_models"].([]any)
	if len(candidates) != 2 {
		t.Errorf("候选模型 = %v", candidates)
	}
	if body["last_fetch_at"] == "" {
		t.Errorf("last_fetch_at 应被设置: %v", body)
	}
	// 拉取候选不应改变已勾选的模型
	if body["model_count"].(float64) != 1 {
		t.Errorf("model_count = %v, 期望仍为 1", body["model_count"])
	}

	h.upstream.setFail(true)
	code, body, _ = h.do(client, http.MethodPost, "/api/providers/seed/fetch-models", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("失败时状态码 = %d, 期望 502", code)
	}
	if _, ok := body["error"]; !ok {
		t.Error("失败响应应包含 error")
	}
}

func TestModelsList(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	// 再建一个供应商：与 seed 共享模型，但对 seed-model 配置了别名
	code, _, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name":            "别名供应商",
		"openai_base_url": h.upstreamURL + "/v1",
		"api_key":         "sk-aaaaaaaaaaaa",
		"models_selected": []string{"seed-model", "extra-model"},
		"model_aliases":   map[string]string{"seed-model": "共享别名"},
		"allow_internal":  true,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d（%s）", code, raw)
	}

	code, body, _ := h.do(client, http.MethodGet, "/api/models", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	type row = map[string]any
	byModel := map[string]row{}
	for _, raw := range items {
		entry := raw.(row)
		byModel[entry["model"].(string)] = entry
	}

	seedRow, ok := byModel["seed-model"]
	if !ok {
		t.Fatalf("缺少 seed-model: %v", items)
	}
	if seedRow["provider_name"] != "seed" {
		t.Errorf("provider_name = %v", seedRow["provider_name"])
	}
	if seedRow["default"] != true {
		t.Errorf("名称最小者应为默认路由: %v", seedRow)
	}
	aliasRow, ok := byModel["共享别名"]
	if !ok {
		t.Fatalf("缺少别名条目: %v", items)
	}
	if aliasRow["upstream_model"] != "seed-model" {
		t.Errorf("别名条目应同时给出上游模型名: %v", aliasRow)
	}
	if aliasRow["alias"] != "共享别名" {
		t.Errorf("别名条目应标注别名: %v", aliasRow)
	}
	// 别名是独立的对外模型名，只有这一个供应商提供它 → 它就是该名的默认路由
	if aliasRow["default"] != true {
		t.Errorf("唯一提供者的对外模型名应标记默认: %v", aliasRow)
	}
	extraRow, ok := byModel["extra-model"]
	if !ok {
		t.Fatalf("缺少 extra-model: %v", items)
	}
	if extraRow["default"] != true {
		t.Errorf("唯一提供者的模型应标记默认: %v", extraRow)
	}
}

func TestModelsListDefaultUsesProviderNameOrder(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	for _, name := range []string{"zeta 供应商", "alpha 供应商"} {
		code, _, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
			"name":            name,
			"openai_base_url": h.upstreamURL + "/v1",
			"api_key":         "sk-aaaaaaaaaaaa",
			"models_selected": []string{"shared-model"},
			"allow_internal":  true,
		})
		if code != http.StatusCreated {
			t.Fatalf("创建 %s 状态码 = %d（%s）", name, code, raw)
		}
	}

	code, body, _ := h.do(client, http.MethodGet, "/api/models", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	defaults := map[string]bool{}
	seen := map[string]bool{}
	for _, raw := range items {
		entry := raw.(map[string]any)
		if entry["model"] != "shared-model" {
			continue
		}
		name := entry["provider_name"].(string)
		seen[name] = true
		if entry["default"] == true {
			defaults[name] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("两个供应商都应出现在列表中: %v", seen)
	}
	if !defaults["alpha 供应商"] || defaults["zeta 供应商"] {
		t.Fatalf("默认路由应按供应商名称升序选择，得到 %v", defaults)
	}
}

func TestManualModelEndpointsRemoved(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, _, _ := h.do(client, http.MethodPost, "/api/models/manual", map[string]any{"provider_id": "seed", "model": "m"})
	if code != http.StatusNotFound {
		t.Fatalf("手动模型接口应已下线，状态码 = %d, 期望 404", code)
	}
	code, _, _ = h.do(client, http.MethodPost, "/api/models/refresh", nil)
	if code != http.StatusNotFound {
		t.Fatalf("全量刷新接口应已下线，状态码 = %d, 期望 404", code)
	}
}

func TestSettingsAndGatewayKeyReset(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	// 首次需要生成一个网关 Key（运行期由 main 调用 EnsureGatewayKey）
	if _, _, raw := h.do(client, http.MethodPost, "/api/gateway-key/reset", nil); raw == "" {
		t.Fatal("生成网关 Key 失败")
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

	code, body, raw = h.do(client, http.MethodPut, "/api/settings", map[string]any{"log_success": true})
	if code != http.StatusOK {
		t.Fatalf("写入设置状态码 = %d（%s）", code, raw)
	}
	if body["log_success"] != "true" {
		t.Errorf("log_success = %v", body["log_success"])
	}
	if _, exists := body["model_refresh_seconds"]; exists {
		t.Errorf("模型刷新间隔设置应已移除: %v", body)
	}
}

func TestLogsEndpointAndFuzzyModelFilter(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	entries := []logging.Entry{
		{TS: time.Now().UTC(), RequestID: "r1", InboundProtocol: "openai", Model: "GPT-4O", ProviderID: "seed", StatusCode: 200},
		{TS: time.Now().UTC(), RequestID: "r2", InboundProtocol: "openai", Model: "claude-3-5", ProviderID: "seed", StatusCode: 502,
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

	// 模型筛选：忽略大小写 + 模糊匹配
	for _, keyword := range []string{"gpt-4o", "GPT", "4o", "claude-3"} {
		code, body, _ = h.do(client, http.MethodGet, "/api/logs?model="+keyword, nil)
		if code != http.StatusOK {
			t.Fatalf("查询 %q 状态码 = %d", keyword, code)
		}
		got, _ := body["items"].([]any)
		if len(got) != 1 {
			t.Fatalf("关键词 %q 应命中 1 条，实际 %d", keyword, len(got))
		}
	}

	// LIKE 通配符按字面匹配（% 不应变成「匹配全部」）
	code, body, _ = h.do(client, http.MethodGet, "/api/logs?model=%25", nil)
	if code != http.StatusOK {
		t.Fatalf("通配符查询状态码 = %d", code)
	}
	if got, _ := body["items"].([]any); len(got) != 0 {
		t.Fatalf("%% 应按字面匹配，实际命中 %d 条", len(got))
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodGet, "/api/export", nil)
	if code != http.StatusOK {
		t.Fatalf("导出状态码 = %d", code)
	}
	if body["format"] != exportFormat {
		t.Errorf("导出格式标识 = %v", body["format"])
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("导出供应商数 = %d", len(providers))
	}
	exported := providers[0].(map[string]any)
	// 导出包含 API Key 明文（换机迁移用），并带上官网地址
	if exported["api_key"] != "sk-seed" {
		t.Fatalf("导出应包含明文凭证，得到 %v", exported["api_key"])
	}
	if _, ok := exported["website_url"]; !ok {
		t.Errorf("导出应包含 website_url: %v", exported)
	}

	// 导入时沿用导出文件里的明文凭证（换机迁移的常规路径）
	exported["id"] = "imported"
	exported["name"] = "导入的供应商"
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
			if p.APIKey != "sk-seed" {
				t.Errorf("导入的凭证 = %q", p.APIKey)
			}
			if len(p.Models) != 1 || p.Models[0] != "seed-model" {
				t.Errorf("导入的模型选择 = %v", p.Models)
			}
		}
	}
	if !found {
		t.Fatal("导入后未找到供应商")
	}

	// 缺少 api_key 的导入应被拒绝
	delete(exported, "id")
	exported["name"] = "无凭证"
	delete(exported, "api_key")
	code, _, _ = h.do(client, http.MethodPost, "/api/import", map[string]any{"providers": []any{exported}})
	if code != http.StatusBadRequest {
		t.Fatalf("缺少凭证的导入状态码 = %d, 期望 400", code)
	}

	// 空文件应给出明确错误
	code, _, _ = h.do(client, http.MethodPost, "/api/import", map[string]any{"providers": []any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("空导入状态码 = %d, 期望 400", code)
	}
}

func TestProviderListIncludesFetchStatusAndCandidates(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	if err := h.store.ReplaceCandidateModels(context.Background(), "seed", []string{"auto-1", "auto-2"}, time.Now().UTC()); err != nil {
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
	candidates, _ := seed["candidate_models"].([]any)
	if len(candidates) != 2 {
		t.Errorf("candidate_models = %v", candidates)
	}
}

// TestProviderWebsiteURL 覆盖官网地址字段：创建、更新沿用、显式清空与非法值。
func TestProviderWebsiteURL(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name":            "带官网",
		"openai_base_url": h.upstreamURL + "/v1",
		"api_key":         "sk-aaaaaaaaaaaa",
		"website_url":     " https://example.com/pricing ",
		"allow_internal":  true,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d（%s）", code, raw)
	}
	id, _ := body["id"].(string)
	if body["website_url"] != "https://example.com/pricing" {
		t.Errorf("website_url 应去掉首尾空白，得到 %v", body["website_url"])
	}

	// 未提供 website_url → 沿用原值
	code, body, raw = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{"name": "带官网-改名"})
	if code != http.StatusOK {
		t.Fatalf("更新状态码 = %d（%s）", code, raw)
	}
	if body["website_url"] != "https://example.com/pricing" {
		t.Errorf("未提供时应沿用官网地址，得到 %v", body["website_url"])
	}

	// 显式传空串 → 清空
	code, body, raw = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{"name": "带官网-改名", "website_url": ""})
	if code != http.StatusOK {
		t.Fatalf("清空状态码 = %d（%s）", code, raw)
	}
	if body["website_url"] != "" {
		t.Errorf("显式空串应清空官网地址，得到 %v", body["website_url"])
	}

	// 非法地址（非 http/https）应被拒绝
	code, _, _ = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{
		"name": "带官网-改名", "website_url": "ftp://example.com",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("非法官网地址状态码 = %d, 期望 400", code)
	}
}

// TestFetchDraftModelsEndpoint 覆盖「未保存的供应商」直接拉取模型。
func TestFetchDraftModelsEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/providers/fetch-models", map[string]any{
		"openai_base_url": h.upstreamURL + "/v1",
		"api_key":         "sk-draft-aaaaaaaa",
		"allow_internal":  true,
	})
	if code != http.StatusOK {
		t.Fatalf("草稿拉取状态码 = %d（%s）", code, raw)
	}
	candidates, _ := body["candidate_models"].([]any)
	if len(candidates) != 2 {
		t.Errorf("候选模型 = %v", candidates)
	}
	if body["fetched_at"] == "" {
		t.Errorf("fetched_at 应被设置: %v", body)
	}
	// 草稿拉取不得落库任何供应商
	records, err := h.store.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Errorf("草稿拉取不应新增供应商，当前 %d 条", len(records))
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺少 API Key", map[string]any{"openai_base_url": h.upstreamURL + "/v1"}},
		{"缺少上游地址", map[string]any{"api_key": "sk-draft-aaaaaaaa"}},
		{"内网地址未放行", map[string]any{"openai_base_url": "http://127.0.0.1:11434/v1", "api_key": "sk-draft-aaaaaaaa"}},
		{"端点覆盖模式", map[string]any{
			"openai_base_url": h.upstreamURL + "/v1", "openai_endpoint_override": h.upstreamURL + "/v1/chat/completions",
			"api_key": "sk-draft-aaaaaaaa", "allow_internal": true,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := h.do(client, http.MethodPost, "/api/providers/fetch-models", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400（%s）", code, raw)
			}
		})
	}

	// 上游拒绝 → 502
	h.upstream.setFail(true)
	code, _, _ = h.do(client, http.MethodPost, "/api/providers/fetch-models", map[string]any{
		"openai_base_url": h.upstreamURL + "/v1", "api_key": "sk-draft-aaaaaaaa", "allow_internal": true,
	})
	if code != http.StatusBadGateway {
		t.Fatalf("上游失败时状态码 = %d, 期望 502", code)
	}
}

// TestGatewayKeyPlaintextForConsole 覆盖「网关 Key 可随时查看/复制」。
func TestGatewayKeyPlaintextForConsole(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/gateway-key/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("重置状态码 = %d（%s）", code, raw)
	}
	plain, _ := body["gateway_key"].(string)
	if plain == "" {
		t.Fatalf("重置响应应包含明文: %v", body)
	}

	// 之后仍可从设置接口取回同一明文（页面复制按钮用）
	code, body, raw = h.do(client, http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读取设置状态码 = %d（%s）", code, raw)
	}
	if body["gateway_key"] != plain {
		t.Errorf("设置接口应返回可复制的明文 Key，得到 %v", body["gateway_key"])
	}
	if body["gateway_key_revealable"] != true {
		t.Errorf("gateway_key_revealable = %v", body["gateway_key_revealable"])
	}
	if body["gateway_key_hint"] != crypto.Mask(plain) {
		t.Errorf("掩码 = %v", body["gateway_key_hint"])
	}

	// 旧版本只存哈希的记录：无法查看，但不影响校验
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE gateway_keys SET key_cipher = NULL`); err != nil {
		t.Fatal(err)
	}
	code, body, raw = h.do(client, http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("只存哈希的记录不应让接口报错，状态码 = %d（%s）", code, raw)
	}
	if body["gateway_key"] != "" || body["gateway_key_revealable"] != false {
		t.Errorf("只存哈希的记录应标记为不可查看: %v（%s）", body, raw)
	}

	// 主密钥被更换（密文无法解密）：同样降级为「不可查看 + 提示重置」，而不是 500
	bogus := make([]byte, 64)
	for i := range bogus {
		bogus[i] = byte(i + 1)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE gateway_keys SET key_cipher = ?`, bogus); err != nil {
		t.Fatal(err)
	}
	code, body, raw = h.do(client, http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("密文无法解密时应降级为不可查看，状态码 = %d（%s）", code, raw)
	}
	if body["gateway_key"] != "" || body["gateway_key_revealable"] != false {
		t.Errorf("密文无法解密时应标记为不可查看: %v（%s）", body, raw)
	}
	if body["gateway_key_hint"] == "" {
		t.Errorf("掩码仍应回显: %v", body)
	}
}

// TestDataLocationsEndpoint 覆盖「数据位置」页面接口。
func TestDataLocationsEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodGet, "/api/data-locations", nil)
	if code != http.StatusOK {
		t.Fatalf("数据位置状态码 = %d（%s）", code, raw)
	}
	if body["os"] == "" || body["os_label"] == "" {
		t.Errorf("缺少系统信息: %v", body)
	}
	if body["version"] != "test-version" {
		t.Errorf("version = %v", body["version"])
	}
	if body["data_dir"] != h.dataDir {
		t.Errorf("data_dir = %v, 期望 %v", body["data_dir"], h.dataDir)
	}

	items, _ := body["items"].([]any)
	byKey := map[string]map[string]any{}
	for _, item := range items {
		entry := item.(map[string]any)
		byKey[entry["key"].(string)] = entry
	}
	db, ok := byKey["database"]
	if !ok {
		t.Fatalf("缺少数据库项: %v", items)
	}
	if db["path"] != filepath.Join(h.dataDir, "agora.db") {
		t.Errorf("数据库路径 = %v", db["path"])
	}
	if db["exists"] != true || db["size_bytes"].(float64) <= 0 {
		t.Errorf("数据库项应报告存在与大小: %v", db)
	}
	if byKey["data_dir"]["exists"] != true {
		t.Errorf("数据目录应存在: %v", byKey["data_dir"])
	}
	if _, ok := byKey["master_key"]; !ok {
		t.Errorf("缺少主密钥项: %v", items)
	}
	// 未配置日志文件：前台运行写 stdout
	if byKey["log_file"]["kind"] != "stdout" {
		t.Errorf("log_file kind = %v", byKey["log_file"]["kind"])
	}
}

// TestHumanSize 覆盖大小格式化（页面展示用）。
func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		512:                    "512 B",
		2048:                   "2.0 KB",
		5 * 1024 * 1024:        "5.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for input, want := range cases {
		if got := humanSize(input); got != want {
			t.Errorf("humanSize(%d) = %q, 期望 %q", input, got, want)
		}
	}
}
