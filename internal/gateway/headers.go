package gateway

import (
	"net/http"
	"strings"

	"agora-model/internal/config"
)

// hopByHopHeaders 是逐跳头，转发时必须剥离（RFC 7230 §6.1）。
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// requestHeadersToStrip 是从入站请求转发到上游时需要剥离的头。
//
//   - authorization / x-api-key：换成供应商凭证
//   - host：由 Go 依 URL 自动设置
//   - content-length：由 http.NewRequest 依 body 计算
//   - accept-encoding：不转发，使上游返回未压缩内容，保证 SSE 逐块透传的字节流干净
var requestHeadersToStrip = map[string]bool{
	"authorization":   true,
	"x-api-key":       true,
	"host":            true,
	"content-length":  true,
	"accept-encoding": true,
}

func canonicalHeader(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func isHopByHop(name string) bool { return hopByHopHeaders[canonicalHeader(name)] }

// buildUpstreamHeaders 依据入站请求与供应商配置构造上游请求头。
//
// 其余业务头（anthropic-version、openai-*、user-agent 等）原样透传。
func buildUpstreamHeaders(in http.Header, p *config.Provider) http.Header {
	out := make(http.Header, len(in)+len(p.ExtraHeaders)+1)
	for name, values := range in {
		lower := canonicalHeader(name)
		if isHopByHop(lower) || requestHeadersToStrip[lower] {
			continue
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}

	out.Set("Authorization", "Bearer "+p.APIKey)

	// extra_headers 可覆盖业务头，但不允许覆盖 Host 与认证头
	for name, value := range p.ExtraHeaders {
		lower := canonicalHeader(name)
		if isHopByHop(lower) || requestHeadersToStrip[lower] || lower == "host" {
			continue
		}
		out.Set(name, value)
	}
	return out
}

// copyResponseHeaders 透传上游响应头，剥离逐跳头与 Content-Length。
//
// 剥离 Content-Length 是刻意的：流式响应长度不可预知，非流式也可能与重新写入的
// 字节数不完全一致（例如 Go 的自动解压），交给 ResponseWriter 处理更安全。
func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := canonicalHeader(name)
		if isHopByHop(lower) || lower == "content-length" {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// isEventStream 判断上游响应是否为 SSE。
func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}
