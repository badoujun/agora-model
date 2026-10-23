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
	// ErrModelNotNamespaced 请求的模型名不是 供应商名/模型名 形式。
	//
	// 网关只接受显式命名空间寻址：裸模型名、或前缀不是已配置供应商名的
	// 模型名（含 meta-llama/Llama-3-70B 这类真实模型名）都会被拒绝，
	// 调用方应先用 /v1/models 获取可用的 供应商名/模型名 清单。
	ErrModelNotNamespaced = errors.New("模型名必须使用 供应商名/模型名 形式")
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
//  1. 只接受显式命名空间：模型名形如 供应商名/模型名，斜杠前的片段必须命中
//     **已存在的供应商名称**，将其强制作为路由目标（转发前把 model 改写为
//     该供应商声明的真实上游模型名）；
//  2. 裸模型名、或前缀不是已配置供应商名的模型名一律拒绝（ErrModelNotNamespaced）；
//  3. 供应商内配置了别名时，别名即对外模型名，转发时替换回真实模型名。
//
// 含斜杠的真实模型名（如 meta-llama/Llama-3-70B）不再按整体模型名透传：
// 需要为该模型配置别称，再用 供应商名/别称 调用。
func Resolve(snap *config.Snapshot, model string) (Result, error) {
	return ResolveScoped(snap, model, nil)
}

// ResolveScoped 在 Resolve 的基础上施加「当前网关 Key 的模型范围」约束。
//
// scope 为空（nil 或空集合）表示不限制，行为与 Resolve 完全一致。
//
// 命名空间命中但越界时返回 ErrModelNotAllowed（403）。
func ResolveScoped(snap *config.Snapshot, model string, scope config.ModelScope) (Result, error) {
	if snap == nil {
		return Result{}, ErrModelNotFound
	}

	idx := strings.Index(model, "/")
	if idx <= 0 || idx == len(model)-1 {
		return Result{}, fmt.Errorf(
			"%w：收到 %q，请改用 供应商名/模型名（可调用 %s 获取可用列表）",
			ErrModelNotNamespaced, model, config.ModelsPath)
	}
	name, rest := model[:idx], model[idx+1:]
	p, ok := snap.ProviderByName(name)
	if !ok {
		return Result{}, fmt.Errorf(
			"%w：%q 的前缀 %q 不是已配置的供应商名称（可调用 %s 获取可用列表）",
			ErrModelNotNamespaced, model, name, config.ModelsPath)
	}
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
