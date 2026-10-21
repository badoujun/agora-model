package route

import (
	"errors"
	"fmt"
	"strings"

	"agora-model/internal/config"
)

var (
	// ErrModelNotFound 模型未被任何启用的供应商声明。
	ErrModelNotFound = errors.New("模型未被任何启用的供应商声明")
	// ErrProviderDisabled 命名空间命中的供应商已停用。
	ErrProviderDisabled = errors.New("供应商已停用")
	// ErrProviderNoKey 命名空间命中的供应商没有可用的 API Key
	// （全部停用，或被网关因凭证类失败自动停用）。
	ErrProviderNoKey = errors.New("供应商没有可用的 API Key")
	// ErrModelNotAllowed 请求的模型存在，但不在当前网关 Key 的模型范围内。
	//
	// 与 ErrModelNotFound 区分开：前者是「配了但你的 Key 不能用」（403），
	// 后者是「根本没配」（404）。
	ErrModelNotAllowed = errors.New("当前网关 Key 不允许调用该模型")
)

// Result 是一次成功的路由决策。
type Result struct {
	Provider *config.Provider
	// Model 是实际要转发给上游的模型名（别名与命名空间前缀都在此剥离完毕）。
	Model string
	// Upstream 是完整的上游 URL。
	Upstream string
}

// Resolve 依据请求中的模型名选择供应商与上游地址（不施加模型范围约束）。
//
// 规则：
//  1. 命名空间路由：模型名形如 供应商名/模型名 且第一段命中**已存在的供应商名称**时，
//     强制走该供应商（转发前把 model 改写为该供应商声明的真实上游模型名）；
//  2. 否则按供应商名称升序遍历启用的供应商，取第一个对外提供该模型的；
//  3. 供应商内配置了别名时，别名即对外模型名，转发时替换回真实模型名。
//
// 斜杠歧义保护：只有当斜杠前的片段确实是已存在的供应商名称时才按命名空间解析，
// 因此 meta-llama/Llama-3-70B 这类真实模型名不会被误判。
func Resolve(snap *config.Snapshot, model string) (Result, error) {
	return ResolveScoped(snap, model, nil)
}

// ResolveScoped 在 Resolve 的基础上施加「当前网关 Key 的模型范围」约束。
//
// scope 为空（nil 或空集合）表示不限制，行为与 Resolve 完全一致。
//
// 范围约束**参与寻址**，而不是先选中再拒绝——否则裸模型名场景下网关会先
// 选中一个范围外的供应商、再把请求打回 403，用户无法通过它访问任何供应商：
//   - 裸模型名：只在范围内的供应商里选默认目标；
//   - 命名空间 供应商/模型：命中但越界时返回 ErrModelNotAllowed。
func ResolveScoped(snap *config.Snapshot, model string, scope config.ModelScope) (Result, error) {
	if snap == nil {
		return Result{}, ErrModelNotFound
	}

	if idx := strings.Index(model, "/"); idx > 0 && idx < len(model)-1 {
		name, rest := model[:idx], model[idx+1:]
		if p, ok := snap.ProviderByName(name); ok {
			if !p.IsEnabled() {
				return Result{}, fmt.Errorf("%w：%s", ErrProviderDisabled, p.Name)
			}
			if !p.HasUsableKey() {
				return Result{}, fmt.Errorf("%w：%s（可能已全部被自动停用，请在供应商页重新启用）", ErrProviderNoKey, p.Name)
			}
			real, known := p.RealModel(rest)
			if !known {
				return Result{}, fmt.Errorf("%w：供应商 %s 未提供模型 %q", ErrModelNotFound, p.Name, rest)
			}
			if !scope.Allows(p.ID, real) {
				return Result{}, fmt.Errorf("%w：供应商 %s 的模型 %q", ErrModelNotAllowed, p.Name, rest)
			}
			if upstream := p.Endpoint(); upstream != "" {
				return Result{Provider: p, Model: real, Upstream: upstream}, nil
			}
			return Result{}, fmt.Errorf("%w：供应商 %s 未配置上游地址", ErrModelNotFound, p.Name)
		}
	}

	providers := snap.Providers()
	if len(providers) == 0 {
		// 全新安装的常见状态：说清楚"没有供应商"而不是笼统的"模型不存在"
		return Result{}, fmt.Errorf(
			"%w：网关尚未配置任何供应商，无法路由模型 %q（请在 Web UI 的「供应商管理」中添加）",
			ErrModelNotFound, model)
	}

	var (
		noKeyProvider    *config.Provider
		outOfScope       bool
		outOfScopeTarget *config.Provider
	)
	for _, p := range providers {
		real, known := p.RealModel(model)
		if !known {
			continue
		}
		// 没有可用 Key 的供应商不参与聚合路由（含被自动停用的场景）；
		// 记下来，便于在"唯一声明者也用不了"时给出明确的 503 而不是笼统的 404。
		if !p.HasUsableKey() {
			if noKeyProvider == nil {
				noKeyProvider = p
			}
			continue
		}
		// 范围外的供应商不参与寻址：跳到下一个声明者，而不是直接拒绝。
		if !scope.Allows(p.ID, real) {
			if !outOfScope {
				outOfScope = true
				outOfScopeTarget = p
			}
			continue
		}
		upstream := p.Endpoint()
		if upstream == "" {
			continue // Normalize 已保证非空，这里仅防御
		}
		return Result{Provider: p, Model: real, Upstream: upstream}, nil
	}
	if noKeyProvider != nil {
		return Result{}, fmt.Errorf("%w：%s（可能已全部被自动停用，请在供应商页重新启用）", ErrProviderNoKey, noKeyProvider.Name)
	}
	if outOfScope && outOfScopeTarget != nil {
		// 模型确实配了，只是当前网关 Key 的范围里没有它：这是权限问题（403），
		// 不能报成「模型不存在」，否则用户会去查供应商配置而找不到原因。
		return Result{}, fmt.Errorf("%w：模型 %q（请在「网关设置」为该 Key 勾选对应模型，或改用其他 Key）",
			ErrModelNotAllowed, model)
	}
	return Result{}, fmt.Errorf("%w：%q", ErrModelNotFound, model)
}
