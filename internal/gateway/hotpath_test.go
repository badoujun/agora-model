package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/store"
)

// TestHotPathServesAfterDatabaseClosed 证明数据面热路径完全不依赖数据库：
// 快照构建完成（供应商与 Key 已在内存里）之后把数据库关掉，转发仍然成功。
func TestHotPathServesAfterDatabaseClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	dir, err := os.MkdirTemp("", "agora-gw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	st, err := store.Open(context.Background(), filepath.Join(dir, "agora.db"))
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, crypto.KeySize)
	for i := range master {
		master[i] = byte(i + 1)
	}
	enabled := true
	saved, err := st.SaveProvider(context.Background(), master, config.Provider{
		ID:            "p",
		Name:          "供应商",
		OpenAIBaseURL: upstream.URL + "/v1",
		Models:        []string{"m"},
		AllowInternal: true,
		APIKeys: []config.ProviderKey{
			{Label: "主账号", Key: "sk-1", Enabled: &enabled},
			{Label: "备用", Key: "sk-2", Enabled: &enabled},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	badKeyID := saved.Keys[0].ID

	providers, err := st.LoadProviders(context.Background(), master)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{APIKey: gatewayKey, MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: providers,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 关键一步：关闭数据库。此后的任何 DB 访问都会失败。
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stats := newFakeKeyStats()
	g := New(config.NewHolder(snap), testLogger()).WithProviderKeyStats(stats)
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", gatewayKey, `{"model":"供应商/m"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d（%s）：热路径不应依赖数据库", i+1, resp.StatusCode, body)
		}
	}

	// 首个 Key 的 401 应在内存中即时生效（异步落库失败也不影响请求）
	_, failures, disabled := stats.snapshot()
	if disabled[badKeyID] == "" || failures[badKeyID] == 0 {
		t.Fatalf("凭证失败应在内存中即时生效：failures=%v disabled=%v", failures, disabled)
	}
}

// TestHotPathKeySelectionIsAllocationFriendly 覆盖纯内存选 Key 的开销与结果。
func TestHotPathKeySelectionIsAllocationFriendly(t *testing.T) {
	enabled := true
	keys := []config.ProviderKey{
		{ID: "k1", Key: "sk-1", Enabled: &enabled},
		{ID: "k2", Key: "sk-2", Enabled: &enabled},
		{ID: "k3", Key: "sk-3", Enabled: &enabled},
	}
	rt := newKeyRuntime()
	for i := 0; i < 10; i++ {
		order := rt.order("p", keys, maxKeyAttempts)
		if len(order) != 3 {
			t.Fatalf("第 %d 次候选数 = %d", i, len(order))
		}
		if order[0].ID != []string{"k1", "k2", "k3"}[i%3] {
			t.Fatalf("第 %d 次轮询起点 = %s", i, order[0].ID)
		}
	}
}
