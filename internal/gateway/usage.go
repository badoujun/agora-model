package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"agora-model/internal/logging"
)

// Usage 是从上游响应中归一化的 token 统计。
//
// 类型别名：定义在 internal/logging/usage.go，避免 gateway / store 之间
// 互相 import（store 写 SQLite 时也要消费这个类型）。
type Usage = logging.Usage

// UsageSink 抽象了 token 统计的下游接收方。
//
// gateway 在请求完成后调用 Record 把 usage 投递出去；当前生产实现是
// store/usage.go 里的 UsageRecorder（异步批量写入 SQLite）。mock 测试里
// 可以注入一个内存版来直接断言。
type UsageSink = logging.UsageSink

// noopUsageSink 在没注入 sink 时使用，避免 nil 检查散落在调用处。
type noopUsageSink struct{}

func (noopUsageSink) Record(string, string, int, Usage, time.Time) {}

// parseOpenAIUsageNonStream 从一个 OpenAI 风格的非流式响应体解析 usage。
//
// OpenAI 非流式响应的顶层结构：
//
//	{ ..., "usage": { "prompt_tokens": int, "completion_tokens": int,
//	  "prompt_tokens_details": { "cached_tokens": int } } }
//
// 返回值 ok=false 表示 body 不是合法 JSON 或不含 usage 字段。
func parseOpenAIUsageNonStream(body []byte) (Usage, bool) {
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

// openAIUsageAccumulator 聚合流式响应里的 token 统计。
//
// OpenAI 协议把 usage 放在最后一个 chunk（形如 `{ ..., "choices": [], "usage": {...} }`）。
// 同一段字节里也可能包含多个 SSE 事件（choices chunk + usage chunk + [DONE]），
// 因此我们每次 Feed 都按 SSE 协议分块逐个解析，取最后一个含 usage 的事件。
type openAIUsageAccumulator struct {
	usage Usage
	found bool
}

// feedOne 解析单个 SSE 事件字节（已去掉 "data: " 前缀、去掉首尾空白）；
// 若含 usage 且不全为 0 则覆盖累计值。返回是否设置过 usage（用于调用方表达错误时区分）。
func (a *openAIUsageAccumulator) feedOne(payload []byte) {
	var envelope struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return
	}
	if envelope.Usage.PromptTokens == 0 && envelope.Usage.CompletionTokens == 0 {
		return
	}
	cached := envelope.Usage.PromptDetails.CachedTokens
	if cached > envelope.Usage.PromptTokens {
		cached = envelope.Usage.PromptTokens
	}
	a.usage = Usage{
		InputTokens:       envelope.Usage.PromptTokens,
		CachedInputTokens: cached,
		OutputTokens:      envelope.Usage.CompletionTokens,
	}
	a.found = true
}

func (a *openAIUsageAccumulator) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	// 兼容两种输入形态：
	//   - 生产路径（stream.go → openAIStreamParser.Feed）：字节来自 pumpUpstream，
	//     可能是 "data: {...}\n\ndata: {...}\n\n" 这种 SSE 形态，也可能包含 [DONE] 哨兵。
	//     此时按 SSE 协议分块逐个解析。
	//   - 单测 / 调试工具：直接喂裸 JSON 对象。
	// 判定依据是去首尾空白后首字符是不是 `{`：是则裸 JSON，否则按 SSE。
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		a.feedOne(trimmed)
		return
	}
	for _, payload := range splitOpenAISSEChunks(chunk) {
		a.feedOne(payload)
	}
}

func (a *openAIUsageAccumulator) Result() (Usage, bool) {
	return a.usage, a.found
}

// parseAnthropicUsageNonStream 从一个 Anthropic messages 响应体解析 usage。
//
// Anthropic 非流式响应顶层：
//
//	{ ..., "usage": { "input_tokens": int, "output_tokens": int,
//	  "cache_read_input_tokens": int, "cache_creation_input_tokens": int } }
//
// 注：Anthropic 的 input_tokens **不含** cache 部分（即上游计费口径已经
// 区分了 input / cache_read / cache_creation）。这里把 cache_read 和
// cache_creation 合并作为「命中缓存」一并记入 CachedInputTokens，与前端 mock 口径一致。
func parseAnthropicUsageNonStream(body []byte) (Usage, bool) {
	var envelope struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			CacheCreationTokens  int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Usage{}, false
	}
	if envelope.Usage.InputTokens == 0 && envelope.Usage.OutputTokens == 0 &&
		envelope.Usage.CacheReadInputTokens == 0 && envelope.Usage.CacheCreationTokens == 0 {
		return Usage{}, false
	}
	cached := envelope.Usage.CacheReadInputTokens + envelope.Usage.CacheCreationTokens
	return Usage{
		InputTokens:       envelope.Usage.InputTokens + cached,
		CachedInputTokens: cached,
		OutputTokens:      envelope.Usage.OutputTokens,
	}, true
}

