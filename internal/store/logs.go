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
	// GatewayKeyID 只返回该网关 Key 发起的请求；空串表示不按 Key 过滤。
	GatewayKeyID string
	FailedOnly   bool
	From         time.Time
	To           time.Time
	Limit        int
	Offset       int
}

// logFilterWhere 把筛选条件编译为 WHERE 子句与参数（CountLogsFiltered 与
// QueryLogs 共用，保证「列表条数」与「总数」口径永远一致）。
func logFilterWhere(f LogFilter) ([]string, []any) {
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
		// 模型筛选：忽略大小写的模糊匹配（用户输入按字面匹配，转义 LIKE 通配符）
		where = append(where, `LOWER(model) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(f.Model))+"%")
	}
	if f.ProviderID != "" {
		where = append(where, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if f.GatewayKeyID != "" {
		where = append(where, "gateway_key_id = ?")
		args = append(args, f.GatewayKeyID)
	}
	if !f.From.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339Nano))
	}
	if !f.To.IsZero() {
		where = append(where, "ts <= ?")
		args = append(args, f.To.UTC().Format(time.RFC3339Nano))
	}
	return where, args
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
		`INSERT INTO logs (ts, request_id, inbound_protocol, model, provider_id, key_hint,
		                   gateway_key_id, upstream_url, status_code, latency_ms, first_byte_ms,
		                   stream, error_msg, client_ip)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
			truncate(e.Model, 256), e.ProviderID, truncate(e.KeyHint, 64), e.GatewayKeyID,
			truncate(e.UpstreamURL, 512),
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
//
// 左连接 gateway_keys 回填网关 Key 的展示名：已软删除的 Key 也保留名称，
// 否则历史日志会变成一串无意义的 id。展示名由 SQL 侧按「别名（备注）」拼接，
// 与 store.GatewayKeyNames 保持同一口径。
func (s *Store) QueryLogs(ctx context.Context, f LogFilter) ([]logging.Entry, error) {
	where, args := logFilterWhere(f)

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT l.ts, l.request_id, l.inbound_protocol, l.model, l.provider_id, l.key_hint,
	             l.gateway_key_id,
	             CASE
	                 WHEN COALESCE(g.name, '') = '' AND COALESCE(g.note, '') = '' THEN ''
	                 WHEN COALESCE(g.note, '') = '' THEN g.name
	                 WHEN COALESCE(g.name, '') = '' THEN g.note
	                 ELSE g.name || '（' || g.note || '）'
	             END,
	             l.upstream_url, l.status_code, l.latency_ms, l.first_byte_ms, l.stream,
	             l.error_msg, l.client_ip
	      FROM logs l LEFT JOIN gateway_keys g ON g.id = l.gateway_key_id`
	if len(where) > 0 {
		// 条件里引用的是未加别名的列名，加前缀会牵连调用方；这里直接用
		// 子查询把过滤放在 logs 上，避免与 join 列名歧义。
		q = `SELECT l.ts, l.request_id, l.inbound_protocol, l.model, l.provider_id, l.key_hint,
		             l.gateway_key_id,
		             CASE
		                 WHEN COALESCE(g.name, '') = '' AND COALESCE(g.note, '') = '' THEN ''
		                 WHEN COALESCE(g.note, '') = '' THEN g.name
		                 WHEN COALESCE(g.name, '') = '' THEN g.note
		                 ELSE g.name || '（' || g.note || '）'
		             END,
		             l.upstream_url, l.status_code, l.latency_ms, l.first_byte_ms, l.stream,
		             l.error_msg, l.client_ip
		      FROM (SELECT * FROM logs WHERE ` + strings.Join(where, " AND ") + `) l
		      LEFT JOIN gateway_keys g ON g.id = l.gateway_key_id`
	}
	q += " ORDER BY l.id DESC LIMIT ? OFFSET ?"
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
			&entry.ProviderID, &entry.KeyHint, &entry.GatewayKeyID, &entry.GatewayKeyName,
			&entry.UpstreamURL, &entry.StatusCode, &entry.LatencyMS,
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

// CountLogsFiltered 返回满足筛选条件的日志条数（与 QueryLogs 共用同一套条件）。
//
// 日志页的分页总数必须跟着筛选走：只按 Key 过滤却用全表总数，翻页会翻到空页。
func (s *Store) CountLogsFiltered(ctx context.Context, f LogFilter) (int64, error) {
	where, args := logFilterWhere(f)
	q := `SELECT COUNT(*) FROM logs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计日志条数失败: %w", err)
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

// escapeLike 转义 LIKE 模式中的通配符，使关键词按字面匹配。
func escapeLike(keyword string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(keyword)
}
