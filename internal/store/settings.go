package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SettingModelRefreshSeconds 是模型聚合刷新间隔（秒）的设置键。
const SettingModelRefreshSeconds = "model_refresh_seconds"

// SettingLogSuccess 控制是否记录成功的请求（默认关闭以避免写放大）。
const SettingLogSuccess = "log_success"

// GetSetting 读取一个设置值。
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取设置 %s 失败: %w", key, err)
	}
	return value, true, nil
}

// SetSetting 写入或覆盖一个设置值。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入设置 %s 失败: %w", key, err)
	}
	return nil
}

// AllSettings 返回全部设置。
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
