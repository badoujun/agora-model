package store

import (
	"context"
	"testing"
)

// TestGatewayKeyScopeCRUD 覆盖 gateway_key_models 表的读写：
// 全量替换语义、去重、空集合表示「不限制」、以及按 Key 批量装载。
func TestGatewayKeyScopeCRUD(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// 建立两个网关 Key（外键非约束，但语义上 scope 属于某个 Key）
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO gateway_keys (id, name, key_hash, key_hint, enabled, created_at, updated_at)
		 VALUES ('gk_a', 'A', 'h-a', 'gw-****a', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z'),
		        ('gk_b', 'B', 'h-b', 'gw-****b', 1, '2024-01-02T00:00:00Z', '2024-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	// 空范围：没有行 → 不限制
	rows, err := st.ListGatewayKeyScopesFor(ctx, "gk_a")
	if err != nil {
		t.Fatalf("ListGatewayKeyScopesFor: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("未配置范围的 Key 应返回空切片，得到 %v", rows)
	}

	// 写入（含重复项，应被去重）
	if err := st.SetGatewayKeyScopes(ctx, "gk_a", []GatewayKeyScopeRow{
		{ProviderID: "p1", Model: "gpt-4o"},
		{ProviderID: "p1", Model: "gpt-4o"},
		{ProviderID: "p1", Model: "gpt-4o-mini"},
		{ProviderID: "p2", Model: "claude"},
		{ProviderID: "", Model: "ignored"},
	}); err != nil {
		t.Fatalf("SetGatewayKeyScopes: %v", err)
	}
	rows, err = st.ListGatewayKeyScopesFor(ctx, "gk_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("去重与空项过滤后应有 3 行，得到 %v", rows)
	}

	// 全量替换为单行
	if err := st.SetGatewayKeyScopes(ctx, "gk_a", []GatewayKeyScopeRow{{ProviderID: "p3", Model: "m3"}}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListGatewayKeyScopesFor(ctx, "gk_a")
	if len(rows) != 1 || rows[0].ProviderID != "p3" || rows[0].Model != "m3" {
		t.Fatalf("全量替换后 = %v", rows)
	}

	// 批量装载：gk_b 不限制，不应出现在结果里
	all, err := st.ListAllGatewayKeyScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !all["gk_a"].Allows("p3", "m3") {
		t.Fatalf("ListAllGatewayKeyScopes = %v", all)
	}
	if _, ok := all["gk_b"]; ok {
		t.Errorf("没有范围的 Key 不应出现在装载结果里: %v", all)
	}

	// 清空 = 回到不限制
	if err := st.SetGatewayKeyScopes(ctx, "gk_a", nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListGatewayKeyScopesFor(ctx, "gk_a")
	if len(rows) != 0 {
		t.Fatalf("清空后应为空，得到 %v", rows)
	}
}
