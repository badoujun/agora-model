package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// TouchProviderKey 更新供应商 Key 的最后使用时间（调用方负责节流）。
func (s *Store) TouchProviderKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE provider_keys SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), keyID)
	return err
}

// RecordProviderKeyFailure 记一次凭证类失败（不改变启用状态，用于 429 等临时失败）。
func (s *Store) RecordProviderKeyFailure(ctx context.Context, keyID, reason string) error {
	if keyID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE provider_keys SET failure_count = failure_count + 1,
		   last_failure_at = ?, last_error = ?
		 WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), truncate(reason, 200), keyID)
	return err
}

// DisableProviderKey 把某个 Key 自动停用（凭证无效 / 欠费 / 无权限 / 限速）。
//
// WHERE enabled = 1 让「用户在界面上刚重新启用」的 Key 不会被随后到达的
// 同一次失败写回停用状态。停用后若该供应商已没有任何启用的 Key，
// 一并停用供应商（恢复必须由人工重新启用）。
func (s *Store) DisableProviderKey(ctx context.Context, keyID, reason string) error {
	if keyID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	reason = truncate(reason, 200)
	if _, err := tx.ExecContext(ctx,
		`UPDATE provider_keys SET enabled = 0, auto_disabled = 1, disabled_reason = ?,
		   failure_count = failure_count + 1, last_failure_at = ?, last_error = ?
		 WHERE id = ? AND enabled = 1`,
		reason, now, reason, keyID); err != nil {
		return err
	}

	var providerID string
	if err := tx.QueryRowContext(ctx,
		`SELECT provider_id FROM provider_keys WHERE id = ?`, keyID).Scan(&providerID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if _, err := disableProviderIfExhaustedTx(ctx, tx, providerID); err != nil {
		return err
	}
	return tx.Commit()
}

// disableProviderIfExhaustedTx 在某供应商已没有任何启用的 Key 时停用该供应商。
//
// 判定放在数据库里用 NOT EXISTS 完成，而不是依赖数据面的内存状态：
// 并发请求各自停用不同 Key 时，只要最后一个 Key 的停用落库，
// 任何一个批次的检查都会命中，不会出现"Key 全停用但供应商仍启用"的泄漏。
// 返回 true 表示本次调用确实停用了供应商。
func disableProviderIfExhaustedTx(ctx context.Context, tx *sql.Tx, providerID string) (bool, error) {
	if providerID == "" {
		return false, nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE providers SET enabled = 0, updated_at = ?
		 WHERE id = ? AND enabled = 1
		   AND NOT EXISTS (SELECT 1 FROM provider_keys WHERE provider_id = ? AND enabled = 1)`,
		time.Now().UTC().Format(time.RFC3339), providerID, providerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ProviderKeyStatsOptions 配置凭证健康状态记录的批量参数。
type ProviderKeyStatsOptions struct {
	// QueueSize 是 channel 缓冲区大小；队列满时丢弃并计数（可用性优先）。
	QueueSize int
	// BatchSize 是每次 flush 的最大事件数。
	BatchSize int
	// FlushEvery 是两次 flush 之间的最长间隔。
	FlushEvery time.Duration
	// TouchThrottle 是同一 Key 的 last_used_at 最小写入间隔。
	TouchThrottle time.Duration
}

// DefaultProviderKeyStatsOptions 返回合理的默认值。
func DefaultProviderKeyStatsOptions() ProviderKeyStatsOptions {
	return ProviderKeyStatsOptions{
		QueueSize:     1024,
		BatchSize:     32,
		FlushEvery:    5 * time.Second,
		TouchThrottle: time.Minute,
	}
}

type keyStatsEvent struct {
	// providerID 仅在 disable 事件里使用：落库时据此检查供应商是否已无可用 Key。
	providerID string
	keyID      string
	reason     string
	failure    bool
	disable    bool
}

// ProviderKeyStatsRecorder 以「异步 + 批量」方式写入凭证健康状态
// （last_used_at / failure_count / 自动停用），实现 gateway.ProviderKeyStats。
//
// 数据面热路径只做内存标记（毫秒级生效），落库交给本 recorder，绝不阻塞转发。
type ProviderKeyStatsRecorder struct {
	writer *Store
	logger slogLogger
	opts   ProviderKeyStatsOptions
	ch     chan keyStatsEvent
	done   chan struct{}

	mu        sync.Mutex
	lastTouch map[string]time.Time
	// onDisabled 在"包含自动停用的批次成功落库"后被异步调用一次，
	// 由 main 用来重建配置快照（让 /v1/models、管理面视图收敛）。
	onDisabled func()

	written atomic.Int64
	dropped atomic.Int64
}

// NewProviderKeyStatsRecorder 创建 recorder 但不启动后台协程（需调用 Start）。
func NewProviderKeyStatsRecorder(s *Store, logger slogLogger, opts ProviderKeyStatsOptions) *ProviderKeyStatsRecorder {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 128
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = 30 * time.Second
	}
	if opts.TouchThrottle <= 0 {
		opts.TouchThrottle = time.Minute
	}
	if logger == nil {
		logger = noopLogger{}
	}
	return &ProviderKeyStatsRecorder{
		writer:    s,
		logger:    logger,
		opts:      opts,
		ch:        make(chan keyStatsEvent, opts.QueueSize),
		done:      make(chan struct{}),
		lastTouch: make(map[string]time.Time),
	}
}

// TouchProviderKey 记录一次成功使用；同一 Key 在 TouchThrottle 内只入队一次。
func (r *ProviderKeyStatsRecorder) TouchProviderKey(keyID string) {
	if keyID == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if last, ok := r.lastTouch[keyID]; ok && now.Sub(last) < r.opts.TouchThrottle {
		r.mu.Unlock()
		return
	}
	r.lastTouch[keyID] = now
	r.mu.Unlock()
	r.enqueue(keyStatsEvent{keyID: keyID})
}

// RecordProviderKeyFailure 记录一次凭证失败（不改变启用状态）。
func (r *ProviderKeyStatsRecorder) RecordProviderKeyFailure(keyID, reason string) {
	if keyID == "" {
		return
	}
	r.enqueue(keyStatsEvent{keyID: keyID, reason: reason, failure: true})
}

// DisableProviderKey 记录一次「自动停用」。
func (r *ProviderKeyStatsRecorder) DisableProviderKey(providerID, keyID, reason string) {
	if keyID == "" {
		return
	}
	r.enqueue(keyStatsEvent{providerID: providerID, keyID: keyID, reason: reason, failure: true, disable: true})
}

// SetOnDisabled 注册自动停用落库后的回调（必须是非阻塞的）。
func (r *ProviderKeyStatsRecorder) SetOnDisabled(fn func()) {
	r.mu.Lock()
	r.onDisabled = fn
	r.mu.Unlock()
}

func (r *ProviderKeyStatsRecorder) enqueue(e keyStatsEvent) {
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
	}
}

// Start 启动后台批量写协程；ctx 取消时把剩余批次刷完再退出。
func (r *ProviderKeyStatsRecorder) Start(ctx context.Context) {
	go r.loop(ctx)
}

// Wait 等待写协程退出（关闭流程使用）。
func (r *ProviderKeyStatsRecorder) Wait(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stats 返回累计写入 / 丢弃的事件数。
func (r *ProviderKeyStatsRecorder) Stats() (written, dropped int64) {
	return r.written.Load(), r.dropped.Load()
}

func (r *ProviderKeyStatsRecorder) loop(ctx context.Context) {
	defer close(r.done)

	ticker := time.NewTicker(r.opts.FlushEvery)
	defer ticker.Stop()

	batch := make([]keyStatsEvent, 0, r.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.flushBatch(ctx, batch); err != nil {
			r.logger.Warn("凭证健康状态写入失败，本批已计入丢弃", "count", len(batch), "err", err)
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
					if len(batch) >= r.opts.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case <-ticker.C:
			flush()
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.opts.BatchSize {
				flush()
			}
		}
	}
}

func (r *ProviderKeyStatsRecorder) flushBatch(ctx context.Context, batch []keyStatsEvent) error {
	if len(batch) == 0 {
		return nil
	}
	hadDisable := false
	for _, e := range batch {
		if e.disable {
			hadDisable = true
			break
		}
	}
	tx, err := r.writer.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	touched := make(map[string]bool, len(batch))
	// 本批次里被自动停用的 Key 所属供应商：本批的 Key 状态全部落库后，
	// 再逐个检查「是否已没有任何启用的 Key」，是则一并停用供应商。
	disabledProviders := make(map[string]bool, 2)
	for _, e := range batch {
		switch {
		case e.disable:
			if _, err := tx.ExecContext(ctx,
				`UPDATE provider_keys SET enabled = 0, auto_disabled = 1, disabled_reason = ?,
				   failure_count = failure_count + 1, last_failure_at = ?, last_error = ?
				 WHERE id = ? AND enabled = 1`,
				truncate(e.reason, 200), now, truncate(e.reason, 200), e.keyID); err != nil {
				return err
			}
			if e.providerID != "" {
				disabledProviders[e.providerID] = true
			}
		case e.failure:
			if _, err := tx.ExecContext(ctx,
				`UPDATE provider_keys SET failure_count = failure_count + 1,
				   last_failure_at = ?, last_error = ?
				 WHERE id = ?`,
				now, truncate(e.reason, 200), e.keyID); err != nil {
				return err
			}
		default:
			if touched[e.keyID] {
				continue
			}
			touched[e.keyID] = true
			if _, err := tx.ExecContext(ctx,
				`UPDATE provider_keys SET last_used_at = ? WHERE id = ?`, now, e.keyID); err != nil {
				return err
			}
		}
	}
	for providerID := range disabledProviders {
		stopped, err := disableProviderIfExhaustedTx(ctx, tx, providerID)
		if err != nil {
			return err
		}
		if stopped {
			r.logger.Warn("供应商的全部 API Key 已不可用，已联动停用该供应商", "provider_id", providerID)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if hadDisable {
		r.mu.Lock()
		fn := r.onDisabled
		r.mu.Unlock()
		if fn != nil {
			go fn()
		}
	}
	return nil
}
