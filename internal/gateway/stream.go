package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
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
func streamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, idle time.Duration, started time.Time) (streamStats, error) {
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
		return stats, err
	}

	chunks := make(chan readChunk, 16)
	go pumpUpstream(ctx, resp.Body, chunks)

	ticker := time.NewTicker(idle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return stats, ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				return stats, nil
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					return stats, nil
				}
				return stats, chunk.err
			}
			if stats.bytes == 0 {
				stats.firstByteIn = time.Since(started)
			}
			if _, err := w.Write(chunk.data); err != nil {
				return stats, err
			}
			if err := rc.Flush(); err != nil {
				return stats, err
			}
			stats.bytes += int64(len(chunk.data))
			ticker.Reset(idle) // 有数据就重置空闲计时
		case <-ticker.C:
			if _, err := w.Write([]byte(keepAliveChunk)); err != nil {
				return stats, err
			}
			if err := rc.Flush(); err != nil {
				return stats, err
			}
		}
	}
}

// copyResponse 透传非流式响应（状态码、头、体），不缓冲整个响应体。
func copyResponse(w http.ResponseWriter, resp *http.Response) (int64, error) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return io.Copy(w, resp.Body)
}
