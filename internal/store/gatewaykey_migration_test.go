package store

import (
	"context"
	"testing"
)

// buildV6Schema 构造一个「升级前」的数据库（schema v1..v6），并在仍处于
// v6 结构时调用 seed 写入测试数据，最后删除 v7 迁移记录并关闭。
//
// 关键顺序：Open 会先把库迁到最新版，因此必须手工回落 v1..v6 结构；
// seed 必须在 Close 之前执行，否则下一次 Open 会把 v7 迁移跑掉、
// 表结构变成新版（缺列的旧 SQL 会直接失败）。
func buildV6Schema(t *testing.T, path string, seed func(st *Store)) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, table := range []string{
		"gateway_key_models", "providers", "gateway_keys", "model_cache", "logs", "settings",
		"model_usage_minute", "model_usage_daily", "provider_keys", "provider_key_usage_daily",
	} {
		if _, err := st.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	stmts := append(append(append(append([]string{}, schemaV1...), schemaV2...), schemaV3...), schemaV4...)
	stmts = append(stmts, schemaV5...)
	stmts = append(stmts, schemaV6...)
	for _, stmt := range stmts {
		if _, err := st.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("重建 v6 结构失败: %v（%s）", err, stmt)
		}
	}
	if seed != nil {
		seed(st)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 7`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// buildV6Database 构造一个「升级前」的数据库（schema v1..v6），并写入：
//   - 两个网关 Key：keyA 创建更早，keyB 更晚；
//   - 两行请求日志（没有网关 Key 归属）；
//   - 两行日用量（旧主键只有 date+provider+model）。
//
// 用于验证 v7 迁移把历史数据统一归到「最早创建的那个 Key」。
func buildV6Database(t *testing.T, path string) {
	t.Helper()
	buildV6Schema(t, path, func(st *Store) {
		ctx := context.Background()
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO gateway_keys (id, name, key_hash, key_hint, enabled, created_at)
			 VALUES ('gk_older', '早的 Key', 'hash-older', 'gw-****older', 1, '2024-01-01T00:00:00Z'),
			        ('gk_newer', '晚的 Key', 'hash-newer', 'gw-****newer', 1, '2024-02-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO logs (ts, request_id, inbound_protocol, model, provider_id, upstream_url,
				status_code, latency_ms, error_msg)
			 VALUES ('2024-01-05T00:00:00Z', 'r1', 'openai', 'gpt-4o', 'p1', 'http://up/v1', 200, 10, ''),
			        ('2024-01-06T00:00:00Z', 'r2', 'openai', 'gpt-4o-mini', 'p1', 'http://up/v1', 500, 20, 'boom')`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO model_usage_daily (date, provider_id, model, requests, input_tokens, cached_input_tokens, output_tokens)
			 VALUES ('2024-01-05', 'p1', 'gpt-4o', 2, 100, 0, 50),
			        ('2024-01-06', 'p1', 'gpt-4o-mini', 1, 10, 0, 5)`); err != nil {
			t.Fatal(err)
		}
	})
}

// TestMigrationV7AttributesHistoryToEarliestKey 覆盖「无归属的历史数据归到第一个 Key」：
// 迁移后旧日志与旧用量都指向 created_at 最早的 Key，而不是留空或报错。
func TestMigrationV7AttributesHistoryToEarliestKey(t *testing.T) {
	path := tempDBPath(t)
	buildV6Database(t, path)

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("迁移 v7 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	var logsCount int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE gateway_key_id = 'gk_older'`).Scan(&logsCount); err != nil {
		t.Fatal(err)
	}
	if logsCount != 2 {
		t.Errorf("历史日志应全部归到最早的 Key，命中 %d 行，期望 2", logsCount)
	}

	var usageCount int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_usage_daily WHERE gateway_key_id = 'gk_older'`).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 2 {
		t.Errorf("历史用量应全部归到最早的 Key，命中 %d 行，期望 2", usageCount)
	}

	// 旧列与新列都必须保留：迁移只加维度，不改数值。
	var requests, input, output int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT requests, input_tokens, output_tokens FROM model_usage_daily
		 WHERE date = '2024-01-05' AND gateway_key_id = 'gk_older' AND model = 'gpt-4o'`).
		Scan(&requests, &input, &output); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || input != 100 || output != 50 {
		t.Errorf("迁移后的用量数值 = (%d,%d,%d)，期望 (2,100,50)", requests, input, output)
	}

	// 新主键允许同一 (date, provider, model) 在不同网关 Key 下各存一行。
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO model_usage_daily (date, gateway_key_id, provider_id, model, requests)
		 VALUES ('2024-01-05', 'gk_newer', 'p1', 'gpt-4o', 1)`); err != nil {
		t.Fatalf("四列主键应允许按 Key 分行: %v", err)
	}
}

// TestMigrationV7WithoutAnyGatewayKey 覆盖「库里一个网关 Key 都没有」的升级场景：
// 历史日志与用量仍能迁移，归属回落为空串，而不是让子查询返回 NULL 报错。
func TestMigrationV7WithoutAnyGatewayKey(t *testing.T) {
	path := tempDBPath(t)
	buildV6Schema(t, path, func(st *Store) {
		ctx := context.Background()
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO logs (ts, request_id, inbound_protocol, model, provider_id, upstream_url, status_code, latency_ms, error_msg)
			 VALUES ('2024-01-05T00:00:00Z', 'r1', 'openai', 'gpt-4o', 'p1', 'http://up/v1', 200, 10, '')`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO model_usage_daily (date, provider_id, model, requests, input_tokens, output_tokens)
			 VALUES ('2024-01-05', 'p1', 'gpt-4o', 3, 30, 7)`); err != nil {
			t.Fatal(err)
		}
	})

	ctx := context.Background()
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("迁移 v7（无 Key）失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var logKey string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT gateway_key_id FROM logs WHERE request_id = 'r1'`).Scan(&logKey); err != nil {
		t.Fatal(err)
	}
	if logKey != "" {
		t.Errorf("没有网关 Key 时日志归属应为空串，得到 %q", logKey)
	}

	var requests int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT requests FROM model_usage_daily WHERE date = '2024-01-05' AND model = 'gpt-4o'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Errorf("无 Key 时用量行仍应完整搬迁，requests = %d，期望 3", requests)
	}
}
