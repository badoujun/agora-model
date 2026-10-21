package store

import (
	"context"
	"testing"
	"time"

	"agora-model/internal/logging"
)

// TestLogsFilterByGatewayKey 覆盖日志按网关 Key 过滤、展示名回填与筛选后计数。
func TestLogsFilterByGatewayKey(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO gateway_keys (id, name, note, key_hash, key_hint, enabled, created_at, updated_at, deleted_at)
		 VALUES ('gk_a', 'Claude Code', '张三', 'h-a', 'gw-****a', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z', NULL),
		        ('gk_b', 'Cursor', '', 'h-b', 'gw-****b', 0, '2024-01-02T00:00:00Z', '2024-01-02T00:00:00Z', '2024-02-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	entries := []logging.Entry{
		{TS: now, RequestID: "r1", InboundProtocol: "openai", Model: "gpt-4o", ProviderID: "p", StatusCode: 200, GatewayKeyID: "gk_a"},
		{TS: now, RequestID: "r2", InboundProtocol: "openai", Model: "gpt-4o", ProviderID: "p", StatusCode: 500, GatewayKeyID: "gk_a"},
		{TS: now, RequestID: "r3", InboundProtocol: "openai", Model: "gpt-4o", ProviderID: "p", StatusCode: 200, GatewayKeyID: "gk_b"},
		{TS: now, RequestID: "r4", InboundProtocol: "openai", Model: "gpt-4o", ProviderID: "p", StatusCode: 200}, // 历史无归属行
	}
	if err := st.InsertLogs(ctx, entries); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	// 按 Key 过滤：只返回该 Key 的行
	got, err := st.QueryLogs(ctx, LogFilter{GatewayKeyID: "gk_a"})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("gk_a 应有 2 条，得到 %d", len(got))
	}
	for _, e := range got {
		if e.GatewayKeyID != "gk_a" {
			t.Errorf("过滤结果混入了其他 Key: %+v", e)
		}
		// 展示名列由 join 回填：别名 + 备注
		if e.GatewayKeyName != "Claude Code（张三）" {
			t.Errorf("展示名 = %q，期望 Claude Code（张三）", e.GatewayKeyName)
		}
	}

	// 已软删除的 Key 也必须能显示名称（历史可读）
	got, err = st.QueryLogs(ctx, LogFilter{GatewayKeyID: "gk_b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GatewayKeyName != "Cursor" {
		t.Fatalf("软删除 Key 的名称应可显示: %+v", got)
	}

	// 不过滤时包含无归属的历史行，且其展示名为空
	all, err := st.QueryLogs(ctx, LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("不过滤应有 4 条，得到 %d", len(all))
	}
	for _, e := range all {
		if e.RequestID == "r4" && e.GatewayKeyName != "" {
			t.Errorf("无归属的历史行不应有展示名: %+v", e)
		}
	}

	// 计数必须跟随筛选条件（否则分页会翻到空页）
	for _, tc := range []struct {
		filter LogFilter
		want   int64
	}{
		{LogFilter{}, 4},
		{LogFilter{GatewayKeyID: "gk_a"}, 2},
		{LogFilter{GatewayKeyID: "gk_b"}, 1},
		{LogFilter{GatewayKeyID: "gk_a", FailedOnly: true}, 1},
		{LogFilter{GatewayKeyID: "gk_missing"}, 0},
	} {
		n, err := st.CountLogsFiltered(ctx, tc.filter)
		if err != nil {
			t.Fatalf("CountLogsFiltered: %v", err)
		}
		if n != tc.want {
			t.Errorf("筛选 %+v 计数 = %d，期望 %d", tc.filter, n, tc.want)
		}
	}

	// 全表计数不受筛选影响（/api/health 使用）
	if n, err := st.CountLogs(ctx); err != nil || n != 4 {
		t.Errorf("CountLogs = %d (%v)，期望 4", n, err)
	}
}
