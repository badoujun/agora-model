package config

import (
	"errors"
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
			ModelAliases:  map[string]string{"m1": " 我的模型 ", "m2": "   ", "ghost": "x"},
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
	if len(p.ModelAliases) != 1 || p.ModelAliases["m1"] != "我的模型" {
		t.Errorf("别名应去空白并丢弃空值: %#v", p.ModelAliases)
	}
	if !p.IsEnabled() {
		t.Error("缺省应视为启用")
	}
	if p.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, 期望 %d", p.TimeoutSeconds, DefaultTimeoutSeconds)
	}
	if got := p.Endpoint(); got != "http://upstream.example/v1/chat/completions" {
		t.Errorf("endpoint = %q", got)
	}
	if got := p.ModelsEndpoint(); got != "http://upstream.example/v1/models" {
		t.Errorf("models endpoint = %q", got)
	}
}

func TestExposedAndRealModel(t *testing.T) {
	p := Provider{
		ID:            "p",
		OpenAIBaseURL: "http://x/v1",
		Models:        []string{"gpt-4o", "gpt-4o-mini"},
		ModelAliases:  map[string]string{"gpt-4o": "我的模型"},
	}
	if got := p.ExposedModel("gpt-4o"); got != "我的模型" {
		t.Errorf("ExposedModel = %q", got)
	}
	if got := p.ExposedModels(); len(got) != 2 || got[0] != "我的模型" || got[1] != "gpt-4o-mini" {
		t.Errorf("ExposedModels = %#v", got)
	}
	real, ok := p.RealModel("我的模型")
	if !ok || real != "gpt-4o" {
		t.Errorf("RealModel(我的模型) = %q ok=%v", real, ok)
	}
	if _, ok := p.RealModel("gpt-4o"); ok {
		t.Error("配置了别名后，上游原名不应再对外可用")
	}
	if !p.KnowsModel("gpt-4o-mini") {
		t.Error("未配置别名的模型应仍可用原名访问")
	}
	if p.KnowsModel("nope") {
		t.Error("未勾选的模型不应被声明")
	}
}

func TestValidateModelSelectionRejectsAliasCollision(t *testing.T) {
	p := Provider{
		ID:            "p",
		Name:          "供应商A",
		OpenAIBaseURL: "http://x/v1",
		Models:        []string{"m1", "m2"},
		ModelAliases:  map[string]string{"m1": "alias", "m2": "ALIAS"},
	}
	err := p.ValidateModelSelection()
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("别名冲突应被拒绝（忽略大小写），得到 %v", err)
	}
}

func TestValidateModelSelectionAllowsDistinctAliases(t *testing.T) {
	p := Provider{
		ID:            "p",
		OpenAIBaseURL: "http://x/v1",
		Models:        []string{"m1", "m2"},
		ModelAliases:  map[string]string{"m1": "别名1", "m2": "别名2"},
	}
	if err := p.ValidateModelSelection(); err != nil {
		t.Fatalf("合法别名不应报错: %v", err)
	}
}

func TestValidateStaticAPIKey(t *testing.T) {
	if err := (File{Gateway: Gateway{APIKey: "   "}}).ValidateStaticAPIKey(); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("空白 Key 应返回 ErrNoAPIKey，得到 %v", err)
	}
	if err := (File{Gateway: Gateway{APIKey: "gw-x"}}).ValidateStaticAPIKey(); err != nil {
		t.Fatalf("有效 Key 不应报错: %v", err)
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := []struct {
		name string
		file File
		want string
	}{
		{
			name: "没有启用的供应商",
			file: File{Gateway: Gateway{APIKey: "k"}},
			want: ErrNoProviders.Error(),
		},
		{
			name: "供应商 id 重复",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "dup", Name: "a", OpenAIBaseURL: "http://x/v1"},
				{ID: "dup", Name: "b", OpenAIBaseURL: "http://y/v1"},
			}},
			want: "id 重复",
		},
		{
			name: "供应商名称重复",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "a", Name: "同名", OpenAIBaseURL: "http://x/v1"},
				{ID: "b", Name: " 同名 ", OpenAIBaseURL: "http://y/v1"},
			}},
			want: "供应商名称重复",
		},
		{
			name: "供应商未配置上游地址",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{{ID: "a", Name: "a"}}},
			want: "必须配置 openai_base_url",
		},
		{
			name: "URL scheme 非法",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "a", Name: "a", OpenAIBaseURL: "ftp://x/v1"},
			}},
			want: "仅支持 http/https",
		},
		{
			name: "别名冲突",
			file: File{Gateway: Gateway{APIKey: "k"}, Providers: []Provider{
				{ID: "a", Name: "a", OpenAIBaseURL: "http://x/v1",
					Models: []string{"m1", "m2"}, ModelAliases: map[string]string{"m2": "m1"}},
			}},
			want: "重复",
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
		ID:                     "p",
		OpenAIBaseURL:          "http://x/v1",
		OpenAIEndpointOverride: "http://x/custom/completions",
	}
	if got := p.Endpoint(); got != "http://x/custom/completions" {
		t.Errorf("override 应优先: %q", got)
	}
	if got := p.ModelsEndpoint(); got != "" {
		t.Errorf("端点覆盖时无法推导 /models: %q", got)
	}
}

