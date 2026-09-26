package logging

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"log/slog"
)

// Entry 是一条请求日志记录。
//
// 字段与 store 的 logs 表一一对应；本包不依赖 store，避免循环依赖。
type Entry struct {
	TS              time.Time
	RequestID       string
	InboundProtocol string
	Model           string
	ProviderID      string
	UpstreamURL     string
	StatusCode      int
	LatencyMS       int64
	FirstByteMS     int64
	Stream          bool
	ErrorMsg        string
	ClientIP        string
}

// Writer 是日志落库接口（由 store 实现）。
type Writer interface {
	InsertLogs(ctx context.Context, entries []Entry) error
}

// Recorder 以「异步 + 批量」方式写入请求日志。
//
// 设计取舍：可用性优先——队列满时丢弃并计数，绝不阻塞转发路径（DESIGN §7.9）。
type Recorder struct {
	writer     Writer
	logger     *slog.Logger
	ch         chan Entry
	batchSize  int
	flushEvery time.Duration
	written    atomic.Int64
	dropped    atomic.Int64
	done       chan struct{}
}

// RecorderOptions 配置批量写入参数。
type RecorderOptions struct {
	QueueSize  int
	BatchSize  int
	FlushEvery time.Duration
}

// NewRecorder 创建请求日志记录器。
func NewRecorder(w Writer, logger *slog.Logger, opts RecorderOptions) *Recorder {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		writer:     w,
		logger:     logger,
		ch:         make(chan Entry, opts.QueueSize),
		batchSize:  opts.BatchSize,
		flushEvery: opts.FlushEvery,
		done:       make(chan struct{}),
	}
}

// Start 启动后台写入协程；ctx 取消时会把剩余批次刷完再退出。
func (r *Recorder) Start(ctx context.Context) {
	go r.loop(ctx)
}

// Wait 等待写入协程退出（关闭流程使用），返回是否在超时前完成。
func (r *Recorder) Wait(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Enqueue 投递一条日志；队列满时丢弃并计数，不阻塞调用方。
func (r *Recorder) Enqueue(e Entry) {
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
	}
}

// Stats 返回已写入与已丢弃条数。
func (r *Recorder) Stats() (written, dropped int64) {
	return r.written.Load(), r.dropped.Load()
}

func (r *Recorder) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(r.flushEvery)
	defer ticker.Stop()

	batch := make([]Entry, 0, r.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// 落库使用独立 ctx：即使入站请求的 ctx 已取消也要把日志写完
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.writer.InsertLogs(writeCtx, batch); err != nil {
			r.logger.Warn("请求日志写入失败，本批已计入丢弃",
				"count", len(batch), "err", err)
			r.dropped.Add(int64(len(batch)))
		} else {
			r.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) >= r.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// secretPattern 匹配形如 sk-xxx / gw-xxx 的密钥串。
var secretPattern = regexp.MustCompile(`\b(?:sk|gw)-[A-Za-z0-9_\-]{6,}`)

// Redact 脱敏并把文本截断到 limit 字节以内（limit <= 0 表示不截断）。
func Redact(text string, limit int) string {
	if text == "" {
		return ""
	}
	out := secretPattern.ReplaceAllString(text, "***REDACTED***")
	if limit > 0 && len(out) > limit {
		out = strings.ToValidUTF8(out[:limit], "")
	}
	return out
}
