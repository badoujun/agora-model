package route

import (
	"errors"
	"testing"

	"agora-model/internal/config"
)

func snapshot(t *testing.T, providers ...config.Provider) *config.Snapshot {
	t.Helper()
	snap, err := config.NewSnapshot(config.File{
		Gateway:   config.Gateway{APIKey: "gw-test"},
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("构建快照失败: %v", err)
	}
	return snap
}

func provider(id, name, upstreamURL string, models ...string) config.Provider {
	return config.Provider{
		ID: id, Name: name, OpenAIBaseURL: upstreamURL + "/v1",
		APIKey: "sk-" + id, Models: models,
	}
}

func TestResolveRequiresNamespace(t *testing.T) {
	snap := snapshot(t,
		provider("a", "供应商A", "http://a", "shared"),
		provider("b", "供应商B", "http://b", "shared"),
	)

	// 裸名一律拒绝
	if _, err := Resolve(snap, "shared"); !errors.Is(err, ErrModelNotNamespaced) {
		t.Fatalf("裸模型名应返回 ErrModelNotNamespaced，得到 %v", err)
	}

	// 前缀不是供应商名（含斜杠的真实模型名）同样拒绝
	if _, err := Resolve(snap, "meta-llama/Llama-3-70B"); !errors.Is(err, ErrModelNotNamespaced) {
		t.Fatalf("前缀不是供应商名时应返回 ErrModelNotNamespaced，得到 %v", err)
	}
}

func TestResolveNamespacePicksNamedProvider(t *testing.T) {
	snap := snapshot(t,
		provider("b", "供应商B", "http://b", "shared"),
		provider("a", "供应商A", "http://a", "shared"),
	)
	got, err := Resolve(snap, "供应商B/shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "b" {
		t.Fatalf("应命中供应商B，得到 %s", got.Provider.ID)
	}
	if got.Upstream != "http://b/v1/chat/completions" {
		t.Errorf("Upstream = %q", got.Upstream)
	}
	if got.Model != "shared" {
		t.Errorf("Model = %q", got.Model)
	}
}

func TestResolveNamespaceSkipsProviderWithoutModel(t *testing.T) {
	snap := snapshot(t,
		provider("a", "A", "http://a", "other"),
		provider("b", "B", "http://b", "wanted"),
	)
	got, err := Resolve(snap, "B/wanted")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "b" {
		t.Fatalf("应命中供应商 B，得到 %s", got.Provider.ID)
	}
}

func TestResolveModelNotFound(t *testing.T) {
	snap := snapshot(t, provider("a", "A", "http://a", "m"))
	if _, err := Resolve(snap, "A/missing"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("期望 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveNilSnapshot(t *testing.T) {
	if _, err := Resolve(nil, "A/m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("空快照应返回 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveNamespaceErrors(t *testing.T) {
	disabled := false
	snap := snapshot(t,
		config.Provider{ID: "on", Name: "启用中", OpenAIBaseURL: "http://on/v1", APIKey: "sk-on", Models: []string{"m"}},
		config.Provider{ID: "off", Name: "已停用", OpenAIBaseURL: "http://off/v1", APIKey: "sk-off", Models: []string{"m"}, Enabled: &disabled},
	)

	if _, err := Resolve(snap, "已停用/m"); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("停用供应商应返回 ErrProviderDisabled，得到 %v", err)
	}
	if _, err := Resolve(snap, "启用中/nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("供应商未声明该模型应返回 ErrModelNotFound，得到 %v", err)
	}
	if _, err := Resolve(snap, "启用中/"); !errors.Is(err, ErrModelNotNamespaced) {
		t.Fatalf("缺少模型名应返回 ErrModelNotNamespaced，得到 %v", err)
	}
}

func TestResolveAliasRewritesToUpstreamModel(t *testing.T) {
	snap := snapshot(t, config.Provider{
		ID:            "p",
		Name:          "供应商A",
		OpenAIBaseURL: "http://a/v1",
		APIKey:        "sk-a",
		Models:        []string{"gpt-4o"},
		ModelAliases:  map[string]string{"gpt-4o": "我的模型"},
	})

	// 命名空间 + 别名
	got, err := Resolve(snap, "供应商A/我的模型")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Model != "gpt-4o" {
		t.Fatalf("转发给上游的 model 应为真实名 gpt-4o，得到 %q", got.Model)
	}

	// 裸别名同样不被接受
	if _, err := Resolve(snap, "我的模型"); !errors.Is(err, ErrModelNotNamespaced) {
		t.Fatalf("裸别名应返回 ErrModelNotNamespaced，得到 %v", err)
	}

	// 上游原名在配置了别名后不再对外可用
	if _, err := Resolve(snap, "供应商A/gpt-4o"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("配置别名后原名不应命中，得到 %v", err)
	}
}

// TestResolveSkipsProviderWithoutUsableKey 覆盖「Key 全部被自动停用」后的路由行为：
// 命名空间命中时给出明确的 ErrProviderNoKey。
func TestResolveSkipsProviderWithoutUsableKey(t *testing.T) {
	noneEnabled := false
	keyless := config.Provider{
		ID: "dead", Name: "无可用Key", OpenAIBaseURL: "http://dead/v1",
		Models:  []string{"shared", "only-dead"},
		APIKeys: []config.ProviderKey{{Key: "sk-dead", Enabled: &noneEnabled}},
	}
	healthy := provider("ok", "健康供应商", "http://ok", "shared")

	snap := snapshot(t, keyless, healthy)

	got, err := Resolve(snap, "健康供应商/shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "ok" {
		t.Fatalf("应命中健康供应商，得到 %s", got.Provider.ID)
	}

	if _, err := Resolve(snap, "无可用Key/only-dead"); !errors.Is(err, ErrProviderNoKey) {
		t.Fatalf("命名空间命中无可用 Key 的供应商应返回 ErrProviderNoKey，得到 %v", err)
	}
}
