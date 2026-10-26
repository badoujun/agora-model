package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/logging"
)

// fakeKeyStats 记录网关对凭证健康状态的所有通知（内存实现，避免依赖 store）。
type fakeKeyStats struct {
	mu        sync.Mutex
	touched   []string
	failures  map[string]int
	disabled  map[string]string
	touchCall int
}

func newFakeKeyStats() *fakeKeyStats {
	return &fakeKeyStats{failures: map[string]int{}, disabled: map[string]string{}}
}

func (f *fakeKeyStats) TouchProviderKey(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, keyID)
	f.touchCall++
}

func (f *fakeKeyStats) RecordProviderKeyFailure(keyID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[keyID]++
}

func (f *fakeKeyStats) DisableProviderKey(providerID, keyID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[keyID]++
	f.disabled[keyID] = reason
}

func (f *fakeKeyStats) snapshot() (touched []string, failures map[string]int, disabled map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]int{}
	for k, v := range f.failures {
		cp[k] = v
	}
	dis := map[string]string{}
	for k, v := range f.disabled {
		dis[k] = v
	}
	return append([]string(nil), f.touched...), cp, dis
}

// keyProviderFor 构造带多个具名 Key 的供应商（id 非空，便于断言运行时状态）。
func keyProviderFor(id, upstreamURL string, keys map[string]string, models ...string) config.Provider {
	p := config.Provider{
		ID:            id,
		Name:          id,
		OpenAIBaseURL: upstreamURL + "/v1",
		Models:        models,
	}
	for keyID, secret := range keys {
		enabled := true
		p.APIKeys = append(p.APIKeys, config.ProviderKey{ID: keyID, Label: keyID, Key: secret, Enabled: &enabled})
	}
	// map 遍历顺序随机：按 id 排序保证轮询顺序可预期
	sortKeys(p.APIKeys)
	return p
}

func sortKeys(keys []config.ProviderKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1].ID > keys[j].ID; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}

// buildGatewayWithKeys 构造网关并返回服务地址、网关对象与统计接收器。
func buildGatewayWithKeys(t *testing.T, providers ...config.Provider) (*httptest.Server, *Gateway, *fakeKeyStats) {
	t.Helper()
	gw := config.Gateway{APIKey: gatewayKey, MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30}
	snap, err := config.NewSnapshot(config.File{Gateway: gw, Providers: providers})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	stats := newFakeKeyStats()
	g := New(config.NewHolder(snap), testLogger()).WithProviderKeyStats(stats)
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, g, stats
}

// authRecorder 记录每个请求收到的 Authorization，并按 key 决定响应。
type authRecorder struct {
	mu     sync.Mutex
	seen   []string
	status map[string]int // Authorization 值 -> 状态码（默认 200）
	body   map[string]string
}

