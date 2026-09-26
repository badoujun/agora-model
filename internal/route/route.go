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
	// ErrProtocolNotConfigured 命中的供应商未配置该协议的地址。
	ErrProtocolNotConfigured = errors.New("该供应商未配置此协议的地址")
	// ErrProviderDisabled 命名空间命中的供应商已停用。
	ErrProviderDisabled = errors.New("供应商已停用")
)

// Result 是一次成功的路由决策。
type Result struct {
	Provider *config.Provider
	Protocol config.Protocol
	// Model 是实际要转发给上游的模型名（Phase 3 的命名空间路由会剥离 provider 前缀）。
	Model string
	// Upstream 是完整的上游 URL。
	Upstream string
}

// Resolve 依据入站协议与请求中的模型名选择供应商与上游地址。
//
// 规则：
//  1. 命名空间路由：模型名形如 provider/model 且第一段命中**已存在的 provider id** 时，
//     强制走该供应商（转发前把 model 改写为斜杠后的部分）；
//  2. 否则按 priority 升序遍历启用的供应商，取第一个声明支持该模型的；
//  3. 命中的供应商必须配置了对应协议的地址，否则返回 ErrProtocolNotConfigured。
//
// 斜杠歧义保护：只有当斜杠前的片段确实是已存在的 provider id 时才按命名空间解析，
// 因此 meta-llama/Llama-3-70B 这类真实模型名不会被误判。
func Resolve(snap *config.Snapshot, proto config.Protocol, model string) (Result, error) {
	if snap == nil {
		return Result{}, ErrModelNotFound
	}

	if idx := strings.Index(model, "/"); idx > 0 && idx < len(model)-1 {
		providerID, rest := model[:idx], model[idx+1:]
		if p, ok := snap.Provider(providerID); ok {
			if !p.IsEnabled() {
				return Result{}, fmt.Errorf("%w：%s", ErrProviderDisabled, providerID)
			}
			if !p.KnowsModel(rest) {
				return Result{}, fmt.Errorf("%w：供应商 %s 未声明模型 %q", ErrModelNotFound, providerID, rest)
			}
			upstream := p.Endpoint(proto)
			if upstream == "" {
				return Result{}, fmt.Errorf("%w（provider=%s, protocol=%s）", ErrProtocolNotConfigured, providerID, proto)
			}
			return Result{Provider: p, Protocol: proto, Model: rest, Upstream: upstream}, nil
		}
	}

	for _, p := range snap.ProvidersByPriority() {
		if !p.KnowsModel(model) {
			continue
		}
		upstream := p.Endpoint(proto)
		if upstream == "" {
			return Result{}, fmt.Errorf("%w（provider=%s, protocol=%s）", ErrProtocolNotConfigured, p.ID, proto)
		}
		return Result{Provider: p, Protocol: proto, Model: model, Upstream: upstream}, nil
	}
	return Result{}, fmt.Errorf("%w：%q", ErrModelNotFound, model)
}
