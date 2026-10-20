package store

import (
	"context"
	"sync/atomic"
	"time"

	"agora-model/internal/logging"
)

// UsageRecorderOptions 配置 UsageRecorder 的批量写入参数。
type UsageRecorderOptions struct {
	// QueueSize 是 channel 缓冲区大小；队列满时丢弃并计数。
	QueueSize int
	// BatchSize 是每次 flush 的最大 bucket 数。
	BatchSize int
	// FlushEvery 是两次 flush 之间的最长间隔（即使 batch 未满也刷）。
	FlushEvery time.Duration
	// PruneEvery 是两次 PruneUsageMinute 之间的间隔（设 ≤ 0 表示不启用）。
	PruneEvery time.Duration
	// PruneRetention 是 PruneUsageMinute 的"早于多少分钟"阈值（设 ≤ 0 表示 24h）。
	PruneRetention time.Duration
}

// DefaultUsageRecorderOptions 返回合理的默认值。
func DefaultUsageRecorderOptions() UsageRecorderOptions {
	return UsageRecorderOptions{
		QueueSize:      4096,
		BatchSize:      256,
		FlushEvery:     30 * time.Second,
		PruneEvery:     time.Hour,
		PruneRetention: 24 * time.Hour,
	}
}

// UsageRecorder 以「异步 + 批量」方式聚合并写入 token 用量。
//
// 设计取舍：可用性优先——队列满时丢弃并计数，绝不阻塞转发路径
// （与 logging.Recorder 一致，DESIGN §7.9）。
//
// UsageRecorder 同时实现 logging.UsageSink 接口（main 把 gateway.WithUsageSink
// 直接注入本 recorder），因此 gateway 与 store 之间不需要相互 import。
type UsageRecorder struct {
	writer  *Store
	logger  slogLogger
	opts    UsageRecorderOptions
	ch      chan usageEvent
	done    chan struct{}
	written atomic.Int64
	dropped atomic.Int64
	pruned  atomic.Int64
}

// usageEvent 是一条投递到 recorder 的「单次请求用量」事件。
//
// 内存里按 minute + (providerID, model) 分组合并；到 flush 时再批量
// upsert 到 SQLite。字段与 logging.Usage 对齐，并附 provider_id / model / ts。
type usageEvent struct {
	ts         time.Time
	providerID string
	keyID      string
	model      string
	usage      logging.Usage
}

// slogLogger 是 logging.Logger 的最小接口，避免直接 import log/slog 进来的
// 跨包依赖噪音（事实上 store 包只用到 Info/Warn/Error 时这两个接口够了）。
type slogLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// noopLogger 用于 main 还没注入 logger 时的兜底；任何 log 都静默丢弃。
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// NewUsageRecorder 创建 recorder 但不启动后台协程。
//
// Start 必须随后被调用才能消费事件；Wait 在关闭时调用以保证剩余批次
// 刷完（与 logging.Recorder 一致）。
func NewUsageRecorder(s *Store, logger slogLogger, opts UsageRecorderOptions) *UsageRecorder {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 4096
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 256
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = 30 * time.Second
	}
	if logger == nil {
		logger = noopLogger{}
	}
	return &UsageRecorder{
		writer: s,
		logger: logger,
		opts:   opts,
		ch:     make(chan usageEvent, opts.QueueSize),
		done:   make(chan struct{}),
	}
}

// Record 满足 logging.UsageSink 接口。
//
// 仅 statusCode < 400 的请求计入（与 gateway 端 recordUsage 一致）。
// zero usage 直接丢弃，避免给 daily 灌零。
func (r *UsageRecorder) Record(providerID, keyID, model string, statusCode int, usage logging.Usage, ts time.Time) {
	if statusCode >= 400 {
		return
	}
	if usage.IsZero() {
		return
	}
	select {
	case r.ch <- usageEvent{ts: ts, providerID: providerID, keyID: keyID, model: model, usage: usage}:
	default:
		r.dropped.Add(1)
	}
}

// Start 启动后台批量写协程；ctx 取消时把剩余批次刷完再退出。
func (r *UsageRecorder) Start(ctx context.Context) {
	go r.loop(ctx)
}

