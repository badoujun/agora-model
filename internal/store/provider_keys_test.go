package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
)

// buildV5Database 构造一个"升级前"的数据库（schema v1..v5）并写入一条单凭证供应商。
func buildV5Database(t *testing.T, path string, master []byte) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, table := range []string{"providers", "gateway_keys", "model_cache", "logs", "settings",
		"model_usage_minute", "model_usage_daily", "provider_keys", "provider_key_usage_daily"} {
		if _, err := st.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	stmts := append(append(append(append([]string{}, schemaV1...), schemaV2...), schemaV3...), schemaV4...)
	stmts = append(stmts, schemaV5...)
	for _, stmt := range stmts {
		if _, err := st.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("重建 v5 结构失败: %v（%s）", err, stmt)
		}
	}
	// 旧版单凭证：AAD = provider.id
	cipherText, err := crypto.Encrypt(master, []byte("sk-legacy-secret"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO providers (id, name, openai_base_url, api_key_cipher, api_key_hint,
			models_selected_json, models_alias_json, created_at, updated_at)
		 VALUES ('legacy', '旧供应商', 'https://api.example.com/v1', ?, ?,
			'["m1"]', '{}', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		cipherText, crypto.Mask("sk-legacy-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 6`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationV6MovesSingleKeyToProviderKeys 覆盖升级路径：
// 旧库的单凭证被搬到 provider_keys，旧的凭证列消失，且明文仍可解出。
func TestMigrationV6MovesSingleKeyToProviderKeys(t *testing.T) {
	path := tempDBPath(t)
	master := make([]byte, crypto.KeySize)
	for i := range master {
		master[i] = byte(i + 1)
	}
	buildV5Database(t, path, master)

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("迁移 v6 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cols := tableColumns(t, st, "providers")
	for _, gone := range []string{"api_key_cipher", "api_key_hint"} {
		if cols[gone] {
			t.Errorf("providers.%s 应已被删除", gone)
		}
	}
	logCols := tableColumns(t, st, "logs")
	if !logCols["key_hint"] {
		t.Error("logs.key_hint 应被新增")
	}

	rec, err := st.GetProvider(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Keys) != 1 {
		t.Fatalf("应迁移为 1 个 Key，得到 %d", len(rec.Keys))
	}
	if rec.Keys[0].Label != "主 Key" || rec.Keys[0].KeyHint != crypto.Mask("sk-legacy-secret") {
		t.Errorf("迁移后的 Key = %+v", rec.Keys[0])
	}
	if !rec.Keys[0].Enabled || rec.Keys[0].AutoDisabled {
		t.Errorf("迁移后的 Key 应处于启用状态: %+v", rec.Keys[0])
	}

	providers, err := st.LoadProviders(context.Background(), master)
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(providers) != 1 || len(providers[0].APIKeys) != 1 {
		t.Fatalf("运行时配置 = %+v", providers)
	}
	if providers[0].APIKeys[0].Key != "sk-legacy-secret" {
		t.Errorf("迁移后应能解出原明文，得到 %q", providers[0].APIKeys[0].Key)
	}
}

// TestSaveProviderReplacesKeySet 覆盖「全量替换」语义：按 id 保留、改明文、新增、删除。
func TestSaveProviderReplacesKeySet(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()
	enabled := true

	p := sampleProvider("p1")
	p.APIKeys = []config.ProviderKey{
		{Label: "主账号", Key: "sk-main-1111", Enabled: &enabled},
		{Label: "备用", Key: "sk-backup-2222", Enabled: &enabled},
	}
	saved, err := st.SaveProvider(ctx, master, p)
	if err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	if len(saved.Keys) != 2 || saved.Keys[0].Label != "主账号" || saved.Keys[1].Position != 1 {
		t.Fatalf("顺序/位置未保持: %+v", saved.Keys)
	}
	mainID, backupID := saved.Keys[0].ID, saved.Keys[1].ID

	// 保留主账号（不传明文）+ 改备注、新增第三个、删除备用
	p.APIKeys = []config.ProviderKey{
		{ID: mainID, Label: "主账号-改名", Enabled: &enabled},
		{Label: "新Key", Key: "sk-new-3333", Enabled: &enabled},
	}
	updated, err := st.SaveProvider(ctx, master, p)
	if err != nil {
		t.Fatalf("第二次 SaveProvider: %v", err)
	}
	if len(updated.Keys) != 2 {
		t.Fatalf("Key 数 = %d", len(updated.Keys))
	}
	if updated.Keys[0].ID != mainID || updated.Keys[0].Label != "主账号-改名" {
		t.Errorf("既有 Key 应保留 id 并更新备注: %+v", updated.Keys[0])
	}
	if updated.Keys[0].KeyHint != crypto.Mask("sk-main-1111") {
		t.Errorf("未传明文时应沿用原密文/掩码: %+v", updated.Keys[0])
	}
	for _, k := range updated.Keys {
		if k.ID == backupID {
			t.Error("未出现在列表中的 Key 应被删除")
		}
	}

	// 新明文 + 保留的明文都能解出
	providers, err := st.LoadProviders(ctx, master)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].APIKeys) != 2 {
		t.Fatalf("运行时配置 = %+v", providers)
	}
	if providers[0].APIKeys[0].Key != "sk-main-1111" || providers[0].APIKeys[1].Key != "sk-new-3333" {
		t.Errorf("明文 = %+v", providers[0].APIKeys)
	}

	// 未知 id 应被拒绝
	_, err = st.SaveProvider(ctx, master, config.Provider{
		ID: "p1", Name: "供应商 p1", OpenAIBaseURL: "https://api.example.com/v1",
		AllowInternal: true,
		APIKeys:       []config.ProviderKey{{ID: "pk_unknown", Enabled: &enabled}},
	})
	if err == nil || !strings.Contains(err.Error(), "不属于该供应商") {
		t.Fatalf("未知 Key id 应被拒绝，得到 %v", err)
	}

	// 全部停用应被拒绝
	disabled := false
	_, err = st.SaveProvider(ctx, master, config.Provider{
		ID: "p1", Name: "供应商 p1", OpenAIBaseURL: "https://api.example.com/v1",
		AllowInternal: true,
		APIKeys:       []config.ProviderKey{{ID: mainID, Enabled: &disabled}},
	})
	if err == nil || !strings.Contains(err.Error(), "至少需要启用一个") {
		t.Fatalf("全部停用应被拒绝，得到 %v", err)
	}
}

// TestDisableProviderKeyOnlyWhenEnabled 覆盖自动停用的幂等性与重新启用语义。
func TestDisableProviderKeyOnlyWhenEnabled(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()
	enabled := true

	p := sampleProvider("p1")
	p.APIKeys = []config.ProviderKey{{Label: "主账号", Key: "sk-main-1111", Enabled: &enabled}}
	saved, err := st.SaveProvider(ctx, master, p)
	if err != nil {
		t.Fatal(err)
	}
	keyID := saved.Keys[0].ID

	if err := st.DisableProviderKey(ctx, keyID, "HTTP 401: invalid"); err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetProviderKey(ctx, "p1", keyID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Enabled || !rec.AutoDisabled || rec.FailureCount != 1 {
		t.Fatalf("停用后的记录 = %+v", rec)
	}
	if rec.DisabledReason != "HTTP 401: invalid" || rec.LastError != "HTTP 401: invalid" {
		t.Errorf("原因未记录: %+v", rec)
	}

	// 重复停用：WHERE enabled=1 使计数不再增长（幂等保护）
	if err := st.DisableProviderKey(ctx, keyID, "HTTP 401: invalid"); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetProviderKey(ctx, "p1", keyID)
	if rec.FailureCount != 1 {
		t.Errorf("重复停用不应重复计数: %d", rec.FailureCount)
	}

	// 运行时不装载停用的 Key
	providers, err := st.LoadProviders(ctx, master)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || len(providers[0].APIKeys) != 0 {
		t.Fatalf("停用的 Key 不应进入运行时配置: %+v", providers)
	}
	if providers[0].HasUsableKey() {
		t.Error("没有启用 Key 时 HasUsableKey 应为 false")
	}

	// 重新启用：清空自动停用标记
	p.APIKeys = []config.ProviderKey{{ID: keyID, Label: "主账号", Enabled: &enabled}}
	if _, err := st.SaveProvider(ctx, master, p); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetProviderKey(ctx, "p1", keyID)
	if !rec.Enabled || rec.AutoDisabled || rec.DisabledReason != "" {
		t.Fatalf("重新启用后应清空标记: %+v", rec)
	}
}

// TestProviderKeyUsageTotals 覆盖按 Key 的日用量聚合与窗口折算。
func TestProviderKeyUsageTotals(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	tenDaysAgo := now.AddDate(0, 0, -10).Format("2006-01-02")

	if err := st.UpsertProviderKeyUsageDaily(ctx, []KeyDailyBucket{
		{Date: today, KeyID: "k1", ProviderID: "p1", Requests: 2, InputTokens: 100, OutputTokens: 50},
		{Date: tenDaysAgo, KeyID: "k1", ProviderID: "p1", Requests: 1, InputTokens: 10, OutputTokens: 5},
		{Date: today, KeyID: "k2", ProviderID: "p1", Requests: 3, InputTokens: 7, OutputTokens: 3},
		{Date: today, KeyID: "kx", ProviderID: "other", Requests: 9, InputTokens: 900, OutputTokens: 0},
	}); err != nil {
		t.Fatal(err)
	}
	// 再累加一次，验证 UPSERT 合并
	if err := st.UpsertProviderKeyUsageDaily(ctx, []KeyDailyBucket{
		{Date: today, KeyID: "k1", ProviderID: "p1", Requests: 1, InputTokens: 5, OutputTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}

	totals, err := st.ProviderKeyUsageTotals(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	k1 := totals["k1"]
	if k1.TodayTokens != 160 || k1.Week7Tokens != 160 || k1.Month30Tokens != 175 {
		t.Errorf("k1 用量 = %+v", k1)
	}
	if k1.Month30Requests != 4 {
		t.Errorf("k1 请求数 = %d", k1.Month30Requests)
	}
	if totals["k2"].Month30Tokens != 10 {
		t.Errorf("k2 用量 = %+v", totals["k2"])
	}
	if _, ok := totals["kx"]; ok {
		t.Error("不应包含其它供应商的 Key")
	}
}

// TestDeleteProviderCascadesKeys 覆盖删除供应商时的凭证/用量级联清理。
func TestDeleteProviderCascadesKeys(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()
	enabled := true

	p := sampleProvider("p1")
	p.APIKeys = []config.ProviderKey{{Label: "主账号", Key: "sk-main", Enabled: &enabled}}
	saved, err := st.SaveProvider(ctx, master, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderKeyUsageDaily(ctx, []KeyDailyBucket{
		{Date: time.Now().UTC().Format("2006-01-02"), KeyID: saved.Keys[0].ID, ProviderID: "p1", Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProvider(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	var keys, usage int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_keys WHERE provider_id = 'p1'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_key_usage_daily WHERE provider_id = 'p1'`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || usage != 0 {
		t.Fatalf("级联删除未生效：keys=%d usage=%d", keys, usage)
	}
}

// TestProviderKeyStatsRecorderFlush 覆盖异步健康状态记录器：
// 触摸节流、失败计数、自动停用写入。
func TestProviderKeyStatsRecorderFlush(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()
	enabled := true

	p := sampleProvider("p1")
	p.APIKeys = []config.ProviderKey{
		{Label: "主账号", Key: "sk-main", Enabled: &enabled},
		{Label: "备用", Key: "sk-backup", Enabled: &enabled},
	}
	saved, err := st.SaveProvider(ctx, master, p)
	if err != nil {
		t.Fatal(err)
	}
	mainID, backupID := saved.Keys[0].ID, saved.Keys[1].ID

	rec := NewProviderKeyStatsRecorder(st, nil, ProviderKeyStatsOptions{FlushEvery: 10 * time.Millisecond})
	cancelCtx, cancel := context.WithCancel(ctx)
	rec.Start(cancelCtx)

	rec.TouchProviderKey(mainID)
	rec.TouchProviderKey(mainID) // 节流：只应写一次
	rec.RecordProviderKeyFailure(backupID, "HTTP 429: slow down")
	disabledCalled := make(chan struct{}, 1)
	rec.SetOnDisabled(func() { disabledCalled <- struct{}{} })
	rec.DisableProviderKey(mainID, "HTTP 401: invalid key")

	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("等待 recorder 落库超时")
		}
		mainRec, err := st.GetProviderKey(ctx, "p1", mainID)
		if err != nil {
			t.Fatal(err)
		}
		if !mainRec.Enabled && !mainRec.LastUsedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if !rec.Wait(2 * time.Second) {
		t.Fatal("recorder 未在超时内退出")
	}
	select {
	case <-disabledCalled:
	case <-time.After(time.Second):
		t.Error("自动停用落库后应触发回调（用于重建配置快照）")
	}

	mainRec, _ := st.GetProviderKey(ctx, "p1", mainID)
	if mainRec.Enabled || !mainRec.AutoDisabled || mainRec.FailureCount != 1 {
		t.Errorf("主 Key 状态 = %+v", mainRec)
	}
	if mainRec.LastUsedAt.IsZero() {
		t.Error("last_used_at 应被写入")
	}
	backupRec, _ := st.GetProviderKey(ctx, "p1", backupID)
	if backupRec.FailureCount != 1 || backupRec.Enabled != true || backupRec.LastError == "" {
		t.Errorf("备用 Key 状态 = %+v", backupRec)
	}
	written, dropped := rec.Stats()
	if dropped != 0 || written < 3 {
		t.Errorf("recorder 统计 = written %d / dropped %d", written, dropped)
	}
}

// TestProviderKeyStatsRecorderDropsWhenFull 覆盖队列满时的可用性优先策略。
func TestProviderKeyStatsRecorderDropsWhenFull(t *testing.T) {
	rec := NewProviderKeyStatsRecorder(nil, nil, ProviderKeyStatsOptions{QueueSize: 1, FlushEvery: time.Hour})
	rec.RecordProviderKeyFailure("k1", "a")
	rec.RecordProviderKeyFailure("k2", "b")
	_, dropped := rec.Stats()
	if dropped == 0 {
		t.Error("队列满时应计数丢弃而不是阻塞")
	}
}