func TestSnapshotOrderingByNameAndDisabled(t *testing.T) {
	f := File{
		Gateway: Gateway{APIKey: "k"},
		Providers: []Provider{
			{ID: "late", Name: "Beta", OpenAIBaseURL: "http://x/v1", Models: []string{"m"}},
			{ID: "off", Name: "Alpha", OpenAIBaseURL: "http://x/v1", Models: []string{"m"}, Enabled: boolPtr(false)},
			{ID: "early", Name: "Delta", OpenAIBaseURL: "http://x/v1", Models: []string{"m"}},
		},
	}
	snap, err := NewSnapshot(f)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	ordered := snap.Providers()
	if len(ordered) != 2 {
		t.Fatalf("停用的供应商不应进入路由列表，得到 %d 项", len(ordered))
	}
	if ordered[0].ID != "late" || ordered[1].ID != "early" {
		t.Fatalf("应按名称升序（Beta < Delta）: %s, %s", ordered[0].ID, ordered[1].ID)
	}
	if !snap.HasProvider("off") {
		t.Error("HasProvider 应包含已停用的供应商")
	}
	if _, ok := snap.Provider("early"); !ok {
		t.Error("Provider 查询失败")
	}
	byName, ok := snap.ProviderByName(" ALPHA ")
	if !ok || byName.ID != "off" {
		t.Errorf("ProviderByName 应忽略空白与大小写、并包含停用项，得到 %v ok=%v", byName, ok)
	}
}

func TestLoadFileAndHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "gateway": {"api_key": "gw-1", "port": 1234},
	  "providers": [{"id": "p1", "name": "供应商 1", "openai_base_url": "http://x/v1",
	                 "models": ["m"], "model_aliases": {"m": "别名M"}}]
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
	if f.Providers[0].ModelAliases["m"] != "别名M" {
		t.Errorf("模型别名未解析: %#v", f.Providers[0].ModelAliases)
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

// 回归：全新安装时数据库为空是**合法**初始状态——用户必须先能启动网关，
// 才能通过 Web UI 添加第一个供应商。此前 NewSnapshot 复用了 Normalize 的严格校验，
// 导致首次启动直接失败并提示「等待 Web UI 添加供应商」，而 Web UI 恰恰需要网关先跑起来。
func TestAllowEmptyProvidersForRuntimeSnapshot(t *testing.T) {
	empty := File{}

	// 引导配置（用户显式提供）仍然严格
	if _, err := empty.Normalize(); !errors.Is(err, ErrNoProviders) {
		t.Fatalf("Normalize 应拒绝空供应商，得到 %v", err)
	}

	// 运行时路径允许空
	normalized, err := empty.NormalizeAllowEmpty()
	if err != nil {
		t.Fatalf("NormalizeAllowEmpty 不应拒绝空供应商: %v", err)
	}
	if normalized.Gateway.Listen != DefaultListen || normalized.Gateway.Port != DefaultPort {
		t.Fatalf("默认值未补齐: %+v", normalized.Gateway)
	}

	snap, err := NewSnapshot(empty)
	if err != nil {
		t.Fatalf("NewSnapshot 不应拒绝空供应商: %v", err)
	}
	if got := len(snap.Providers()); got != 0 {
		t.Fatalf("空快照的启用供应商数 = %d，期望 0", got)
	}
	if snap.Gateway().Addr() != "127.0.0.1:9090" {
		t.Fatalf("默认监听地址未补齐: %s", snap.Gateway().Addr())
	}
}

func TestNormalizeAllowEmptyStillValidatesOtherFields(t *testing.T) {
	// 「允许空」只针对供应商数量，其它校验一个都不能松
	if _, err := (File{Providers: []Provider{{ID: "a", Name: "a"}}}).NormalizeAllowEmpty(); err == nil {
		t.Fatal("缺上游地址仍应报错")
	}
	if _, err := (File{Providers: []Provider{
		{ID: "dup", Name: "a", OpenAIBaseURL: "http://x/v1"},
		{ID: "dup", Name: "b", OpenAIBaseURL: "http://y/v1"},
	}}).NormalizeAllowEmpty(); err == nil {
		t.Fatal("id 重复仍应报错")
	}

	// 全部停用：严格模式拒绝，宽松模式放行（用户可能先停用、稍后再启用）
	disabled := File{Providers: []Provider{
		{ID: "a", Name: "a", OpenAIBaseURL: "http://x/v1", Enabled: boolPtr(false)},
	}}
	if _, err := disabled.Normalize(); !errors.Is(err, ErrNoProviders) {
		t.Fatalf("Normalize 应拒绝全部停用的供应商，得到 %v", err)
	}
	if _, err := disabled.NormalizeAllowEmpty(); err != nil {
		t.Fatalf("NormalizeAllowEmpty 应放行全部停用的供应商: %v", err)
	}
}
