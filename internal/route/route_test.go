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
