package logging

import "encoding/json"

// ParseOpenAIUsage 解析一个 OpenAI 风格（非流式）响应体里的 token 统计。
//
// 口径与网关转发路径完全一致：prompt_tokens 记作输入，completion_tokens 记作输出，
// prompt_tokens_details.cached_tokens 记作命中缓存的输入（不超过输入总量）。
//
// 返回值 ok=false 表示 body 不是合法 JSON、不含 usage 字段，或 usage 全为 0。
// 控制台的「单 Key 连通性测试」也走这个解析，确保两条路径的计数方式不会分叉。
func ParseOpenAIUsage(body []byte) (Usage, bool) {
	var envelope struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Usage{}, false
	}
	if envelope.Usage.PromptTokens == 0 && envelope.Usage.CompletionTokens == 0 {
		return Usage{}, false
	}
	cached := envelope.Usage.PromptDetails.CachedTokens
	if cached > envelope.Usage.PromptTokens {
		cached = envelope.Usage.PromptTokens
	}
	return Usage{
		InputTokens:       envelope.Usage.PromptTokens,
		CachedInputTokens: cached,
		OutputTokens:      envelope.Usage.CompletionTokens,
	}, true
}
