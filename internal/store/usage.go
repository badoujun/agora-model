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

// KeyDailyBucket 是一天内一个 API Key 的 token 累计（按 Key 维度，供控制台展示）。
//
// 与 model_usage_daily 的关系：后者是「供应商 + 模型」维度（用于使用统计页），
// 本表是「供应商 + Key」维度（用于供应商编辑页展示每个 Key 的消耗），两者互不影响。
type KeyDailyBucket struct {
	Date              string // YYYY-MM-DD (UTC)
	KeyID             string
	ProviderID        string
	Requests          int
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// KeyUsageTotals 是单个 Key 在若干时间窗口内的用量汇总（token 为输入+输出）。
type KeyUsageTotals struct {
	TodayTokens     int64
	Week7Tokens     int64
	Month30Tokens   int64
	Month30Requests int64
}

// UpsertProviderKeyUsageDaily 合并按 Key 的日用量（UPSERT，原子累加）。
func (s *Store) UpsertProviderKeyUsageDaily(ctx context.Context, buckets []KeyDailyBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入按 Key 用量开启事务失败: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO provider_key_usage_daily
		 (date, key_id, provider_id, requests, input_tokens, cached_input_tokens, output_tokens)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(date, key_id) DO UPDATE SET
		   requests            = requests            + excluded.requests,
		   input_tokens        = input_tokens        + excluded.input_tokens,
		   cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
		   output_tokens       = output_tokens       + excluded.output_tokens`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备按 Key 用量 UPSERT 语句失败: %w", err)
	}
	defer stmt.Close()

	for _, b := range buckets {
		if b.KeyID == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx,
			b.Date, b.KeyID, b.ProviderID,
			b.Requests, b.InputTokens, b.CachedInputTokens, b.OutputTokens,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入按 Key 用量失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交按 Key 用量失败: %w", err)
	}
	return nil
}

// ProviderKeyUsageTotals 返回某供应商每个 Key 的今日 / 近 7 天 / 近 30 天用量。
//
// 只查最近 30 天的日行（数量 = 天数 × Key 数，很小），在内存里折算窗口，
// 避免为三个窗口写三条 SQL。时间为 UTC 自然日，与现有统计口径一致。
func (s *Store) ProviderKeyUsageTotals(ctx context.Context, providerID string) (map[string]KeyUsageTotals, error) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -6).Format("2006-01-02")
	monthAgo := now.AddDate(0, 0, -29).Format("2006-01-02")

	rows, err := s.db.QueryContext(ctx,
		`SELECT date, key_id, requests, input_tokens, output_tokens
		 FROM provider_key_usage_daily
		 WHERE provider_id = ? AND date >= ?`,
		providerID, monthAgo)
	if err != nil {
		return nil, fmt.Errorf("查询按 Key 用量失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]KeyUsageTotals)
	for rows.Next() {
		var (
			date, keyID             string
			requests, input, output int64
		)
		if err := rows.Scan(&date, &keyID, &requests, &input, &output); err != nil {
			return nil, err
		}
		total := input + output
		t := out[keyID]
		t.Month30Tokens += total
		t.Month30Requests += requests
		if date >= weekAgo {
			t.Week7Tokens += total
		}
		if date == today {
			t.TodayTokens += total
		}
		out[keyID] = t
	}
	return out, rows.Err()
}
