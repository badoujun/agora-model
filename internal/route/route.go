package route

import (
	"errors"
	"fmt"

	"agora-model/internal/config"
)

var (
	// ErrModelNotFound 模型未被任何启用的供应商声明。
	ErrModelNotFound = errors.New("模型未被任何启用的供应商声明")
	// ErrProtocolNotConfigured 命中的供应商未配置该协议的地址。
	ErrProtocolNotConfigured = errors.New("该供应商未配置此协议的地址")
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
// 规则（Phase 1）：
//  1. 按 priority 升序遍历启用的供应商；
//  2. 取第一个声明支持该模型的供应商；
//  3. 该供应商必须配置了对应协议的地址，否则返回 ErrProtocolNotConfigured。
//
// Phase 3 会在第 1 步之前插入 provider/model 命名空间的显式路由分支。
func Resolve(snap *config.Snapshot, proto config.Protocol, model string) (Result, error) {
	if snap == nil {
		return Result{}, ErrModelNotFound
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