func (a *authRecorder) handler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	a.mu.Lock()
	a.seen = append(a.seen, auth)
	status := a.status[auth]
	body := a.body[auth]
	a.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = `{"ok":true}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (a *authRecorder) auths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func TestKeyRoundRobinAlternatesAuthorization(t *testing.T) {
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2"}, "m")
	srv, _, _ := buildGatewayWithKeys(t, p)

	for i := 0; i < 4; i++ {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d（%s）", i+1, resp.StatusCode, body)
		}
	}

	auths := rec.auths()
	want := []string{"Bearer sk-1", "Bearer sk-2", "Bearer sk-1", "Bearer sk-2"}
	if strings.Join(auths, ",") != strings.Join(want, ",") {
		t.Fatalf("多 Key 应按顺序轮询，实际 %v，期望 %v", auths, want)
	}
}

func TestKeyFailoverOn401DisablesFailedKey(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-bad": http.StatusUnauthorized},
		body: map[string]string{"Bearer sk-bad": `{"error":{"message":"invalid key"}}`}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-bad", "k2": "sk-good"}, "m")
	srv, _, stats := buildGatewayWithKeys(t, p)

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("首个 Key 401 后应自动切换：状态码 = %d，body = %s", resp.StatusCode, body)
	}

	_, failures, disabled := stats.snapshot()
	if disabled["k1"] == "" {
		t.Fatalf("失败的 Key 应被自动停用，disabled=%v failures=%v", disabled, failures)
	}
	if failures["k1"] != 1 {
		t.Errorf("失败计数 = %d，期望 1", failures["k1"])
	}
	if len(rec.auths()) != 2 {
		t.Fatalf("应尝试两个 Key，实际 %v", rec.auths())
	}

	// 第二个请求不应再使用被停用的 k1
	before := len(rec.auths())
	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("后续请求应仍然成功，状态码 = %d", resp.StatusCode)
	}
	auths := rec.auths()[before:]
	if len(auths) != 1 || auths[0] != "Bearer sk-good" {
		t.Fatalf("被自动停用的 Key 不应再参与轮询，实际 %v", auths)
	}
}

func TestKeyRateLimitDisablesFailedKeyLikePaymentRequired(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-limited": http.StatusTooManyRequests},
		body: map[string]string{"Bearer sk-limited": `{"error":{"message":"rate limited"}}`}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-limited", "k2": "sk-good"}, "m")
	srv, _, stats := buildGatewayWithKeys(t, p)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("429 后应切换到下一个 Key，状态码 = %d", resp.StatusCode)
	}

	_, failures, disabled := stats.snapshot()
	if disabled["k1"] == "" {
		t.Fatalf("429 应与 402 一样自动停用 Key：disabled=%v", disabled)
	}
	if !strings.Contains(disabled["k1"], "429") {
		t.Errorf("停用原因应包含状态码，得到 %q", disabled["k1"])
	}
	if failures["k1"] != 1 {
		t.Errorf("429 也应计入失败次数，得到 %d", failures["k1"])
	}

	// 后续请求不应再使用被停用的 k1
	before := len(rec.auths())
	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("后续请求应仍然成功，状态码 = %d", resp.StatusCode)
	}
	auths := rec.auths()[before:]
	if len(auths) != 1 || auths[0] != "Bearer sk-good" {
		t.Fatalf("被自动停用的 Key 不应再参与轮询，实际 %v", auths)
	}
}

// TestSingleKeyRateLimitDisablesKey 验证 429 与 402 同等处理：
// 即使供应商只有一个 Key，也会被自动停用（不再有"只冷却"的保护）。
func TestSingleKeyRateLimitDisablesKey(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-1": http.StatusTooManyRequests}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1"}, "m")
	srv, g, stats := buildGatewayWithKeys(t, p)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("单 Key 429 应原样透传，得到 %d", resp.StatusCode)
	}
	_, _, disabled := stats.snapshot()
	if disabled["k1"] == "" {
		t.Fatalf("唯一的 Key 也应被自动停用：%v", disabled)
	}
	if g.keyRuntime.usable(p.APIKeys[0]) {
		t.Fatal("429 后该 Key 不应再被视为可用")
	}

	// 全部 Key 停用后，后续请求返回 503（供应商停用由落库层联动完成）
	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Key 全部停用后应返回 503，得到 %d（%s）", resp.StatusCode, body)
	}
}

func TestKeyAllFailedPassesLastUpstreamErrorThrough(t *testing.T) {
	rec := &authRecorder{status: map[string]int{
		"Bearer sk-1": http.StatusUnauthorized,
		"Bearer sk-2": http.StatusForbidden,
	}, body: map[string]string{
		"Bearer sk-1": `{"error":{"message":"key1 invalid"}}`,
		"Bearer sk-2": `{"error":{"message":"key2 forbidden"}}`,
	}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2"}, "m")
	srv, _, stats := buildGatewayWithKeys(t, p)

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("全部 Key 失败时应透传最后一次上游状态码，得到 %d", resp.StatusCode)
	}
	if body != `{"error":{"message":"key2 forbidden"}}` {
		t.Fatalf("上游错误体应原样透传，得到 %s", body)
	}
	_, failures, disabled := stats.snapshot()
	if disabled["k1"] == "" {
		t.Fatalf("失败的 Key 应被自动停用：%v", disabled)
	}
	if disabled["k2"] == "" {
		t.Fatalf("最后一个可用 Key 也应被自动停用：%v", disabled)
	}
	if failures["k1"] == 0 || failures["k2"] == 0 {
		t.Fatalf("两个 Key 的失败都应计数：%v", failures)
	}

	// 全部 Key 已停用：下一次请求直接 503，不再打上游
	resp, body = doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("全部 Key 停用后应返回 503，得到 %d（%s）", resp.StatusCode, body)
	}
}

// TestAllKeysDisabledReturns503 覆盖"配置层面没有任何可用 Key"：
// 路由不再命中该供应商，返回 503 并说明原因。
func TestAllKeysDisabledReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	disabled := false
	p := config.Provider{
		ID: "p", Name: "p", OpenAIBaseURL: upstream.URL + "/v1", Models: []string{"m"},
		APIKeys: []config.ProviderKey{{ID: "k1", Key: "sk-1", Enabled: &disabled}},
	}
	srv, _, _ := buildGatewayWithKeys(t, p)

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("没有可用 Key 时应返回 503，得到 %d（%s）", resp.StatusCode, body)
	}
	if code := decodeJSON(t, body)["error"].(map[string]any)["code"]; code != "no_available_provider" {
		t.Errorf("code = %v，期望 no_available_provider", code)
	}
}

// TestSingleKeyFailureDisablesKey 覆盖"只有一个 Key"的情况：
// 401 会直接停用该 Key（不再只冷却），客户端仍看到原始 401；后续请求 503。
func TestSingleKeyFailureDisablesKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1"}, "m")
	srv, g, stats := buildGatewayWithKeys(t, p)

	resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("单 Key 失败应原样透传 401，得到 %d", resp.StatusCode)
	}
	_, _, disabled := stats.snapshot()
	if disabled["k1"] == "" {
		t.Fatalf("唯一的 Key 也应被自动停用：%v", disabled)
	}
	if g.keyRuntime.usable(p.APIKeys[0]) {
		t.Fatal("失败后该 Key 不应再被视为可用")
	}

	// 恢复必须由人工触发：重新启用 Key 前持续 503
	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Key 停用后应返回 503，得到 %d（%s）", resp.StatusCode, body)
	}
}

func TestKeyServerErrorIsNotRetried(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-1": http.StatusInternalServerError},
		body: map[string]string{"Bearer sk-1": `{"error":{"message":"boom"}}`}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2"}, "m")
	srv, _, stats := buildGatewayWithKeys(t, p)

	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("5xx 应原样透传（不重试），得到 %d", resp.StatusCode)
	}
	if body != `{"error":{"message":"boom"}}` {
		t.Errorf("5xx 错误体应原样透传，得到 %s", body)
	}
	if auths := rec.auths(); len(auths) != 1 {
		t.Fatalf("5xx 不应触发故障转移，实际尝试 %v", auths)
	}
	_, failures, disabled := stats.snapshot()
	if len(failures) != 0 || len(disabled) != 0 {
		t.Errorf("5xx 不应改变 Key 状态：failures=%v disabled=%v", failures, disabled)
	}
}

func TestKeyRateLimitedKeySkippedOnNextRequest(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-1": http.StatusTooManyRequests}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2"}, "m")
	srv, _, _ := buildGatewayWithKeys(t, p)

	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	// 第一次请求：k1(429) -> k2(200)；第二次请求应直接用 k2（k1 已被自动停用）
	before := len(rec.auths())
	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	auths := rec.auths()[before:]
	if len(auths) != 1 || auths[0] != "Bearer sk-2" {
		t.Fatalf("已自动停用的 Key 应被跳过，实际 %v", auths)
	}
}

// TestKeyReEnableAfterNewSnapshot 覆盖"用户在界面上重新启用"的语义：
// 新快照里重新出现该 Key 时，运行时的自动停用标记必须被清掉。
func TestKeyReEnableAfterNewSnapshot(t *testing.T) {
	rec := &authRecorder{status: map[string]int{"Bearer sk-1": http.StatusUnauthorized}}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2"}, "m")
	srv, g, _ := buildGatewayWithKeys(t, p)

	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if g.keyRuntime.usable(p.APIKeys[0]) {
		t.Fatal("k1 应已因 401 被自动停用")
	}

	// 模拟用户重新启用：构造"包含全部 Key"的新快照
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{APIKey: gatewayKey, MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: []config.Provider{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.holder.Store(snap)

	// 新快照在请求路径上被 observe 时复位 k1
	g.observe(snap)
	if !g.keyRuntime.usable(p.APIKeys[0]) {
		t.Fatal("新快照里重新出现的 Key 应恢复健康")
	}
}

// TestKeyStatsTouchRecordedOnSuccess 验证成功请求会刷新 last_used（异步接收器）。
func TestKeyStatsTouchRecordedOnSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1"}, "m")
	srv, _, stats := buildGatewayWithKeys(t, p)

	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	touched, _, _ := stats.snapshot()
	if len(touched) != 1 || touched[0] != "k1" {
		t.Fatalf("成功请求应记录 Key 使用：%v", touched)
	}
}

// TestKeySelectionConcurrent 覆盖并发下的轮询与健康判定（配合 -race 运行）。
// capturingLogWriter 捕获异步写入的请求日志条目，供断言使用。
type capturingLogWriter struct {
	mu      sync.Mutex
	entries []logging.Entry
}

func (w *capturingLogWriter) InsertLogs(_ context.Context, entries []logging.Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, entries...)
	return nil
}

// wait 等待第一条日志写入（最多 3 秒），返回最新一条。
func (w *capturingLogWriter) wait(t *testing.T) logging.Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		if len(w.entries) > 0 {
			entry := w.entries[len(w.entries)-1]
			w.mu.Unlock()
			return entry
		}
		w.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待请求日志写入超时")
	return logging.Entry{}
}

// buildGatewayWithLogs 构造带请求日志 recorder 的网关，返回服务地址与日志捕获器。
func buildGatewayWithLogs(t *testing.T, providers ...config.Provider) (*httptest.Server, *capturingLogWriter) {
	t.Helper()
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{APIKey: gatewayKey, MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	writer := &capturingLogWriter{}
	recorder := logging.NewRecorder(writer, testLogger(), logging.RecorderOptions{
		QueueSize: 64, BatchSize: 1, FlushEvery: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	recorder.Start(ctx)
	t.Cleanup(func() {
		cancel()
		recorder.Wait(2 * time.Second)
	})

	g := New(config.NewHolder(snap), testLogger()).
		WithProviderKeyStats(newFakeKeyStats()).
		WithRecorder(recorder)
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, writer
}

// TestKeyFailuresRecordedInRequestLog 验证凭证失败信息会写入请求日志的 error_msg：
// 即使后续 Key 成功（客户端看到 200），失败原因也不会丢；全部失败时每一次尝试都有记录。
func TestKeyFailuresRecordedInRequestLog(t *testing.T) {
	t.Run("失败后切换成功仍记录", func(t *testing.T) {
		rec := &authRecorder{
			status: map[string]int{"Bearer sk-limited": http.StatusTooManyRequests},
			body:   map[string]string{"Bearer sk-limited": `{"error":{"message":"rate limited"}}`},
		}
		upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
		defer upstream.Close()

		p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-limited", "k2": "sk-good-key"}, "m")
		srv, writer := buildGatewayWithLogs(t, p)

		resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("首个 Key 429 后应切换成功，状态码 = %d", resp.StatusCode)
		}

		entry := writer.wait(t)
		if entry.StatusCode != http.StatusOK {
			t.Errorf("请求日志状态码 = %d，期望 200", entry.StatusCode)
		}
		for _, want := range []string{"sk-****ited", "HTTP 429", "rate limited", "已自动停用"} {
			if !strings.Contains(entry.ErrorMsg, want) {
				t.Errorf("请求日志 error_msg 缺少 %q：%q", want, entry.ErrorMsg)
			}
		}
		if entry.KeyHint != "sk-****-key" {
			t.Errorf("请求日志 key_hint = %q，期望实际服务 Key 的掩码", entry.KeyHint)
		}
	})

	t.Run("全部失败记录每一次尝试", func(t *testing.T) {
		rec := &authRecorder{
			status: map[string]int{
				"Bearer sk-unpaid":  http.StatusPaymentRequired,
				"Bearer sk-limited": http.StatusTooManyRequests,
			},
			body: map[string]string{
				"Bearer sk-unpaid": `{"error":{"message":"insufficient balance"}}`,
			},
		}
		upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
		defer upstream.Close()

		p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-unpaid", "k2": "sk-limited"}, "m")
		srv, writer := buildGatewayWithLogs(t, p)

		resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("全部失败时应透传最后一次上游状态码 429，得到 %d", resp.StatusCode)
		}

		entry := writer.wait(t)
		if entry.StatusCode != http.StatusTooManyRequests {
			t.Errorf("请求日志状态码 = %d，期望 429", entry.StatusCode)
		}
		// 第一次（402）与最后一次（429）都会被自动停用，且最后一次触发供应商联动停用
		for _, want := range []string{"HTTP 402", "insufficient balance", "已自动停用", "HTTP 429", "已联动停用供应商"} {
			if !strings.Contains(entry.ErrorMsg, want) {
				t.Errorf("请求日志 error_msg 缺少 %q：%q", want, entry.ErrorMsg)
			}
		}
	})
}

// TestDescribeKeyFailures 覆盖空列表与无原因时的格式化兜底。
func TestDescribeKeyFailures(t *testing.T) {
	if got := describeKeyFailures(nil); got != "" {
		t.Errorf("空失败列表应返回空串，得到 %q", got)
	}
	got := describeKeyFailures([]keyFailure{
		{keyHint: "sk-****aaaa", status: http.StatusTooManyRequests, reason: "HTTP 429", action: "已自动停用"},
		{keyHint: "sk-****bbbb", status: http.StatusPaymentRequired, action: "已自动停用（该供应商已无可用 Key，已联动停用供应商）"},
	})
	want := "Key sk-****aaaa: HTTP 429（已自动停用）；Key sk-****bbbb: HTTP 402（已自动停用（该供应商已无可用 Key，已联动停用供应商））"
	if got != want {
		t.Errorf("失败摘要 = %q，期望 %q", got, want)
	}
}

func TestKeySelectionConcurrent(t *testing.T) {
	rec := &authRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1", "k2": "sk-2", "k3": "sk-3"}, "m")
	srv, g, _ := buildGatewayWithKeys(t, p)

	snap := g.holder.Get()
	keys := snap.Providers()[0].UsableKeys()
	rt := g.keyRuntime

	const workers = 32
	const perWorker = 64
	var wg sync.WaitGroup
	selErrs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				order := rt.order("p", keys, maxKeyAttempts)
				if len(order) == 0 {
					// 并发下其它 goroutine 可能刚好把所有 Key 标记为自动停用、
					// 尚未 forget：候选为空是合法结果（调用方会回 503）。
					continue
				}
				// 候选里不应出现重复 Key
				seen := map[string]bool{}
				for _, k := range order {
					if seen[k.ID] {
						selErrs <- "候选重复: " + k.ID
						return
					}
					seen[k.ID] = true
				}
				// 模拟自动停用/恢复标记，验证无数据竞争（其他 goroutine 也可能同时改状态，
				// 因此这里不假设候选数量恒为 3）
				rt.markAutoDisabled(order[0])
				rt.forget(order[0])
			}
		}()
	}
	wg.Wait()
	close(selErrs)
	for msg := range selErrs {
		t.Errorf("并发选 Key 失败: %s", msg)
	}

	// 并发请求也要能正常工作
	var wg2 sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"p/m"}`))
			if err != nil {
				errs <- err.Error()
				return
			}
			req.Header.Set("Authorization", "Bearer "+gatewayKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- "status " + resp.Status
			}
		}()
	}
	wg2.Wait()
	close(errs)
	for msg := range errs {
		t.Errorf("并发请求失败: %s", msg)
	}

	if len(rec.auths()) < 16 {
		t.Fatalf("上游收到的请求数 = %d", len(rec.auths()))
	}
	// 三个 Key 都应被轮询到
	seen := map[string]bool{}
	for _, a := range rec.auths() {
		seen[a] = true
	}
	if len(seen) != 3 {
		t.Fatalf("并发下应轮询到全部 3 个 Key，实际 %v", seen)
	}
}

