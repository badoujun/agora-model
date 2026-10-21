package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agora-model/internal/config"
)

// fakeKeyStore 是 KeyStore 的内存替身：只有 gw-ok 能通过校验，且总是命中同一个 id。
type fakeKeyStore struct{ id string }

func (f fakeKeyStore) VerifyGatewayKey(_ context.Context, provided string) (string, bool, error) {
	if provided == "gw-ok" {
		return f.id, true, nil
	}
	return "", false, nil
}

func (f fakeKeyStore) TouchGatewayKey(context.Context, string) error { return nil }

// buildScopedGateway 构造一个带网关 Key 模型范围的测试服务。
func buildScopedGateway(t *testing.T, keyID string, scope config.ModelScope, providers ...config.Provider) *httptest.Server {
	t.Helper()
	snap, err := config.NewSnapshotWithScopes(config.File{
		Gateway:   config.Gateway{MaxBodyBytes: 1 << 20, SSEIdleSeconds: 30},
		Providers: providers,
	}, map[string]config.ModelScope{keyID: scope})
	if err != nil {
		t.Fatalf("NewSnapshotWithScopes: %v", err)
	}
	g := New(config.NewHolder(snap), testLogger()).WithKeyStore(fakeKeyStore{id: keyID})
	mux := http.NewServeMux()
	g.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestScopedRouteRejectsModelOutsideScope 覆盖「模型存在但不在当前 Key 范围」的场景：
// 必须返回 403 + model_not_allowed + permission_error，而不是 404 或 500。
func TestScopedRouteRejectsModelOutsideScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer upstream.Close()

	const keyID = "gk_scoped"
	srv := buildScopedGateway(t, keyID, config.ModelScope{"a": {"allowed": {}}},
		providerFor("a", upstream.URL, "allowed", "denied"))

	t.Run("范围内模型放行", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "gw-ok",
			`{"model":"allowed"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d（%s），期望 200", resp.StatusCode, body)
		}
	})

	t.Run("范围外模型返回 403 permission_error", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "gw-ok",
			`{"model":"denied"}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("状态码 = %d（%s），期望 403", resp.StatusCode, body)
		}
		errObj, _ := decodeJSON(t, body)["error"].(map[string]any)
		if errObj["code"] != "model_not_allowed" {
			t.Errorf("error.code = %v，期望 model_not_allowed", errObj["code"])
		}
		if errObj["type"] != "permission_error" {
			t.Errorf("error.type = %v，期望 permission_error", errObj["type"])
		}
	})

	t.Run("命名空间越界同样返回 403", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "gw-ok",
			`{"model":"a/denied"}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("状态码 = %d（%s），期望 403", resp.StatusCode, body)
		}
	})

	t.Run("不存在的模型仍是 404", func(t *testing.T) {
		resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "gw-ok",
			`{"model":"missing"}`, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("状态码 = %d（%s），期望 404", resp.StatusCode, body)
		}
	})
}

// TestModelsListHonoursScope 覆盖 /v1/models：范围外的模型不出现在清单里。
func TestModelsListHonoursScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	srv := buildScopedGateway(t, "gk_scoped", config.ModelScope{"a": {"allowed": {}}},
		providerFor("a", upstream.URL, "allowed", "denied"))

	resp, body := doRequest(t, http.MethodGet, srv.URL+"/v1/models", "gw-ok", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", resp.StatusCode, body)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析响应失败: %v（%s）", err, body)
	}
	ids := map[string]bool{}
	for _, m := range payload.Data {
		ids[m.ID] = true
	}
	if !ids["allowed"] || !ids["a/allowed"] {
		t.Errorf("范围内的模型应在清单里: %v", ids)
	}
	if ids["denied"] || ids["a/denied"] {
		t.Errorf("范围外的模型不应出现在清单里: %v", ids)
	}
}

// TestUnscopedKeySeesEverything 覆盖「范围为空 = 不限制」：升级后的既有 Key 行为不变。
func TestUnscopedKeySeesEverything(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer upstream.Close()

	srv := buildScopedGateway(t, "gk_free", nil, providerFor("a", upstream.URL, "allowed", "denied"))
	resp, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "gw-ok",
		`{"model":"denied"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未配置范围的 Key 应可调用全部模型：状态码 = %d（%s）", resp.StatusCode, body)
	}
}
