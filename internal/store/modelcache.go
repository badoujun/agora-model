package store

import (
	"context"
	"fmt"
	"time"
)

// ReplaceCandidateModels 用最近一次拉取结果替换某供应商的候选模型缓存（事务内完成），
// 并同步更新 providers.last_fetch_at 与清空 last_fetch_error。
//
// 候选列表只用于供应商编辑页的勾选，不影响路由：真正生效的模型是
// providers.models_selected_json（见 SaveProvider）。
func (s *Store) ReplaceCandidateModels(ctx context.Context, providerID string, candidates []string, fetchedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("更新模型候选缓存开启事务失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM model_cache WHERE provider_id = ?`, providerID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("清理模型候选缓存失败: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO model_cache (provider_id, model_id, source, fetched_at) VALUES (?, ?, 'auto', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备模型候选缓存语句失败: %w", err)
	}
	defer stmt.Close()

	ts := fetchedAt.UTC().Format(time.RFC3339)
	for _, m := range candidates {
		if m == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, providerID, m, ts); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入模型候选缓存失败: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE providers SET last_fetch_at = ?, last_fetch_error = '' WHERE id = ?`, ts, providerID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("更新拉取时间失败: %w", err)
	}
	return tx.Commit()
}

// LoadCandidateModels 读取单个供应商的候选模型（最近一次拉取结果，按名称排序）。
func (s *Store) LoadCandidateModels(ctx context.Context, providerID string) ([]string, time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_id, fetched_at FROM model_cache WHERE provider_id = ? ORDER BY model_id`, providerID)
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

// SetProviderFetchError 记录拉取失败原因（不清空既有候选缓存，只标记陈旧）。
func (s *Store) SetProviderFetchError(ctx context.Context, providerID, message string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE providers SET last_fetch_error = ?, last_fetch_at = ? WHERE id = ?`,
		truncate(message, 512), time.Now().UTC().Format(time.RFC3339), providerID)
	return err
}
