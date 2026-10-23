package config

import "sort"

// ModelScope 约束一个网关 Key 能使用的「供应商 × 上游模型」集合。
//
// 键是供应商 id，值是该供应商下允许调用的**上游模型名**（不是对外别名）：
// 别名随时可改，上游名才是稳定的路由目标。空集合表示不限制——这是默认值，
// 保持升级前的行为（所有网关 Key 都可调用全部模型）。
type ModelScope map[string]map[string]struct{}

// IsUnrestricted 报告该范围是否不限制任何模型。
func (s ModelScope) IsUnrestricted() bool { return len(s) == 0 }

// Allows 报告该范围是否允许调用某供应商的某个上游模型。
//
// 空范围一律放行；非空范围必须精确命中 (providerID, upstreamModel)。
func (s ModelScope) Allows(providerID, upstreamModel string) bool {
	if len(s) == 0 {
		return true
	}
	models, ok := s[providerID]
	if !ok {
		return false
	}
	_, ok = models[upstreamModel]
	return ok
}

// AllowsExposed 报告是否允许调用某供应商对外暴露的模型名（原名或别名）。
//
// 供应商未声明该模型时返回 false——调用方应先用 RealModel 判断存在性。
func (s ModelScope) AllowsExposed(p *Provider, exposed string) bool {
	if p == nil {
		return false
	}
	if len(s) == 0 {
		return true
	}
	real, known := p.RealModel(exposed)
	if !known {
		return false
	}
	return s.Allows(p.ID, real)
}

// Providers 返回范围内包含的供应商 id 集合（副本）。
func (s ModelScope) Providers() []string {
	out := make([]string, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	return out
}

// Clone 返回范围的深拷贝（快照持有它，避免与调用方共享底层 map）。
func (s ModelScope) Clone() ModelScope {
	if s == nil {
		return nil
	}
	out := make(ModelScope, len(s))
	for id, models := range s {
		inner := make(map[string]struct{}, len(models))
		for m := range models {
			inner[m] = struct{}{}
		}
		out[id] = inner
	}
	return out
}

// ScopeRef 是一个「供应商 × 模型」引用，用于报告失效的范围条目。
type ScopeRef struct {
	ProviderID string
	Model      string
}

// UnknownScopeRefs 返回范围里已不再生效的条目：供应商不存在、或供应商当前
// 未勾选该上游模型（含供应商已停用的情况——停用的供应商不参与路由）。
//
// 控制面用它拒绝保存会静默失效的范围（例如用户删了模型却把范围留着），
// 避免出现「界面上勾着、实际永远命中不了」的困惑配置。返回顺序按
// provider_id、model 排序，便于稳定报错。
func (s *Snapshot) UnknownScopeRefs(scope ModelScope) []ScopeRef {
	if s == nil || len(scope) == 0 {
		return nil
	}
	out := make([]ScopeRef, 0, 4)
	for providerID, models := range scope {
		p, ok := s.byID[providerID]
		for model := range models {
			if !ok || !p.HasUpstreamModel(model) {
				out = append(out, ScopeRef{ProviderID: providerID, Model: model})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].Model < out[j].Model
	})
	return out
}
