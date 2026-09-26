package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agora-model/internal/logging"
)

// LogFilter 描述日志查询条件。
type LogFilter struct {
	StatusMin  int
	StatusMax  int
	Model      string
	ProviderID string
	FailedOnly bool
	From       time.Time
	To         time.Time
	Limit      int
	Offset     int
}

// InsertLogs 批量写入请求日志（单事务，供异步 Recorder 调用）。
func (s *Store) InsertLogs(ctx context.Context, entries []logging.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入日志开启事务失败: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO logs (ts, request_id, inbound_protocol, model, provider_id, upstream_url,
		                   status_code, latency_ms, first_byte_ms, stream, error_msg, client_ip)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("准备日志语句失败: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		ts := e.TS
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		stream := 0
		if e.Stream {
			stream = 1
		}
		if _, err := stmt.ExecContext(ctx,
			ts.UTC().Format(time.RFC3339Nano), e.RequestID, e.InboundProtocol,
			truncate(e.Model, 256), e.ProviderID, truncate(e.UpstreamURL, 512),
			e.StatusCode, e.LatencyMS, e.FirstByteMS, stream,
			logging.Redact(e.ErrorMsg, 1024), e.ClientIP,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("写入日志失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交日志失败: %w", err)
	}
	return nil
}

// QueryLogs 按条件查询日志（时间倒序）。
func (s *Store) QueryLogs(ctx context.Context, f LogFilter) ([]logging.Entry, error) {
	var (
		where []string
		args  []any
	)
	if f.StatusMin > 0 {
		where = append(where, "status_code >= ?")
		args = append(args, f.StatusMin)
	}
	if f.StatusMax > 0 {
		where = append(where, "status_code <= ?")
		args = append(args, f.StatusMax)
	}
	if f.FailedOnly {
		where = append(where, "status_code >= 400")
	}
	if f.Model != "" {
		where = append(where, "model = ?")
		args = append(args, f.Model)
	}
	if f.ProviderID != "" {
		where = append(where, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if !f.From.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339Nano))
	}
	if !f.To.IsZero() {
		where = append(where, "ts <= ?")
		args = append(args, f.To.UTC().Format(time.RFC3339Nano))
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ts, request_id, inbound_protocol, model, provider_id, upstream_url,
	             status_code, latency_ms, first_byte_ms, stream, error_msg, client_ip
	      FROM logs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询日志失败: %w", err)
	}
	defer rows.Close()

	var out []logging.Entry
	for rows.Next() {
		var (
			entry   logging.Entry
			ts      string
			stream  int
			firstMS int64
		)
		if err := rows.Scan(&ts, &entry.RequestID, &entry.InboundProtocol, &entry.Model,
			&entry.ProviderID, &entry.UpstreamURL, &entry.StatusCode, &entry.LatencyMS,
			&firstMS, &stream, &entry.ErrorMsg, &entry.ClientIP); err != nil {
			return nil, err
		}
		entry.TS = parseTime(ts)
		entry.FirstByteMS = firstMS
		entry.Stream = stream != 0
		out = append(out, entry)
	}
	return out, rows.Err()
}

// CountLogs 返回日志总条数。
func (s *Store) CountLogs(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// PruneLogs 仅保留最近 keep 条日志，返回删除条数（P2：防止 SQLite 无界增长）。
func (s *Store) PruneLogs(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT ?)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}
