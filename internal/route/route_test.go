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
	return config.Provider{ID: id, Name: name, OpenAIBaseURL: upstreamURL + "/v1", Models: models}
}

func TestResolvePicksFirstProviderByName(t *testing.T) {
	snap := snapshot(t,
		provider("b", "供应商B", "http://b", "shared"),
		provider("a", "供应商A", "http://a", "shared"),
	)
	got, err := Resolve(snap, "shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// 名称排序：A 在前
	if got.Provider.ID != "a" {
		t.Fatalf("应选中名称最小者 a，得到 %s", got.Provider.ID)
	}
	if got.Upstream != "http://a/v1/chat/completions" {
		t.Errorf("Upstream = %q", got.Upstream)
	}
	if got.Model != "shared" {
		t.Errorf("Model = %q", got.Model)
	}
}

func TestResolveSkipsProviderWithoutModel(t *testing.T) {
	snap := snapshot(t,
		provider("a", "A", "http://a", "other"),
		provider("b", "B", "http://b", "wanted"),
	)
	got, err := Resolve(snap, "wanted")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "b" {
		t.Fatalf("应跳过未勾选该模型的供应商，得到 %s", got.Provider.ID)
	}
}

func TestResolveModelNotFound(t *testing.T) {
	snap := snapshot(t, provider("a", "A", "http://a", "m"))
	if _, err := Resolve(snap, "missing"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("期望 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveNilSnapshot(t *testing.T) {
	if _, err := Resolve(nil, "m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("空快照应返回 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveNamespaceForcesProvider(t *testing.T) {
	snap := snapshot(t,
		provider("a", "供应商A", "http://a", "shared", "only-a"),
		provider("b", "供应商B", "http://b", "shared", "only-b"),
	)
	// 裸名按名称命中 A
	bare, err := Resolve(snap, "shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bare.Provider.ID != "a" {
		t.Fatalf("裸名应命中名称最小者，得到 %s", bare.Provider.ID)
	}

	// 命名空间（供应商名/模型名）强制走 B
	forced, err := Resolve(snap, "供应商B/shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if forced.Provider.ID != "b" {
		t.Fatalf("命名空间应强制走 供应商B，得到 %s", forced.Provider.ID)
	}
	if forced.Model != "shared" {
		t.Fatalf("转发用的 model 应为 shared，得到 %q", forced.Model)
	}
	if forced.Upstream != "http://b/v1/chat/completions" {
		t.Fatalf("Upstream = %q", forced.Upstream)
	}
}

func TestResolveNamespaceDoesNotBreakSlashModelNames(t *testing.T) {
	// meta-llama 不是已存在的供应商名称，应整体当作模型名
	snap := snapshot(t, config.Provider{
		ID:            "p",
		Name:          "p",
		OpenAIBaseURL: "http://p/v1",
		Models:        []string{"meta-llama/Llama-3-70B"},
	})
	got, err := Resolve(snap, "meta-llama/Llama-3-70B")
	if err != nil {
		t.Fatalf("含斜杠的真实模型名不应被误判为命名空间: %v", err)
	}
	if got.Provider.ID != "p" || got.Model != "meta-llama/Llama-3-70B" {
		t.Fatalf("解析结果异常: provider=%s model=%q", got.Provider.ID, got.Model)
	}
}

func TestResolveNamespaceErrors(t *testing.T) {
	disabled := false
	snap := snapshot(t,
		config.Provider{ID: "on", Name: "启用中", OpenAIBaseURL: "http://on/v1", Models: []string{"m"}},
		config.Provider{ID: "off", Name: "已停用", OpenAIBaseURL: "http://off/v1", Models: []string{"m"}, Enabled: &disabled},
	)

	if _, err := Resolve(snap, "已停用/m"); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("停用供应商应返回 ErrProviderDisabled，得到 %v", err)
	}
	if _, err := Resolve(snap, "启用中/nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("供应商未声明该模型应返回 ErrModelNotFound，得到 %v", err)
	}
	if _, err := Resolve(snap, "启用中/"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("尾部斜杠应退回普通模型名查找，得到 %v", err)
	}
}

func TestResolveAliasRewritesToUpstreamModel(t *testing.T) {
	snap := snapshot(t, config.Provider{
		ID:            "p",
		Name:          "供应商A",
		OpenAIBaseURL: "http://a/v1",
		Models:        []string{"gpt-4o"},
		ModelAliases:  map[string]string{"gpt-4o": "我的模型"},
	})

	// 别名即对外模型名
	got, err := Resolve(snap, "我的模型")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Model != "gpt-4o" {
		t.Fatalf("转发给上游的 model 应为真实名 gpt-4o，得到 %q", got.Model)
	}

	// 命名空间 + 别名
	ns, err := Resolve(snap, "供应商A/我的模型")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ns.Model != "gpt-4o" {
		t.Fatalf("命名空间下转发用的 model 应为 gpt-4o，得到 %q", ns.Model)
	}

	// 上游原名在配置了别名后不再对外可用
	if _, err := Resolve(snap, "gpt-4o"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("配置别名后原名不应命中，得到 %v", err)
	}
}
