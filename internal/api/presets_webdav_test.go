package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/presets"
	"agora-model/internal/store"
)

var _ = json.Marshal

// TestProviderPresetsList 覆盖预设列表。
func TestProviderPresetsList(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	// 默认 harness 没有 presets；GET 应给出 503
	code, _, _ := h.do(client, http.MethodGet, "/api/provider-presets", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("未启用预设时 GET 应 503，得到 %d", code)
	}

	// 重新构造一个带 presets Store 的 harness
	presetStore := presets.New(presets.Options{
		OverridePath: filepath.Join(h.dataDir, "no-presets.json"), // 不存在 → 用内置
	})
	_, harness2 := harnessWithPresets(t, presetStore)
	defer harness2.srv.Close()
	client2 := newClient(t)

	code, body, raw := harness2.do(client2, http.MethodGet, "/api/provider-presets", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/provider-presets 状态码 = %d（%s）", code, raw)
	}
	items, _ := body["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("内置预设非空：%v", items)
	}
	wantIDs := map[string]bool{"deepseek": true, "minimax": true, "scnet": true, "agnes": true}
	for _, raw := range items {
		entry := raw.(map[string]any)
		delete(wantIDs, entry["id"].(string))
	}
	if len(wantIDs) != 0 {
		t.Errorf("缺少预设：%v", wantIDs)
	}
}

// TestWebDAVStatusAndPushPull 覆盖 webdav 配置读写 + push/pull 流程。
func TestWebDAVStatusAndPushPull(t *testing.T) {
	// 远端 WebDAV mock：内存保存 PUT 内容
	var remoteBytes atomic.Value // []byte
	remoteBytes.Store([]byte(nil))
	putCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b := remoteBytes.Load().([]byte)
			if b == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			remoteBytes.Store(body)
			putCount++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	presetStore := presets.New(presets.Options{})
	server, harness := harnessWithPresets(t, presetStore)
	defer server.Close()
	client := newClient(t)

	// 初次：未配置 WebDAV
	code, body, _ := harness.do(client, http.MethodGet, "/api/sync/webdav/status", nil)
	if code != http.StatusOK || body["configured"] != false {
		t.Fatalf("未配置状态：%d %v", code, body)
	}

	// 配置 WebDAV
	code, body, raw := harness.do(client, http.MethodPut, "/api/settings/webdav", map[string]any{
		"url":      srv.URL + "/dav/",
		"username": "alice",
		"password": "s3cret",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT webdav 状态码 = %d（%s）", code, raw)
	}
	if body["configured"] != true || body["username"] != "alice" || body["has_password"] != true {
		t.Errorf("配置响应异常: %v", body)
	}
	// 响应不应回传明文密码
	if pwd, _ := body["password"].(string); pwd != "" {
		t.Errorf("响应不应包含明文密码，得到 %q", pwd)
	}

	// 状态：远端不存在 → has_remote=false
	code, body, _ = harness.do(client, http.MethodGet, "/api/sync/webdav/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status 状态码 = %d", code)
	}
	if body["configured"] != true || body["has_remote"] != false {
		t.Errorf("远端不存在时状态异常: %v", body)
	}
	if body["local_fingerprint"] == "" {
		t.Error("local_fingerprint 应被填充")
	}

	// Push（远端空 → 应允许）
	code, body, raw = harness.do(client, http.MethodPost, "/api/sync/webdav/push", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("push 状态码 = %d（%s）", code, raw)
	}
	if putCount != 1 {
		t.Errorf("PUT 次数 = %d", putCount)
	}
	firstFingerprint, _ := body["fingerprint"].(string)
	if firstFingerprint == "" {
		t.Error("fingerprint 应被填充")
	}

	// 状态：远端已存在 + 本地未变 → match=true
	code, body, _ = harness.do(client, http.MethodGet, "/api/sync/webdav/status", nil)
	if code != http.StatusOK || body["match"] != true || body["has_remote"] != true {
		t.Errorf("远端与本地一致时状态: %v", body)
	}

	// 修改远端（模拟别人在别处改过）
	remoteBytes.Store([]byte(`{"format":"v1","providers":[]}`))
	code, body, _ = harness.do(client, http.MethodGet, "/api/sync/webdav/status", nil)
	if code != http.StatusOK || body["match"] != false {
		t.Errorf("远端被改后 match=false: %v", body)
	}

	// 再 push：应被拒绝（409）
	code, body, raw = harness.do(client, http.MethodPost, "/api/sync/webdav/push", map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("冲突时 push 应 409，得到 %d（%s）", code, raw)
	}
	if body["reason"] != "remote_changed" {
		t.Errorf("reason = %v", body["reason"])
	}

	// force=true 覆盖
	remoteBytes.Store([]byte("stale-remote"))
	code, _, raw = harness.do(client, http.MethodPost, "/api/sync/webdav/push", map[string]any{"force": true})
	if code != http.StatusOK {
		t.Fatalf("force push 应 200，得到 %d（%s）", code, raw)
	}

	// Pull：远端与本地不同 → 409
	remoteBytes.Store([]byte("stale"))
	code, body, _ = harness.do(client, http.MethodPost, "/api/sync/webdav/pull", map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("冲突时 pull 应 409，得到 %d", code)
	}
	if body["reason"] != "local_changed" {
		t.Errorf("reason = %v", body["reason"])
	}

	// Pull force=true：用一个格式合法的远端文件覆盖本地
	remoteBytes.Store([]byte(`{"format":"agoramodel.providers/v1","providers":[
		{"id":"remote","name":"Remote","openai_base_url":"https://api.example.com/v1","api_key":"sk-remote-test","enabled":true,"allow_internal":true,"models_selected":["r1"]}
	]}`))
	code, body, raw = harness.do(client, http.MethodPost, "/api/sync/webdav/pull", map[string]any{"force": true})
	if code != http.StatusOK {
		t.Fatalf("force pull 应 200，得到 %d（%s）", code, raw)
	}
	if _, ok := body["imported"]; !ok {
		t.Errorf("响应应包含 imported: %v", body)
	}
}

