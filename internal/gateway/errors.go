package gateway

import (
	"encoding/json"
	"net/http"
)

// 错误码，与 docs/DESIGN.md §5.4 的错误码表一致。
const (
	codeInvalidRequest      = "invalid_request_error"
	codeInvalidAPIKey       = "invalid_api_key"
	codeModelNotFound       = "model_not_found"
	codeNotFound            = "not_found"
	codePayloadTooLarge     = "payload_too_large"
	codeUpstreamUnreachable = "upstream_unreachable"
	codeNoAvailableProvider = "no_available_provider"
	codeUpstreamTimeout     = "upstream_timeout"
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

// writeError 以 OpenAI 兼容的错误体输出网关自身产生的错误。
//
// 注意：上游返回的错误（含 4xx/5xx）一律原样透传，不走这里。
func writeError(w http.ResponseWriter, status int, code, message, param string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{
		Error: openAIErrorBody{
			Message: message,
			Type:    openAITypeOf(code),
			Param:   param,
			Code:    code,
		},
	})
}
