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

func TestResolvePicksLowestPriority(t *testing.T) {
	snap := snapshot(t,
		config.Provider{ID: "b", OpenAIBaseURL: "http://b/v1", Priority: 20, Models: []string{"shared"}},
		config.Provider{ID: "a", OpenAIBaseURL: "http://a/v1", Priority: 10, Models: []string{"shared"}},
	)
	got, err := Resolve(snap, config.ProtocolOpenAI, "shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "a" {
		t.Fatalf("应选中 priority 最小者 a，得到 %s", got.Provider.ID)
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
		config.Provider{ID: "a", OpenAIBaseURL: "http://a/v1", Priority: 1, Models: []string{"other"}},
		config.Provider{ID: "b", OpenAIBaseURL: "http://b/v1", Priority: 2, Models: []string{"wanted"}},
	)
	got, err := Resolve(snap, config.ProtocolOpenAI, "wanted")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider.ID != "b" {
		t.Fatalf("应跳过未声明该模型的供应商，得到 %s", got.Provider.ID)
	}
}

func TestResolveModelNotFound(t *testing.T) {
	snap := snapshot(t, config.Provider{ID: "a", OpenAIBaseURL: "http://a/v1", Models: []string{"m"}})
	_, err := Resolve(snap, config.ProtocolOpenAI, "missing")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("期望 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveProtocolNotConfigured(t *testing.T) {
	// 只配了 OpenAI 地址，Anthropic 入站应报协议未配置
	snap := snapshot(t, config.Provider{ID: "a", OpenAIBaseURL: "http://a/v1", Models: []string{"m"}})
	_, err := Resolve(snap, config.ProtocolAnthropic, "m")
	if !errors.Is(err, ErrProtocolNotConfigured) {
		t.Fatalf("期望 ErrProtocolNotConfigured，得到 %v", err)
	}
}

func TestResolveAnthropicEndpoint(t *testing.T) {
	snap := snapshot(t, config.Provider{
		ID:               "a",
		AnthropicBaseURL: "https://api.a.com",
		Models:           []string{"claude-x"},
	})
	got, err := Resolve(snap, config.ProtocolAnthropic, "claude-x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Upstream != "https://api.a.com/messages" {
		t.Errorf("Upstream = %q", got.Upstream)
	}
}

func TestResolveNilSnapshot(t *testing.T) {
	if _, err := Resolve(nil, config.ProtocolOpenAI, "m"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("空快照应返回 ErrModelNotFound，得到 %v", err)
	}
}

func TestResolveNamespaceForcesProvider(t *testing.T) {
	snap := snapshot(t,
		config.Provider{ID: "a", OpenAIBaseURL: "http://a/v1", Priority: 1, Models: []string{"shared", "only-a"}},
		config.Provider{ID: "b", OpenAIBaseURL: "http://b/v1", Priority: 2, Models: []string{"shared", "only-b"}},
	)
	// 裸名按 priority 命中 a
	bare, err := Resolve(snap, config.ProtocolOpenAI, "shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bare.Provider.ID != "a" {
		t.Fatalf("裸名应命中 priority 最小者，得到 %s", bare.Provider.ID)
	}

	// 命名空间强制走 b，并且转发用的 model 要去掉前缀
	forced, err := Resolve(snap, config.ProtocolOpenAI, "b/shared")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if forced.Provider.ID != "b" {
		t.Fatalf("命名空间应强制走 b，得到 %s", forced.Provider.ID)
	}
	if forced.Model != "shared" {
		t.Fatalf("转发用的 model 应为 shared，得到 %q", forced.Model)
	}
	if forced.Upstream != "http://b/v1/chat/completions" {
		t.Fatalf("Upstream = %q", forced.Upstream)
	}
}

func TestResolveNamespaceDoesNotBreakSlashModelNames(t *testing.T) {
	// meta-llama 不是已存在的 provider id，应整体当作模型名
	snap := snapshot(t, config.Provider{
		ID:            "p",
		OpenAIBaseURL: "http://p/v1",
		Models:        []string{"meta-llama/Llama-3-70B"},
	})
	got, err := Resolve(snap, config.ProtocolOpenAI, "meta-llama/Llama-3-70B")
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
		config.Provider{ID: "on", OpenAIBaseURL: "http://on/v1", Models: []string{"m"}},
		config.Provider{ID: "off", OpenAIBaseURL: "http://off/v1", Models: []string{"m"}, Enabled: &disabled},
	)

	if _, err := Resolve(snap, config.ProtocolOpenAI, "off/m"); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("停用供应商应返回 ErrProviderDisabled，得到 %v", err)
	}
	if _, err := Resolve(snap, config.ProtocolOpenAI, "on/nope"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("供应商未声明该模型应返回 ErrModelNotFound，得到 %v", err)
	}
	if _, err := Resolve(snap, config.ProtocolOpenAI, "on/"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("尾部斜杠应退回普通模型名查找，得到 %v", err)
	}
}

func TestResolveNamespaceProtocolNotConfigured(t *testing.T) {
	snap := snapshot(t, config.Provider{ID: "on", OpenAIBaseURL: "http://on/v1", Models: []string{"m"}})
	if _, err := Resolve(snap, config.ProtocolAnthropic, "on/m"); !errors.Is(err, ErrProtocolNotConfigured) {
		t.Fatalf("期望 ErrProtocolNotConfigured，得到 %v", err)
	}
}
