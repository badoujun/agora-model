package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// 纯 Go 的 SQLite 驱动：无 CGO，可交叉编译（ADR-001 跨平台基线）。
	_ "modernc.org/sqlite"
)

// Store 是 SQLite 持久化层（WAL 模式，单机单文件）。
type Store struct {
	db *sql.DB
}

// 迁移定义。新增迁移时追加到切片末尾，切勿修改已发布的条目。
var migrations = []migration{
	{version: 1, name: "init", statements: schemaV1},
	{version: 2, name: "provider_fetch_status", statements: schemaV2},
	{version: 3, name: "single_protocol_model_selection", statements: schemaV3},
}

type migration struct {
	version    int
	name       string
	statements []string
}

// Open 打开（必要时创建）数据库、设置 pragma 并应用迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("数据库路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}

	// 重要：DSN 参数不要用 url.Values.Encode()，它会转义括号导致 pragma 失效。
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 读连接可多开（WAL 支持并发读）；写入集中在少数路径上，靠 busy_timeout 兜底。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB 暴露底层句柄（供测试与后续模块使用）。
func (s *Store) DB() *sql.DB { return s.db }

// migrate 按版本顺序应用未执行的迁移（幂等）。
func (s *Store) migrate(ctx context.Context) error {
	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`
	if _, err := s.db.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("创建 schema_migrations 失败: %w", err)
	}

	applied := make(map[int]bool)
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("读取迁移记录失败: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("迁移 %d 开启事务失败: %w", m.version, err)
		}
		for _, stmt := range m.statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("迁移 %d (%s) 执行失败: %w", m.version, m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("记录迁移 %d 失败: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 %d 失败: %w", m.version, err)
		}
	}
	return nil
}

// schemaV1 对应 docs/DESIGN.md §4.1 的数据模型。
var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS providers (
		id                          TEXT    PRIMARY KEY,
		name                        TEXT    NOT NULL,
		openai_base_url             TEXT    NOT NULL DEFAULT '',
		anthropic_base_url          TEXT    NOT NULL DEFAULT '',
		openai_endpoint_override    TEXT    NOT NULL DEFAULT '',
		anthropic_endpoint_override TEXT    NOT NULL DEFAULT '',
		api_key_cipher              BLOB    NOT NULL,
		api_key_hint                TEXT    NOT NULL DEFAULT '',
		models_manual_json          TEXT    NOT NULL DEFAULT '[]',
		models_excluded_json        TEXT    NOT NULL DEFAULT '[]',
		auto_fetch_models           INTEGER NOT NULL DEFAULT 1,
		priority                    INTEGER NOT NULL DEFAULT 100,
		timeout_seconds             INTEGER NOT NULL DEFAULT 120,
		extra_headers_json          TEXT    NOT NULL DEFAULT '{}',
		extra_body_json             TEXT    NOT NULL DEFAULT '{}',
		allow_internal              INTEGER NOT NULL DEFAULT 0,
		enabled                     INTEGER NOT NULL DEFAULT 1,
		created_at                  TEXT    NOT NULL,
		updated_at                  TEXT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS gateway_keys (
		id           TEXT    PRIMARY KEY,
		name         TEXT    NOT NULL DEFAULT 'default',
		key_hash     TEXT    NOT NULL UNIQUE,
		key_hint     TEXT    NOT NULL,
		enabled      INTEGER NOT NULL DEFAULT 1,
		created_at   TEXT    NOT NULL,
		last_used_at TEXT,
		revoked_at   TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS model_cache (
		provider_id TEXT NOT NULL,
		model_id    TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT 'auto',
		fetched_at  TEXT NOT NULL,
		PRIMARY KEY (provider_id, model_id)
	)`,
	`CREATE TABLE IF NOT EXISTS logs (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		ts               TEXT    NOT NULL,
		request_id       TEXT    NOT NULL,
		inbound_protocol TEXT    NOT NULL,
		model            TEXT    NOT NULL DEFAULT '',
		provider_id      TEXT    NOT NULL DEFAULT '',
		upstream_url     TEXT    NOT NULL DEFAULT '',
		status_code      INTEGER NOT NULL,
		latency_ms       INTEGER NOT NULL,
		first_byte_ms    INTEGER NOT NULL DEFAULT 0,
		stream           INTEGER NOT NULL DEFAULT 0,
		error_msg        TEXT    NOT NULL DEFAULT '',
		client_ip        TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_ts     ON logs(ts DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status_code)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_model  ON logs(model)`,
	`CREATE INDEX IF NOT EXISTS idx_cache_model ON model_cache(model_id)`,
}

// schemaV2 为 providers 增加模型拉取状态列（Phase 3 · 模型聚合）。
var schemaV2 = []string{
	`ALTER TABLE providers ADD COLUMN last_fetch_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE providers ADD COLUMN last_fetch_error TEXT NOT NULL DEFAULT ''`,
}

// schemaV3 把供应商收敛为单一 OpenAI 兼容协议，并以「勾选模型 + 别名」取代
// 手动模型 / 排除模型 / 优先级 / 自动拉取开关。
//
// 旧版的手动模型列表继续有效（迁移为已勾选模型）；自动拉取到的模型需要重新勾选。
var schemaV3 = []string{
	`ALTER TABLE providers ADD COLUMN models_selected_json TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE providers ADD COLUMN models_alias_json TEXT NOT NULL DEFAULT '{}'`,
	`UPDATE providers SET models_selected_json = models_manual_json
		WHERE models_manual_json <> '' AND models_manual_json <> '[]'`,
	`ALTER TABLE providers DROP COLUMN anthropic_base_url`,
	`ALTER TABLE providers DROP COLUMN anthropic_endpoint_override`,
	`ALTER TABLE providers DROP COLUMN models_manual_json`,
	`ALTER TABLE providers DROP COLUMN models_excluded_json`,
	`ALTER TABLE providers DROP COLUMN auto_fetch_models`,
	`ALTER TABLE providers DROP COLUMN priority`,
}
