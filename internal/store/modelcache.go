package store

import (
	"context"
	"fmt"
	"time"
)

// ReplaceModelCache 用新的自动拉取结果替换某供应商的模型缓存（事务内完成），
// 并同步更新 providers.last_fetch_at 与清空 last_fetch_error。
func (s *Store) ReplaceModelCache(ctx context.Context, providerID string, models []string, fetchedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("更新模型缓存开启事务失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM model_cache WHERE provider_id = ? AND source = 'auto'`, providerID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("清理模型缓存失败: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO model_cache (provider_id, model_id, source, fetched_at) VALUES (?, ?, 'auto', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备模型缓存语句失败: %w", err)
	}
	defer stmt.Close()

	ts := fetchedAt.UTC().Format(time.RFC3339)
	for _, m := range models {
		if m == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, providerID, m, ts); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入模型缓存失败: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE providers SET last_fetch_at = ?, last_fetch_error = '' WHERE id = ?`, ts, providerID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("更新拉取时间失败: %w", err)
	}
	return tx.Commit()
}

// LoadAllModelCache 一次读出全部供应商的自动拉取缓存（构建配置快照时使用）。
func (s *Store) LoadAllModelCache(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_id, model_id FROM model_cache WHERE source = 'auto' ORDER BY provider_id, model_id`)
	if err != nil {
		return nil, fmt.Errorf("查询模型缓存失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]string)
	for rows.Next() {
		var providerID, modelID string
		if err := rows.Scan(&providerID, &modelID); err != nil {
			return nil, err
		}
		out[providerID] = append(out[providerID], modelID)
	}
	return out, rows.Err()
}

// LoadModelCache 读取单个供应商的模型缓存与最后成功拉取时间。
func (s *Store) LoadModelCache(ctx context.Context, providerID string) ([]string, time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_id, fetched_at FROM model_cache WHERE provider_id = ? AND source = 'auto' ORDER BY model_id`,
		providerID)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	var (
		models  []string
		fetched time.Time
	)
	for rows.Next() {
		var modelID, ts string
		if err := rows.Scan(&modelID, &ts); err != nil {
			return nil, time.Time{}, err
		}
		models = append(models, modelID)
		if parsed := parseTime(ts); parsed.After(fetched) {
			fetched = parsed
		}
	}
	return models, fetched, rows.Err()
}

// SetProviderFetchError 记录拉取失败原因（不清空既有缓存，只标记陈旧）。
func (s *Store) SetProviderFetchError(ctx context.Context, providerID, message string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE providers SET last_fetch_error = ?, last_fetch_at = ? WHERE id = ?`,
		truncate(message, 512), time.Now().UTC().Format(time.RFC3339), providerID)
	return err
}
