package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDefaults(t *testing.T) {
	f := File{
		Gateway: Gateway{APIKey: "  gw-x  "},
		Providers: []Provider{{
			ID:            " a ",
			OpenAIBaseURL: "http://upstream.example/v1/",
			Models:        []string{"m1", " m1 ", "", "m2"},
		}},
	}
	out, err := f.Normalize()
	if err != nil {
		t.Fatalf("Normalize 失败: %v", err)
	}

	if out.Gateway.Listen != DefaultListen {
		t.Errorf("Listen = %q, 期望 %q", out.Gateway.Listen, DefaultListen)
	}
	if out.Gateway.Port != DefaultPort {
		t.Errorf("Port = %d, 期望 %d", out.Gateway.Port, DefaultPort)
	}
	if out.Gateway.SSEIdleSeconds != DefaultSSEIdleSeconds {
		t.Errorf("SSEIdleSeconds = %d, 期望 %d", out.Gateway.SSEIdleSeconds, DefaultSSEIdleSeconds)
	}
	if out.Gateway.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, 期望 %d", out.Gateway.MaxBodyBytes, DefaultMaxBodyBytes)
	}
	if out.Gateway.APIKey != "  gw-x  " {
		t.Errorf("APIKey 不应被裁剪空白: %q", out.Gateway.APIKey)
	}

	p := out.Providers[0]
	if p.ID != "a" {
		t.Errorf("ID = %q, 期望 a", p.ID)
	}
	if p.Name != "a" {
		t.Errorf("Name 应回落为 ID，得到 %q", p.Name)
	}
	if p.OpenAIBaseURL != "http://upstream.example/v1" {
		t.Errorf("base URL 尾部斜杠应被去掉: %q", p.OpenAIBaseURL)
	}
	if len(p.Models) != 2 || p.Models[0] != "m1" || p.Models[1] != "m2" {
		t.Errorf("模型列表应去空白去重: %#v", p.Models)
	}
	if !p.IsEnabled() {
		t.Error("缺省应视为启用")
	}
	if p.Priority != DefaultPriority {
		t.Errorf("Priority = %d, 期望 %d", p.Priority, DefaultPriority)
	}
	if p.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, 期望 %d", p.TimeoutSeconds, DefaultTimeoutSeconds)
	}
	if got := p.Endpoint(ProtocolOpenAI); got != "http://upstream.example/v1/chat/completions" {
		t.Errorf("OpenAI endpoint = %q", got)
	}
	if got := p.Endpoint(ProtocolAnthropic); got != "" {
		t.Errorf("未配置的 Anthropic endpoint 应为空, 得到 %q", got)
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := []struct {
		name string
		file File
		want string
	}{
		{
			name: "缺少网关 Key",
			file: File{Providers: []Provider{{ID: "a", OpenAIBaseURL: "http://x/v1"}}},
			want: ErrNoAPIKey.Error(),
		},
		{
			name: "没有启用的供应商",
			file: File{Gateway: Gateway{APIKey: "k"}},
			want: ErrNoProviders.Error(),
		},
		{
			name: "供应商 id 重复",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "dup", OpenAIBaseURL: "http://x/v1"},
				{ID: "dup", OpenAIBaseURL: "http://y/v1"},
			}},
			want: "id 重复",
		},
		{
			name: "供应商未配置任何协议地址",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{{ID: "a"}}},
			want: "至少需要配置一个协议地址",
		},
		{
			name: "URL scheme 非法",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "a", OpenAIBaseURL: "ftp://x/v1"},
			}},
			want: "仅支持 http/https",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.file.Normalize()
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 = %q, 期望包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestEndpointOverrideTakesPrecedence(t *testing.T) {
	p := Provider{
		ID:                        "p",
		OpenAIBaseURL:             "http://x/v1",
		OpenAIEndpointOverride:    "http://x/custom/completions",
		AnthropicBaseURL:          "http://x",
		AnthropicEndpointOverride: "http://x/custom/messages",
	}
	if got := p.Endpoint(ProtocolOpenAI); got != "http://x/custom/completions" {
		t.Errorf("override 应优先: %q", got)
	}
	if got := p.Endpoint(ProtocolAnthropic); got != "http://x/custom/messages" {
		t.Errorf("override 应优先: %q", got)
	}
}

func TestSnapshotOrderingAndDisabled(t *testing.T) {
	f := File{
		Gateway: Gateway{APIKey: "k"},
		Providers: []Provider{
			{ID: "late", OpenAIBaseURL: "http://x/v1", Priority: 50, Models: []string{"m"}},
			{ID: "off", OpenAIBaseURL: "http://x/v1", Priority: 1, Models: []string{"m"}, Enabled: boolPtr(false)},
			{ID: "early", OpenAIBaseURL: "http://x/v1", Priority: 5, Models: []string{"m"}},
		},
	}
	snap, err := NewSnapshot(f)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	ordered := snap.ProvidersByPriority()
	if len(ordered) != 2 {
		t.Fatalf("停用的供应商不应进入路由列表，得到 %d 项", len(ordered))
	}
	if ordered[0].ID != "early" || ordered[1].ID != "late" {
		t.Fatalf("应按 priority 升序: %s, %s", ordered[0].ID, ordered[1].ID)
	}
	if !snap.HasProvider("off") {
		t.Error("HasProvider 应包含已停用的供应商（供 Phase 3 命名空间判定）")
	}
	if _, ok := snap.Provider("early"); !ok {
		t.Error("Provider 查询失败")
	}
}

func TestLoadFileAndHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "gateway": {"api_key": "gw-1", "port": 1234},
	  "providers": [{"id": "p1", "openai_base_url": "http://x/v1", "models": ["m"]}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if f.Gateway.Port != 1234 {
		t.Errorf("Port = %d", f.Gateway.Port)
	}

	snap, err := NewSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	holder := NewHolder(snap)
	if holder.Get() == nil {
		t.Fatal("Holder.Get 不应为 nil")
	}

	// 原子替换后读到新快照
	f.Gateway.Port = 4321
	next, err := NewSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	holder.Store(next)
	if got := holder.Get().Gateway().Port; got != 4321 {
		t.Errorf("替换后 Port = %d, 期望 4321", got)
	}
}

func TestLoadFileMissing(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("文件不存在时应报错")
	}
}

func boolPtr(b bool) *bool { return &b }