// TestUsageRecordsServingKey 验证用量统计携带实际服务的 Key id。
func TestUsageRecordsServingKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	p := keyProviderFor("p", upstream.URL, map[string]string{"k1": "sk-1"}, "m")
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{APIKey: gatewayKey, MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: []config.Provider{p},
	})
	if err != nil {
		t.Fatal(err)
	}

	type record struct {
		gatewayKey, provider, key, model string
	}
	got := make(chan record, 4)
	g := New(config.NewHolder(snap), testLogger()).WithUsageSink(
		usageSinkFunc(func(gatewayKeyID, providerID, keyID, model string, statusCode int, u Usage, ts time.Time) {
			got <- record{gatewayKey: gatewayKeyID, provider: providerID, key: keyID, model: model}
		}),
	)
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if resp, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"p/m"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	select {
	case r := <-got:
		if r.provider != "p" || r.key != "k1" || r.model != "m" {
			t.Fatalf("用量记录 = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到用量记录")
	}
}

// usageSinkFunc 让测试函数满足 UsageSink（避免依赖 logging.UsageSinkFunc 的签名细节）。
type usageSinkFunc func(gatewayKeyID, providerID, keyID, model string, statusCode int, u Usage, ts time.Time)

func (f usageSinkFunc) Record(gatewayKeyID, providerID, keyID, model string, statusCode int, u Usage, ts time.Time) {
	f(gatewayKeyID, providerID, keyID, model, statusCode, u, ts)
}

// BenchmarkKeySelectionHotPath 记录纯内存选 Key 的开销（无锁、无 DB）。
func BenchmarkKeySelectionHotPath(b *testing.B) {
	keys := make([]config.ProviderKey, 0, 32)
	for i := 0; i < 32; i++ {
		enabled := true
		keys = append(keys, config.ProviderKey{
			ID:      "pk-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Label:   "k",
			Key:     "sk-secret",
			Enabled: &enabled,
		})
	}
	rt := newKeyRuntime()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if order := rt.order("p", keys, maxKeyAttempts); len(order) != maxKeyAttempts {
			b.Fatalf("候选数 = %d", len(order))
		}
	}
}