// harnessWithPresets 创建一个带 presets Store 的新 harness（不复用原 harness）。
// 用于测试需要 opts.Presets != nil 的接口。
func harnessWithPresets(t *testing.T, pstore *presets.Store) (*httptest.Server, *harness) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agora-api-presets-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	st, err := store.Open(context.Background(), filepath.Join(dir, "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	master := make([]byte, crypto.KeySize)
	for i := range master {
		master[i] = byte(i + 1)
	}

	// 至少一个供应商让快照合法
	if _, err := st.SaveProvider(context.Background(), master, config.Provider{
		ID:            "seed",
		Name:          "seed",
		OpenAIBaseURL: "https://api.example.com/v1",
		APIKey:        "sk-seed",
		Models:        []string{"seed-model"},
		AllowInternal: true,
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := buildSnapshot(context.Background(), st, master)
	if err != nil {
		t.Fatal(err)
	}
	holder := config.NewHolder(snap)

	h := &harness{
		t: t, store: st, master: master, holder: holder,
		dataDir: dir,
	}
	mux := http.NewServeMux()
	apiSrv := New(Options{
		Store:  st,
		Master: master,
		Holder: holder,
		Reload: func(ctx context.Context) (*config.Snapshot, error) {
			return buildSnapshot(ctx, st, master)
		},
		Version:       "test-version",
		AdminPassword: "",
		LocalOnly:     true,
		Paths: DataPaths{
			DataDir:       dir,
			DBPath:        filepath.Join(dir, "agora.db"),
			MasterKeyPath: filepath.Join(dir, "master.key"),
		},
		Presets: pstore,
	})
	apiSrv.Register(mux)

	h.srv = httptest.NewServer(mux)
	h.apiServer = apiSrv
	return h.srv, h
}

// TestWebDAVFingerprintIsStable 覆盖「刷新状态时指纹不能变」的回归：
//
// exportProvidersBytes 里有 ExportedAt 字段，每次调用都会生成新的时间戳。
// 直接拿它算指纹会导致每次刷新都换指纹、与远端永远不匹配。
// exportProvidersFingerprintBytes 必须把 ExportedAt 抹平、保证同份配置算出相同 hash。
func TestWebDAVFingerprintIsStable(t *testing.T) {
	presetStore := presets.New(presets.Options{})
	_, harness := harnessWithPresets(t, presetStore)
	defer harness.srv.Close()

	if harness.apiServer == nil {
		t.Fatal("harnessWithPresets 未返回 apiServer")
	}
	ctx := context.Background()

	first, err := harness.apiServer.exportProvidersFingerprintBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 强制换 ExportedAt 的来源值：直接改包内变量，确保指纹生成路径里真的包含时间源。
	prevTime := timeNow
	timeNow = func() string { return "2099-12-31T23:59:59Z" }
	defer func() { timeNow = prevTime }()

	second, err := harness.apiServer.exportProvidersFingerprintBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("指纹必须稳定，但两次不一致：\n  first=%s\n  second=%s", first, second)
	}
}

// TestWebDAVTestConnection 覆盖「测试连接」接口：
//   - 未配置 → 400
//   - 远端可达 + 文件存在 → ok=true + 远端指纹 / 字节数
//   - 远端可达 + 文件不存在 → ok=true + has_remote=false
//   - 远端返回 401 → 502 + 中文凭证错误文案
//   - GET /test 不会写远端文件（确保「测试」真的是只读探测）
func TestWebDAVTestConnection(t *testing.T) {
	var remoteBytes atomic.Value // []byte
	remoteBytes.Store([]byte(nil))
	putCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b := remoteBytes.Load().([]byte)
			if b == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			remoteBytes.Store(body)
			putCount++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	presetStore := presets.New(presets.Options{})
	_, harness := harnessWithPresets(t, presetStore)
	defer harness.srv.Close()
	client := newClient(t)

	// 未配置：直接打 /test 应得到 400
	code, body, _ := harness.do(client, http.MethodGet, "/api/sync/webdav/test", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置时 GET /test 应 400，得到 %d", code)
	}
	if errMsg, _ := body["error"].(string); !contains(errMsg, "尚未配置") {
		t.Errorf("error 文案异常: %v", body)
	}

	// 配置 + 远端空 → 200，has_remote=false
	code, _, raw := harness.do(client, http.MethodPut, "/api/settings/webdav", map[string]any{
		"url": srv.URL + "/dav/", "username": "u", "password": "p",
	})
	if code != http.StatusOK {
		t.Fatalf("保存 webdav 配置：%d（%s）", code, raw)
	}

	code, body, raw = harness.do(client, http.MethodGet, "/api/sync/webdav/test", nil)
	if code != http.StatusOK {
		t.Fatalf("远端空时 GET /test 应 200，得到 %d（%s）", code, raw)
	}
	if body["ok"] != true || body["has_remote"] != false {
		t.Errorf("远端空响应异常: %v", body)
	}
	if putCount != 0 {
		t.Errorf("测试连接不应写远端，PUT 次数 = %d", putCount)
	}

	// 远端写入一些字节再测：ok=true + remote_fingerprint 非空
	remoteBytes.Store([]byte(`{"format":"agoramodel.providers/v1","providers":[]}`))
	code, body, _ = harness.do(client, http.MethodGet, "/api/sync/webdav/test", nil)
	if code != http.StatusOK {
		t.Fatalf("远端存在时 GET /test 应 200，得到 %d", code)
	}
	if body["ok"] != true || body["has_remote"] != true {
		t.Errorf("远端存在响应异常: %v", body)
	}
	if _, ok := body["remote_fingerprint"].(string); !ok {
		t.Errorf("缺少 remote_fingerprint: %v", body)
	}
	if _, ok := body["remote_bytes"].(float64); !ok {
		t.Errorf("缺少 remote_bytes: %v", body)
	}

	// 凭据错误 → 502 + 中文错误
	authFailSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer authFailSrv.Close()
	code, _, _ = harness.do(client, http.MethodPut, "/api/settings/webdav", map[string]any{
		"url": authFailSrv.URL + "/dav/", "username": "u", "password": "wrong",
	})
	if code != http.StatusOK {
		t.Fatalf("更新为错误凭证应成功")
	}
	code, body, _ = harness.do(client, http.MethodGet, "/api/sync/webdav/test", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("认证失败应 502，得到 %d", code)
	}
	if errMsg, _ := body["error"].(string); !contains(errMsg, "认证失败") {
		t.Errorf("错误文案异常: %v", body)
	}
}

func contains(haystack, needle string) bool {
	return needle != "" && strings.Index(haystack, needle) >= 0
}