// Wait 等待写协程退出（关闭流程使用），返回是否在超时前完成。
func (r *UsageRecorder) Wait(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stats 返回累计写入 / 丢弃 / 清理的条数（dropped 是事件数，pruned 是被删的 minute 行数）。
func (r *UsageRecorder) Stats() (written, dropped, pruned int64) {
	return r.written.Load(), r.dropped.Load(), r.pruned.Load()
}

func (r *UsageRecorder) loop(ctx context.Context) {
	defer close(r.done)

	flushTicker := time.NewTicker(r.opts.FlushEvery)
	defer flushTicker.Stop()

	var pruneTicker *time.Ticker
	var pruneC <-chan time.Time
	if r.opts.PruneEvery > 0 {
		pruneTicker = time.NewTicker(r.opts.PruneEvery)
		defer pruneTicker.Stop()
		pruneC = pruneTicker.C
	}

	// 启动时跑一次 prune，避免上次崩溃时残留旧 minute 行
	r.runPrune(ctx)

	batch := make([]usageEvent, 0, r.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.flushBatch(ctx, batch); err != nil {
			r.logger.Warn("token 用量写入失败，本批已计入丢弃",
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
			// 退出前把 channel 里剩余的全部取出 flush
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) >= r.opts.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case <-flushTicker.C:
			flush()
		case <-pruneC:
			r.runPrune(ctx)
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.opts.BatchSize {
				flush()
			}
		}
	}
}

// flushBatch 把一批事件按 (provider, model, minute/day) 聚合后写库。
//
// 日聚合与分钟明细是两个独立事务：日聚合失败时仍尝试写分钟明细，
// 避免分钟明细也失败时整批丢失（分钟明细是 source of truth，日聚合可下一窗口重算）。
//
// 返回任一事务的错误让调用方把整批计入 dropped。
func (r *UsageRecorder) flushBatch(ctx context.Context, batch []usageEvent) error {
	dayBuckets := aggregateDay(batch)
	minuteBuckets := aggregateMinute(batch)
	keyBuckets := aggregateKeyDay(batch)
	if len(dayBuckets) == 0 && len(minuteBuckets) == 0 && len(keyBuckets) == 0 {
		return nil
	}
	// 按 Key 的日用量是附加维度：失败只告警，不影响主用量链路。
	if err := r.writer.UpsertProviderKeyUsageDaily(ctx, keyBuckets); err != nil {
		r.logger.Warn("按 Key 用量写入失败，下一窗口会重试",
			"buckets", len(keyBuckets), "err", err)
	}
	if err := r.writer.UpsertUsageDaily(ctx, dayBuckets); err != nil {
		r.logger.Warn("日聚合写入失败，下一窗口会重试",
			"buckets", len(dayBuckets), "err", err)
	}
	if err := r.writer.UpsertUsageMinute(ctx, minuteBuckets); err != nil {
		r.logger.Error("分钟明细写入失败",
			"buckets", len(minuteBuckets), "err", err)
		return err
	}
	return nil
}

// aggregateDay 按 (date, provider, model) 合并事件，返回 DailyBucket 列表。
func aggregateDay(batch []usageEvent) []DailyBucket {
	type key struct {
		date       string
		providerID string
		model      string
	}
	merged := make(map[key]*DailyBucket)
	for _, e := range batch {
		date := e.ts.UTC().Format("2006-01-02")
		k := key{date: date, providerID: e.providerID, model: e.model}
		b, ok := merged[k]
		if !ok {
			b = &DailyBucket{
				Date:       date,
				ProviderID: e.providerID,
				Model:      e.model,
			}
			merged[k] = b
		}
		b.Requests++
		b.InputTokens += e.usage.InputTokens
		b.CachedInputTokens += e.usage.CachedInputTokens
		b.OutputTokens += e.usage.OutputTokens
	}
	out := make([]DailyBucket, 0, len(merged))
	for _, b := range merged {
		out = append(out, *b)
	}
	return out
}

// aggregateMinute 按 (minute, provider, model) 合并事件，返回 MinuteBucket 列表。
func aggregateMinute(batch []usageEvent) []MinuteBucket {
	type key struct {
		minute     string
		providerID string
		model      string
	}
	merged := make(map[key]*MinuteBucket)
	for _, e := range batch {
		minute := e.ts.UTC().Format("2006-01-02T15:04")
		k := key{minute: minute, providerID: e.providerID, model: e.model}
		b, ok := merged[k]
		if !ok {
			b = &MinuteBucket{
				Minute:     parseMinuteUTC(minute),
				ProviderID: e.providerID,
				Model:      e.model,
			}
			merged[k] = b
		}
		b.Requests++
		b.InputTokens += e.usage.InputTokens
		b.CachedInputTokens += e.usage.CachedInputTokens
		b.OutputTokens += e.usage.OutputTokens
	}
	out := make([]MinuteBucket, 0, len(merged))
	for _, b := range merged {
		out = append(out, *b)
	}
	return out
}

// aggregateKeyDay 按 (date, key) 合并事件，返回按 Key 的日用量桶。
//
// 没有 keyID 的事件（历史调用方/未注入）直接跳过：按 Key 统计是附属能力。
func aggregateKeyDay(batch []usageEvent) []KeyDailyBucket {
	type key struct {
		date  string
		keyID string
	}
	merged := make(map[key]*KeyDailyBucket)
	for _, e := range batch {
		if e.keyID == "" {
			continue
		}
		date := e.ts.UTC().Format("2006-01-02")
		k := key{date: date, keyID: e.keyID}
		b, ok := merged[k]
		if !ok {
			b = &KeyDailyBucket{
				Date:       date,
				KeyID:      e.keyID,
				ProviderID: e.providerID,
			}
			merged[k] = b
		}
		b.Requests++
		b.InputTokens += e.usage.InputTokens
		b.CachedInputTokens += e.usage.CachedInputTokens
		b.OutputTokens += e.usage.OutputTokens
	}
	out := make([]KeyDailyBucket, 0, len(merged))
	for _, b := range merged {
		out = append(out, *b)
	}
	return out
}

// parseMinuteUTC 把 "YYYY-MM-DDTHH:MM" 解析回 time.Time（UTC）。
func parseMinuteUTC(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.UTC)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}

// runPrune 调一次 PruneUsageMinute。Retention <= 0 时用 24h。
func (r *UsageRecorder) runPrune(ctx context.Context) {
	retention := r.opts.PruneRetention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	before := time.Now().UTC().Add(-retention)
	n, err := r.writer.PruneUsageMinute(ctx, before)
	if err != nil {
		r.logger.Warn("清理分钟明细失败",
			"before", before.Format(time.RFC3339), "err", err)
		return
	}
	if n > 0 {
		r.pruned.Add(n)
		r.logger.Info("清理过期分钟明细",
			"rows", n, "before", before.Format(time.RFC3339))
	}
}
