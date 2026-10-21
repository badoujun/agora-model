package store

import (
	"context"
	"testing"
)

// TestActiveGatewayKeyPicksEarliestEnabled 覆盖「默认网关 Key」的固化语义：
// 多个 Key 同时启用时固定取 created_at 最早的一个；它停用后顺延到下一个。
// 同时验证 GatewayKeyInfo 的 Note / DeletedAt / Revealable 三个新字段。
func TestActiveGatewayKeyPicksEarliestEnabled(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO gateway_keys (id, name, note, key_hash, key_hint, enabled, created_at, updated_at)
		 VALUES ('gk_older', '早', '备用', 'h-older', 'gw-****older', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z'),
		        ('gk_newer', '晚', '', 'h-newer', 'gw-****newer', 1, '2024-02-01T00:00:00Z', '2024-02-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	info, err := st.ActiveGatewayKey(ctx)
	if err != nil {
		t.Fatalf("ActiveGatewayKey: %v", err)
	}
	if info.ID != "gk_older" {
		t.Fatalf("默认 Key 应为最早创建者，得到 %q", info.ID)
	}
	if info.Note != "备用" {
		t.Errorf("Note = %q，期望 备用", info.Note)
	}
	if info.Revealable {
		t.Error("只保存哈希的记录不应标记为可查看明文")
	}

	// 停用最早的 → 顺延到下一个仍启用的
	if _, err := st.db.ExecContext(ctx, `UPDATE gateway_keys SET enabled = 0 WHERE id = 'gk_older'`); err != nil {
		t.Fatal(err)
	}
	info, err = st.ActiveGatewayKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "gk_newer" {
		t.Fatalf("停用最早者后应顺延到下一个启用 Key，得到 %q", info.ID)
	}

	// 全部停用 → 没有默认 Key
	if _, err := st.db.ExecContext(ctx, `UPDATE gateway_keys SET enabled = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActiveGatewayKey(ctx); err == nil {
		t.Fatal("没有任何启用的 Key 时应返回错误")
	}

	// 软删除的记录：默认列表排除，includeDeleted 时返回并带 DeletedAt
	if _, err := st.db.ExecContext(ctx,
		`UPDATE gateway_keys SET deleted_at = '2024-03-01T00:00:00Z' WHERE id = 'gk_newer'`); err != nil {
		t.Fatal(err)
	}
	live, err := st.ListGatewayKeys(ctx, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "gk_older" {
		t.Fatalf("默认列表应排除软删除记录，得到 %v", live)
	}
	all, err := st.ListGatewayKeys(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("includeDeleted 应返回两条，得到 %v", all)
	}
	if all[1].DeletedAt.IsZero() {
		t.Errorf("软删除记录应带 DeletedAt: %v", all[1])
	}
}
