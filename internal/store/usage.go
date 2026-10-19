package store

import (
	"context"
	"fmt"
	"time"
)

// MinuteBucket 是一分钟内一个 (provider, model) 的 token 累计。
//
// 由 UsageRecorder 在内存里按分钟聚合，每 30 秒 flush 一次；同事务
// UPSERT 到 model_usage_daily 后再 replace model_usage_minute 同主键行。
type MinuteBucket struct {
	Minute            time.Time // 按 .Format("2006-01-02T15:04") 截到分钟
	ProviderID        string
	Model             string
	Requests          int
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// DailyBucket 是一天内一个 (provider, model) 的 token 累计。
//
// 与 MinuteBucket 同样的字段，区别仅在聚合键（按天 vs 按分钟）。
type DailyBucket struct {
	Date              string // YYYY-MM-DD
	ProviderID        string
	Model             string
	Requests          int
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// minuteBucketKey 返回 model_usage_minute 主键的列值。
//
// minute 按 .UTC().Format("2006-01-02T15:04") 标准化为 UTC 分钟字符串，
// 跨时区 / 跨重启也能稳定合并到同一行。
func minuteBucketKey(b MinuteBucket) (minute string, providerID string, model string) {
	return b.Minute.UTC().Format("2006-01-02T15:04"), b.ProviderID, b.Model
}

// UpsertUsageMinute 合并同一分钟内同一 (provider, model) 的累计行；
// 单事务、可并发安全（SQLite UPSERT 原子）。
//
// 当 minute 比"今天"早（即 UTC 日期小于当前日期），本函数仍会写入，
// 但 UsageRecorder 的 PruneUsageMinute 会随后清理这些行——这样凌晨跨日
// 也能正确合并最近一分钟的累计。
func (s *Store) UpsertUsageMinute(ctx context.Context, buckets []MinuteBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入 usage 分钟桶开启事务失败: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO model_usage_minute
		 (minute, provider_id, model, requests, input_tokens, cached_input_tokens, output_tokens)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(minute, provider_id, model) DO UPDATE SET
		   requests           = requests           + excluded.requests,
		   input_tokens       = input_tokens       + excluded.input_tokens,
		   cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
		   output_tokens      = output_tokens      + excluded.output_tokens`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备 usage 分钟 UPSERT 语句失败: %w", err)
	}
	defer stmt.Close()

	for _, b := range buckets {
		minute, providerID, model := minuteBucketKey(b)
		if _, err := stmt.ExecContext(ctx,
			minute, providerID, model,
			b.Requests, b.InputTokens, b.CachedInputTokens, b.OutputTokens,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入 usage 分钟桶失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 usage 分钟桶失败: %w", err)
	}
	return nil
}

// UpsertUsageDaily 把当日分钟的累计"提升"到日聚合表（同时也会用于跨日
// 写入——同主键原子合并）。调用方通常在 UpsertUsageMinute 的同一事务
// 里一并调用，保证日聚合与分钟明细强一致。
func (s *Store) UpsertUsageDaily(ctx context.Context, buckets []DailyBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入 usage 日桶开启事务失败: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO model_usage_daily
		 (date, provider_id, model, requests, input_tokens, cached_input_tokens, output_tokens)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(date, provider_id, model) DO UPDATE SET
		   requests           = requests           + excluded.requests,
		   input_tokens       = input_tokens       + excluded.input_tokens,
		   cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
		   output_tokens      = output_tokens      + excluded.output_tokens`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备 usage 日 UPSERT 语句失败: %w", err)
	}
	defer stmt.Close()

	for _, b := range buckets {
		if _, err := stmt.ExecContext(ctx,
			b.Date, b.ProviderID, b.Model,
			b.Requests, b.InputTokens, b.CachedInputTokens, b.OutputTokens,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入 usage 日桶失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 usage 日桶失败: %w", err)
	}
	return nil
}

// PruneUsageMinute 删除 minute 早于 before 的所有行，返回删除条数。
//
// 设计：minute 表只保留当日；超过 24h 的分钟明细没有查询意义（页面只查
// 日聚合 + 当日分钟明细）。UTC "今天零点" 是天然截止线——before 应当传
// `time.Now().UTC().Truncate(24*time.Hour)` 之类。
func (s *Store) PruneUsageMinute(ctx context.Context, before time.Time) (int64, error) {
	cutoff := before.UTC().Format("2006-01-02T15:04")
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM model_usage_minute WHERE minute < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理 usage 分钟桶失败: %w", err)
	}
	return res.RowsAffected()
}
