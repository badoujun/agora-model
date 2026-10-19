package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// keepAliveChunk 是 SSE 注释行：以 ":" 开头，所有合规的 SSE 客户端都会忽略它。
//
// 只在空闲超过阈值时注入，有数据时字节流与上游完全一致。
const keepAliveChunk = ": keep-alive\n\n"

type readChunk struct {
	data []byte
	err  error
}

type streamStats struct {
	bytes       int64
	firstByteIn time.Duration // 从入站请求开始到写出第一个数据块的耗时
}

// pumpUpstream 把上游字节搬进 channel。
//
// 它只读上游、只投递，绝不触碰 ResponseWriter —— 保证「单写入者」，
// 主循环因此无需加锁。两次投递都用 select 监听 ctx，避免主循环退出后永久阻塞。
func pumpUpstream(ctx context.Context, body io.ReadCloser, out chan<- readChunk) {
	defer close(out)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case out <- readChunk{data: chunk}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case out <- readChunk{err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

// streamResponse 透传 SSE 响应，并在空闲超过 idle 时注入心跳注释行。
//
// started 是入站请求的起始时刻。首字节延迟以它为起点，才包含「读请求体 + 路由 +
// 上游往返」这整段，从而把「上游慢」与「网关慢」区分开（DESIGN §7.4 第 5 条）；
// 若以本函数内的时刻为起点，`http.Client.Do` 返回时首批 body 字节往往已在本地
// 缓冲中，读数会恒为 0，该指标随即失去意义。
//
// 退出保证：无论主循环因何退出（客户端断开、写失败、上游结束），
// defer cancel() + defer resp.Body.Close() 都会让读协程结束，不留下阻塞的 goroutine。
//
// 返回值中的 usageParser 由调用方在循环结束后调用 Result() 取出最终累计的
// token 统计（OpenAI 通常在最后一个 chunk、Anthropic 在 message_delta）。
func streamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, idle time.Duration, started time.Time) (streamStats, streamUsageParser, error) {
	var stats streamStats

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer resp.Body.Close()

	rc := http.NewResponseController(w)
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // 提示反向代理关闭缓冲
	w.WriteHeader(resp.StatusCode)
	if err := rc.Flush(); err != nil {
		return stats, nil, err
	}

	// usage 解析：OpenAI 协议把每个 chunk 当 JSON 解析（找顶层 usage）；
	// Anthropic 协议按 SSE event 解析（message_start + message_delta 累计）。
	// 两种模式靠同一个 Content-Type 判断，但解析器入口不同；这里通过
	// streamUsageParser 接口把差异封装掉。
	parser := newStreamUsageParser(resp.Header.Get("Content-Type"))

	chunks := make(chan readChunk, 16)
	go pumpUpstream(ctx, resp.Body, chunks)

	ticker := time.NewTicker(idle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return stats, parser, ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				return stats, parser, nil
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					return stats, parser, nil
				}
				return stats, parser, chunk.err
			}
			if stats.bytes == 0 {
				stats.firstByteIn = time.Since(started)
			}
			if _, err := w.Write(chunk.data); err != nil {
				return stats, parser, err
			}
			if err := rc.Flush(); err != nil {
				return stats, parser, err
			}
			stats.bytes += int64(len(chunk.data))
			ticker.Reset(idle) // 有数据就重置空闲计时
			// 把字节喂给 usage 解析器。Feed 永远不返回错误，解析失败静默忽略。
			parser.Feed(chunk.data)
		case <-ticker.C:
			if _, err := w.Write([]byte(keepAliveChunk)); err != nil {
				return stats, parser, err
			}
			if err := rc.Flush(); err != nil {
				return stats, parser, err
			}
		}
	}
}

// streamUsageParser 抽象了"按 chunk 解析 usage"的差异。
//
// 真实实现由 newStreamUsageParser 按 Content-Type 选择 OpenAI 或 Anthropic。
// 返回的 Usage 通过 Result() 在流结束后取最终累计值。
type streamUsageParser interface {
	Feed(chunk []byte)
	Result() (Usage, bool)
}

// openAIStreamParser 复用 usage.go 里的 openAIUsageAccumulator。
type openAIStreamParser struct {
	acc *openAIUsageAccumulator
}

func (p *openAIStreamParser) Feed(chunk []byte)     { p.acc.Feed(chunk) }
func (p *openAIStreamParser) Result() (Usage, bool) { return p.acc.Result() }

// anthropicStreamParser 解析 SSE event 流（`event: <name>\ndata: <json>\n\n`）。
//
// pumpUpstream 给我们的字节是原始 chunk；可能跨多个 SSE 事件，也可能一个 chunk
// 包含多个事件。我们用一个游标维护未消费字节，逐个事件分派到 anthropicUsageAccumulator。
type anthropicStreamParser struct {
	acc    *anthropicUsageAccumulator
	buffer []byte
}

func (p *anthropicStreamParser) Feed(chunk []byte) {
	p.buffer = append(p.buffer, chunk...)
	p.parseBuffer()
}

func (p *anthropicStreamParser) Result() (Usage, bool) {
	// 流结束前可能残留不完整的最后一个事件；不解析，直接返回累计
	return p.acc.Result()
}

func (p *anthropicStreamParser) parseBuffer() {
	for {
		// 一个完整 SSE 事件以 `\n\n` 结束
		idx := bytes.Index(p.buffer, []byte("\n\n"))
		if idx < 0 {
			break
		}
		raw := p.buffer[:idx]
		p.buffer = p.buffer[idx+2:]
		eventName, data := splitSSEEvent(raw)
		if len(data) == 0 {
			continue
		}
		p.acc.Feed(eventName, data)
	}
}

// splitSSEEvent 把单个 SSE 事件块解析为 (eventName, dataBytes)。
//
// SSE 事件格式（每行以 \n 分隔）：
//
//	event: <name>\n
//	data: <json>\n
//
// 允许缺省 event 字段（默认空）、允许多个 data 行拼接。容错：忽略空行 / 不含 data 的事件。
func splitSSEEvent(block []byte) (eventName string, data []byte) {
	var dataLines [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	if len(dataLines) == 0 {
		return eventName, nil
	}
	// 多个 data 行之间用 \n 连接（OpenAI 习惯）
	data = bytes.Join(dataLines, []byte("\n"))
	return eventName, data
}

// newStreamUsageParser 根据 Content-Type 选 OpenAI 或 Anthropic 解析器。
//
// Anthropic 走标准 SSE `event:`/`data:` 协议；OpenAI 走非标准 chunked JSON。
// 其它 / 未知协议一律返回 noop parser。
func newStreamUsageParser(contentType string) streamUsageParser {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "anthropic"):
		return &anthropicStreamParser{acc: &anthropicUsageAccumulator{}}
	default:
		// OpenAI / 其它兼容协议都按 chunked JSON 解析
		return &openAIStreamParser{acc: &openAIUsageAccumulator{}}
	}
}

// copyResponse 透传非流式响应（状态码、头、体），不缓冲整个响应体。
//
// buf 可为 nil：传 nil 时等价于原来的 io.Copy 行为（不缓冲，不解析 usage）。
// 当调用方希望解析 usage 时传一个非 nil buffer，本函数把字节同步写入 buffer，
// 用 io.TeeReader 保证客户端拿到的字节与上游完全一致（DESIGN §11「透传保真」）。
func copyResponse(w http.ResponseWriter, resp *http.Response, buf *bytes.Buffer) (int64, error) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if buf == nil {
		return io.Copy(w, resp.Body)
	}
	return teeNonStreamBody(w, resp.Body, buf)
}
