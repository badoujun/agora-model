// Package logging 提供跨 gateway / store / api 共享的轻量类型。
//
// 本文件定义 Usage 类型与 UsageSink 接口：它们被以下三个包共同使用，
// 因此放在没有业务依赖的 logging 包里，避免循环依赖：
//
//   - internal/gateway：解析上游 usage 后调用 Record 上报；
//   - internal/store：UsageRecorder 实现 Record 接口消费；
//   - cmd/agoramodel：main 把 gateway.WithUsageSink(recorder) 串起来。
package logging

import "time"

// Usage 是从上游响应中归一化的 token 统计。
//
// 与前端 /api/usage 的字段一一对应：
//   - InputTokens: 输入 token（含未命中 + 命中缓存的总和）
//   - CachedInputTokens: 命中缓存的输入 token（OpenAI prompt_tokens_details.cached_tokens；
//     Anthropic cache_read_input_tokens + cache_creation_input_tokens 合并）
//   - OutputTokens: 输出 token
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// IsZero 报告 usage 是否全为 0（上游没返回 usage 或返回 0）。
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.CachedInputTokens == 0 && u.OutputTokens == 0
}

// UsageSink 是 token 统计的下游接收方抽象。
//
// gateway 在请求完成后调用 Record 把 usage 投递出去；
// 生产实现是 store.UsageRecorder（异步批量写 SQLite）。
// gatewayKeyID 是发起请求的网关 API Key（可能为空：静态 Key 模式或历史数据）；
// keyID 是实际服务该请求的供应商 Key。
type UsageSink interface {
	Record(gatewayKeyID, providerID, keyID, model string, statusCode int, usage Usage, ts time.Time)
}

// UsageSinkFunc 让普通函数实现 UsageSink 接口，避免写 adapter 类型。
type UsageSinkFunc func(gatewayKeyID, providerID, keyID, model string, statusCode int, usage Usage, ts time.Time)

// Record 让 UsageSinkFunc 满足 UsageSink 接口。
func (f UsageSinkFunc) Record(gatewayKeyID, providerID, keyID, model string, statusCode int, usage Usage, ts time.Time) {
	if f == nil {
		return
	}
	f(gatewayKeyID, providerID, keyID, model, statusCode, usage, ts)
}
