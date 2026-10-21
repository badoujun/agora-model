package store

import (
	"context"
	"testing"

	"agora-model/internal/config"
)

// TestGatewayKeyScopesCascadeOnProviderChange 覆盖范围行的级联清理：
//   - 删除供应商 → 该供应商下的范围行全部消失；
//   - 供应商取消勾选某个模型 → 只清掉该模型的范围行，其余保留。
//
// 否则设置页会显示「已勾选」，实际路由却永远命中不了。
func TestGatewayKeyScopesCascadeOnProviderChange(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	key, _, err := st.CreateGatewayKey(ctx, master, "受限", "", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []config.Provider{
		{ID: "p1", Name: "供应商1", OpenAIBaseURL: "https://a.example.com/v1", APIKey: "sk-a",
			Models: []string{"m1", "m2"}, AllowInternal: true},
		{ID: "p2", Name: "供应商2", OpenAIBaseURL: "https://b.example.com/v1", APIKey: "sk-b",
			Models: []string{"m3"}, AllowInternal: true},
	} {
		if _, err := st.SaveProvider(ctx, master, p); err != nil {
			t.Fatalf("SaveProvider(%s): %v", p.ID, err)
		}
	}

	if err := st.SetGatewayKeyScopes(ctx, key.ID, []GatewayKeyScopeRow{
		{ProviderID: "p1", Model: "m1"},
		{ProviderID: "p1", Model: "m2"},
		{ProviderID: "p2", Model: "m3"},
	}); err != nil {
		t.Fatal(err)
	}

	// 取消勾选 p1 的 m2：只应清掉 (p1, m2)
	if _, err := st.SaveProvider(ctx, master, config.Provider{
		ID: "p1", Name: "供应商1", OpenAIBaseURL: "https://a.example.com/v1", APIKey: "sk-a",
		Models: []string{"m1"}, AllowInternal: true,
	}); err != nil {
		t.Fatalf("再次保存 p1: %v", err)
	}
	rows, err := st.ListGatewayKeyScopesFor(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("取消勾选后应剩 2 行，得到 %v", rows)
	}
	for _, r := range rows {
		if r.ProviderID == "p1" && r.Model == "m2" {
			t.Fatal("已取消勾选的模型不应留在范围里")
		}
	}

	// 删除 p2：它的范围行应一并消失
	if err := st.DeleteProvider(ctx, "p2"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	rows, err = st.ListGatewayKeyScopesFor(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ProviderID != "p1" || rows[0].Model != "m1" {
		t.Fatalf("删除供应商后只应剩下 (p1,m1)，得到 %v", rows)
	}

	// 供应商整体停用不影响范围行（停用是可恢复的，范围应保留）
	disabled := false
	if _, err := st.SaveProvider(ctx, master, config.Provider{
		ID: "p1", Name: "供应商1", OpenAIBaseURL: "https://a.example.com/v1", APIKey: "sk-a",
		Models: []string{"m1"}, AllowInternal: true, Enabled: &disabled,
	}); err != nil {
		t.Fatalf("停用 p1: %v", err)
	}
	rows, _ = st.ListGatewayKeyScopesFor(ctx, key.ID)
	if len(rows) != 1 {
		t.Fatalf("整体停用供应商不应清理范围行，得到 %v", rows)
	}
}