// anthropicUsageAccumulator 处理 Anthropic 流式响应的多事件类型：
//
//   - message_start: data.usage.input_tokens（首次；output_tokens 当时为 0）
//   - content_block_start / content_block_delta / content_block_stop / ping：不带 usage
//   - message_delta: data.usage.output_tokens（最终累计）
//   - message_stop: 不带 usage
//
// 我们把 message_start 的 input/cache_* 累加到 input 字段；把 message_delta 的
// output_tokens 作为最终 output_tokens 字段。
type anthropicUsageAccumulator struct {
	inputTokens       int
	cachedInputTokens int
	outputTokens      int
	started           bool
}

func (a *anthropicUsageAccumulator) Feed(event string, data []byte) {
	switch event {
	case "message_start":
		var env struct {
			Message struct {
				Usage struct {
					InputTokens          int `json:"input_tokens"`
					OutputTokens         int `json:"output_tokens"`
					CacheReadInputTokens int `json:"cache_read_input_tokens"`
					CacheCreationTokens  int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return
		}
		cached := env.Message.Usage.CacheReadInputTokens + env.Message.Usage.CacheCreationTokens
		a.inputTokens = env.Message.Usage.InputTokens + cached
		a.cachedInputTokens = cached
		a.started = true
	case "message_delta":
		var env struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return
		}
		a.outputTokens = env.Usage.OutputTokens
	}
}

func (a *anthropicUsageAccumulator) Result() (Usage, bool) {
	if !a.started {
		return Usage{}, false
	}
	return Usage{
		InputTokens:       a.inputTokens,
		CachedInputTokens: a.cachedInputTokens,
		OutputTokens:      a.outputTokens,
	}, true
}

// teeNonStreamBody 把上游响应体同时写到 w 和 buf，并返回 buf。
//
// gateway 在非流式透传时使用：用 io.TeeReader 把字节同时给客户端和我们的
// 副本缓冲，副本用来解析 usage。客户端拿到的字节与上游完全一致（零改写）。
func teeNonStreamBody(w io.Writer, src io.Reader, buf *bytes.Buffer) (int64, error) {
	tee := io.TeeReader(src, buf)
	return io.Copy(w, tee)
}

// recordUsage 在成功请求里调用 sink。失败请求（statusCode >= 400）由调用方跳过。
func recordUsage(sink UsageSink, providerID, model string, statusCode int, u Usage, ts time.Time) {
	if sink == nil {
		return
	}
	if statusCode >= 400 {
		return
	}
	if u.IsZero() {
		return
	}
	sink.Record(providerID, model, statusCode, u, ts)
}

// parseUsageNonStream 从一个完整的非流式响应体解析 usage，按 Content-Type
// 决定走 OpenAI 还是 Anthropic 解析器。
//
// 返回值 ok=false 表示 body 不是合法 JSON、不含 usage、或全是 0。
func parseUsageNonStream(contentType string, body []byte) (Usage, bool) {
	if len(body) == 0 {
		return Usage{}, false
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "anthropic") {
		return parseAnthropicUsageNonStream(body)
	}
	return parseOpenAIUsageNonStream(body)
}

// splitOpenAISSEChunks 把一段字节按 SSE 协议切成多个 data 负载。
//
// pumpUpstream 一次 Read 可能拿到"多个 SSE 事件拼一起"的字节：
//
//	data: {...choices...}\n\n
//	data: {...usage...}\n\n
//	data: [DONE]\n\n
//
// 用 encoding/json 一次解整块会因为非空白后缀字符（如 [DONE] 或 data:）而失败，
// 整个 chunk 内的 usage 事件被一并丢弃。本函数按 \n\n 分块并去掉 data: 前缀，
// 把每个事件视作独立 JSON 字符串丢给 feedOne 解析，确保"含 usage 的 chunk"
// 即便与 [DONE] 或前一个 choices chunk 拼在一起也能被捕获。
//
// 容错策略：
//   - 块首尾空白被裁掉
//   - 仅保留以 "data:" 开头的行；忽略 event: / id: / retry: 等行
//   - 同一个事件内多 data 行用 \n 拼接（与 splitSSEEvent 一致）
//   - 块不是合法 JSON / 不带 data: 行 → 不返回该块（不会让 feedOne 报错）
func splitOpenAISSEChunks(buf []byte) [][]byte {
	out := make([][]byte, 0, 4)
	for {
		idx := bytes.Index(buf, []byte("\n\n"))
		var raw []byte
		if idx < 0 {
			raw = buf
			buf = nil
		} else {
			raw = buf[:idx]
			buf = buf[idx+2:]
		}
		if payload, ok := extractOpenAIDataPayload(raw); ok {
			out = append(out, payload)
		}
		if idx < 0 {
			break
		}
	}
	return out
}

// extractOpenAIDataPayload 从一个 SSE 事件块（去掉首尾空白、未分隔 \n\n）抽出 data 行。
// 仅返回非空、含至少一行 "data: ..." 的合并负载；其它情况返回 ok=false。
func extractOpenAIDataPayload(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	var dataLines [][]byte
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		dataLines = append(dataLines, payload)
	}
	if len(dataLines) == 0 {
		return nil, false
	}
	return bytes.Join(dataLines, []byte("\n")), true
}
