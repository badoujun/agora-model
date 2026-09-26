package gateway

import (
	"encoding/json"
	"net/http"

	"agora-model/internal/config"
)

// 错误码，与 docs/DESIGN.md §5.4 的错误码表一致。
const (
	codeInvalidRequest        = "invalid_request_error"
	codeInvalidAPIKey         = "invalid_api_key"
	codeProtocolNotConfigured = "upstream_protocol_not_configured"
	codeModelNotFound         = "model_not_found"
	codeNotFound              = "not_found"
	codePayloadTooLarge       = "payload_too_large"
	codeUpstreamUnreachable   = "upstream_unreachable"
	codeNoAvailableProvider   = "no_available_provider"
	codeUpstreamTimeout       = "upstream_timeout"
)

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicError struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

// openAITypeOf 把错误码映射为 OpenAI 惯用的 error.type。
func openAITypeOf(code string) string {
	switch code {
	case codeInvalidAPIKey:
		return "authentication_error"
	case codeModelNotFound, codeNotFound:
		return "not_found_error"
	case codeUpstreamUnreachable, codeUpstreamTimeout, codeNoAvailableProvider:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// anthropicTypeOf 把 HTTP 状态映射为 Anthropic 的 error.type。
func anthropicTypeOf(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// writeError 以入站协议对应的风格输出网关自身产生的错误。
//
// 注意：上游返回的错误（含 4xx/5xx）一律原样透传，不走这里。
func writeError(w http.ResponseWriter, proto config.Protocol, status int, code, message, param string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if proto == config.ProtocolAnthropic {
		_ = enc.Encode(anthropicError{
			Type:  "error",
			Error: anthropicErrorBody{Type: anthropicTypeOf(status), Message: message},
		})
		return
	}
	_ = enc.Encode(openAIError{
		Error: openAIErrorBody{
			Message: message,
			Type:    openAITypeOf(code),
			Param:   param,
			Code:    code,
		},
	})
}
