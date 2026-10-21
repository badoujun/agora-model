package config

import "testing"

// TestUnknownScopeRefs 覆盖网关 Key 模型范围的失效条目识别：
// 供应商不存在、模型未被勾选都会被视为失效；别名改动不影响判定。
func TestUnknownScopeRefs(t *testing.T) {
	disabled := false
	snap, err := NewSnapshot(File{
		Providers: []Provider{
			{ID: "p1", Name: "供应商A", OpenAIBaseURL: "http://a/v1", APIKey: "sk-a",
				Models: []string{"gpt-4o"}, ModelAliases: map[string]string{"gpt-4o": "我的模型"}},
			{ID: "p2", Name: "已停用", OpenAIBaseURL: "http://b/v1", APIKey: "sk-b",
				Models: []string{"claude"}, Enabled: &disabled},
		},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	// 空范围：没有失效项
	if refs := snap.UnknownScopeRefs(nil); len(refs) != 0 {
		t.Fatalf("空范围应为空，得到 %v", refs)
	}

	// 上游名有效（即便配了别名，范围内存的仍是上游名）
	refs := snap.UnknownScopeRefs(ModelScope{"p1": {"gpt-4o": {}}})
	if len(refs) != 0 {
		t.Fatalf("有效范围不应报失效，得到 %v", refs)
	}

	// 写成别名的范围应被视为失效（范围只认上游名）
	refs = snap.UnknownScopeRefs(ModelScope{"p1": {"我的模型": {}}})
	if len(refs) != 1 || refs[0].Model != "我的模型" {
		t.Fatalf("别名不应被视为有效上游模型，得到 %v", refs)
	}

	// 供应商不存在
	refs = snap.UnknownScopeRefs(ModelScope{"nope": {"m": {}}})
	if len(refs) != 1 || refs[0].ProviderID != "nope" {
		t.Fatalf("不存在的供应商应报失效，得到 %v", refs)
	}

	// 结果按 provider_id / model 稳定排序
	refs = snap.UnknownScopeRefs(ModelScope{
		"p9": {"b": {}, "a": {}},
		"p1": {"zzz": {}},
	})
	if len(refs) != 3 {
		t.Fatalf("应有 3 个失效项，得到 %v", refs)
	}
	if refs[0].ProviderID != "p1" || refs[1].Model != "a" || refs[2].Model != "b" {
		t.Errorf("排序不稳定: %v", refs)
	}
}

// TestHasUpstreamModel 覆盖上游名与对外名的区分。
func TestHasUpstreamModel(t *testing.T) {
	p := Provider{Models: []string{"gpt-4o"}, ModelAliases: map[string]string{"gpt-4o": "别名"}}
	p.CleanSelection()

	if !p.HasUpstreamModel("gpt-4o") {
		t.Error("应识别上游模型名")
	}
	if p.HasUpstreamModel("别名") {
		t.Error("不应把对外别名当作上游模型名")
	}
	if p.HasUpstreamModel("") || p.HasUpstreamModel("missing") {
		t.Error("空串与不存在的模型应为 false")
	}
	if !p.KnowsModel("别名") {
		t.Error("KnowsModel 应接受对外别名")
	}
}
