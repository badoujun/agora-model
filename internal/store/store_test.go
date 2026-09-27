package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
)

// tempDBPath 返回一个独立的临时数据库路径。
//
// 刻意不用 t.TempDir()：Windows 上 SQLite 关闭后句柄释放可能有延迟，
// TempDir 的严格清理会因此把测试判为失败；这里改为带重试的静默清理。
func tempDBPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agora-store-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("临时目录未能清理（Windows 句柄释放延迟）: %s", dir)
	})
	return filepath.Join(dir, "agora.db")
}

func newTestStore(t *testing.T) (*Store, []byte) {
	t.Helper()
	path := tempDBPath(t)
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	master := make([]byte, crypto.KeySize)
	for i := range master {
		master[i] = byte(i + 1)
	}
	return st, master
}

func sampleProvider(id string) config.Provider {
	enabled := true
	return config.Provider{
		ID:             id,
		Name:           "供应商 " + id,
		OpenAIBaseURL:  "https://api.example.com/v1",
		APIKey:         "sk-secret-" + id,
		Models:         []string{"m1", "m2"},
		ModelAliases:   map[string]string{"m1": "别名一"},
		TimeoutSeconds: 60,
		AllowInternal:  true, // 测试跳过 DNS/IP 校验，SSRF 单测另行覆盖
		Enabled:        &enabled,
	}
}

// tableColumns 返回某张表的列名集合。
func tableColumns(t *testing.T, st *Store, table string) map[string]bool {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("读取 %s 表结构失败: %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	return cols
}

func TestOpenIsIdempotentAndMigrates(t *testing.T) {
	path := tempDBPath(t)
	ctx := context.Background()

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	var version int
	if err := first.DB().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("读取迁移版本: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("迁移版本 = %d, 期望 %d", version, len(migrations))
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("二次 Open 应幂等: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var count int
	if err := second.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Fatalf("迁移记录数 = %d, 期望 %d（不应重复执行）", count, len(migrations))
	}
}

func TestMigrationV3MigratesLegacyProviders(t *testing.T) {
	path := tempDBPath(t)
	ctx := context.Background()

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 回退到 v2 结构，模拟升级前的数据库
	for _, table := range []string{"providers", "gateway_keys", "model_cache", "logs", "settings"} {
		if _, err := st.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range append(append([]string{}, schemaV1...), schemaV2...) {
		if _, err := st.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("重建 v2 结构失败: %v（%s）", err, stmt)
		}
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO providers (id, name, openai_base_url, api_key_cipher, api_key_hint,
			models_manual_json, models_excluded_json, created_at, updated_at)
		 VALUES ('legacy', '旧供应商', 'https://api.example.com/v1', x'00', 'sk-****',
			'["m1","m2"]', '["m2"]', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 3`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("迁移 v3 失败: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	rec, err := reopened.GetProvider(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ModelsSelected) != 2 || rec.ModelsSelected[0] != "m1" || rec.ModelsSelected[1] != "m2" {
		t.Fatalf("旧的手动模型应迁移为已勾选模型，得到 %v", rec.ModelsSelected)
	}

	cols := tableColumns(t, reopened, "providers")
	for _, gone := range []string{"anthropic_base_url", "anthropic_endpoint_override", "models_manual_json", "models_excluded_json", "auto_fetch_models", "priority"} {
		if cols[gone] {
			t.Errorf("旧列 %s 应已被删除", gone)
		}
	}
	for _, want := range []string{"models_selected_json", "models_alias_json", "last_fetch_at", "last_fetch_error"} {
		if !cols[want] {
			t.Errorf("providers 表缺少字段 %s", want)
		}
	}
}

func TestProviderRoundTripStoresCiphertext(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	saved, err := st.SaveProvider(ctx, master, sampleProvider("p1"))
	if err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	if saved.APIKeyHint != crypto.Mask("sk-secret-p1") {
		t.Errorf("hint = %q", saved.APIKeyHint)
	}
	if strings.Contains(string(saved.APIKeyCipher), "sk-secret-p1") {
		t.Fatal("落库的凭证必须是密文")
	}
	if !saved.Enabled {
		t.Error("enabled 字段落库异常")
	}

	providers, err := st.LoadProviders(ctx, master)
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("供应商数量 = %d", len(providers))
	}
	got := providers[0]
	if got.APIKey != "sk-secret-p1" {
		t.Errorf("解密后凭证 = %q", got.APIKey)
	}
	if got.Name != "供应商 p1" || len(got.Models) != 2 {
		t.Errorf("字段往返异常: %+v", got)
	}
	if got.ModelAliases["m1"] != "别名一" {
		t.Errorf("别名往返异常: %#v", got.ModelAliases)
	}
	if got.Endpoint() != "https://api.example.com/v1/chat/completions" {
		t.Errorf("endpoint = %q", got.Endpoint())
	}
	if got.ModelsEndpoint() != "https://api.example.com/v1/models" {
		t.Errorf("models endpoint = %q", got.ModelsEndpoint())
	}
}

func TestProviderCipherIsBoundToProviderID(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	recA, err := st.SaveProvider(ctx, master, sampleProvider("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveProvider(ctx, master, sampleProvider("b")); err != nil {
		t.Fatal(err)
	}
	// 把 a 的密文搬到 b 上（模拟数据库被手工篡改）
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE providers SET api_key_cipher = ? WHERE id = ?`, recA.APIKeyCipher, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadProviders(ctx, master); err == nil {
		t.Fatal("AAD 绑定 provider.id：密文被搬到其他记录后应解密失败")
	}
}

func TestSaveProviderRejectsInternalURLWithoutAllow(t *testing.T) {
	st, master := newTestStore(t)
	p := sampleProvider("p1")
	p.AllowInternal = false
	p.OpenAIBaseURL = "http://127.0.0.1:9999/v1"

	_, err := st.SaveProvider(context.Background(), master, p)
	if err == nil {
		t.Fatal("未开启 allow_internal 时内网地址应被拒绝")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("错误信息应说明内网地址被拒: %v", err)
	}
}

func TestListProvidersOrderedByName(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	for _, p := range []config.Provider{
		{ID: "b", Name: "Beta", OpenAIBaseURL: "https://b.example/v1", APIKey: "sk-b", AllowInternal: true},
		{ID: "a", Name: "Alpha", OpenAIBaseURL: "https://a.example/v1", APIKey: "sk-a", AllowInternal: true},
		{ID: "c", Name: "Gamma", OpenAIBaseURL: "https://c.example/v1", APIKey: "sk-c", AllowInternal: true},
	} {
		if _, err := st.SaveProvider(ctx, master, p); err != nil {
			t.Fatal(err)
		}
	}

	records, err := st.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(records))
	for _, rec := range records {
		got = append(got, rec.Name)
	}
	want := []string{"Alpha", "Beta", "Gamma"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v, 期望 %v", got, want)
		}
	}
}

func TestDeleteAndCountProviders(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()
	if _, err := st.SaveProvider(ctx, master, sampleProvider("p1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveProvider(ctx, master, sampleProvider("p2")); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ProviderCount(ctx); err != nil || n != 2 {
		t.Fatalf("ProviderCount = %d (%v)", n, err)
	}
	if err := st.DeleteProvider(ctx, "p1"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := st.GetProvider(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应返回 ErrNotFound，得到 %v", err)
	}
	if err := st.DeleteProvider(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除不存在的供应商应返回 ErrNotFound，得到 %v", err)
	}
}

func TestImportProviders(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	n, err := st.ImportProviders(ctx, master, []config.Provider{sampleProvider("x"), sampleProvider("y")})
	if err != nil {
		t.Fatalf("ImportProviders: %v", err)
	}
	if n != 2 {
		t.Fatalf("导入数量 = %d", n)
	}
	if count, _ := st.ProviderCount(ctx); count != 2 {
		t.Fatalf("ProviderCount = %d", count)
	}
}

func TestGatewayKeyLifecycle(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	plain, created, err := st.EnsureGatewayKey(ctx)
	if err != nil {
		t.Fatalf("EnsureGatewayKey: %v", err)
	}
	if !created || !strings.HasPrefix(plain, GatewayKeyPrefix) {
		t.Fatalf("首次应生成带前缀的 Key，得到 %q created=%v", plain, created)
	}
	if again, created2, err := st.EnsureGatewayKey(ctx); err != nil || created2 || again != "" {
		t.Fatalf("二次调用不应重新生成: %q created=%v err=%v", again, created2, err)
	}

	info, err := st.ActiveGatewayKey(ctx)
	if err != nil {
		t.Fatalf("ActiveGatewayKey: %v", err)
	}
	if info.KeyHint != crypto.Mask(plain) {
		t.Errorf("hint = %q, 期望 %q", info.KeyHint, crypto.Mask(plain))
	}
	var hash string
	if err := st.DB().QueryRowContext(ctx, `SELECT key_hash FROM gateway_keys`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, plain) {
		t.Fatal("数据库中不得存放网关 Key 明文")
	}

	if _, ok, err := st.VerifyGatewayKey(ctx, plain); err != nil || !ok {
		t.Fatalf("正确 Key 应通过校验: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.VerifyGatewayKey(ctx, "gw-wrong"); err != nil || ok {
		t.Fatalf("错误 Key 不应通过: ok=%v err=%v", ok, err)
	}

	newPlain, err := st.ResetGatewayKey(ctx)
	if err != nil {
		t.Fatalf("ResetGatewayKey: %v", err)
	}
	if newPlain == plain {
		t.Fatal("重置后应得到不同的 Key")
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, plain); ok {
		t.Fatal("重置后旧 Key 必须立即失效")
	}
	if _, ok, _ := st.VerifyGatewayKey(ctx, newPlain); !ok {
		t.Fatal("重置后新 Key 应可用")
	}
	if err := st.TouchGatewayKey(ctx, info.ID); err != nil {
		t.Fatalf("TouchGatewayKey: %v", err)
	}
}

func TestSettings(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := st.GetSetting(ctx, SettingLogSuccess); err != nil || ok {
		t.Fatalf("未设置的键应返回 ok=false: ok=%v err=%v", ok, err)
	}
	if err := st.SetSetting(ctx, SettingLogSuccess, "true"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := st.GetSetting(ctx, SettingLogSuccess); err != nil || !ok || v != "true" {
		t.Fatalf("GetSetting = %q ok=%v err=%v", v, ok, err)
	}
	if err := st.SetSetting(ctx, SettingLogSuccess, "false"); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllSettings(ctx)
	if err != nil || all[SettingLogSuccess] != "false" {
		t.Fatalf("AllSettings = %v (%v)", all, err)
	}
}

func TestLogsInsertQueryRedactAndPrune(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	entries := []logging.Entry{
		{TS: time.Now().UTC(), RequestID: "r1", InboundProtocol: "openai", Model: "GPT-4O", StatusCode: 200, LatencyMS: 12},
		{TS: time.Now().UTC(), RequestID: "r2", InboundProtocol: "openai", Model: "claude-3-5", StatusCode: 404, LatencyMS: 3,
			ErrorMsg: "上游拒绝：authorization=Bearer sk-leaked-secret-value"},
		{TS: time.Now().UTC(), RequestID: "r3", InboundProtocol: "openai", Model: "gpt-4o-mini", StatusCode: 504, LatencyMS: 1000, Stream: true},
	}
	if err := st.InsertLogs(ctx, entries); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}
	if n, err := st.CountLogs(ctx); err != nil || n != 3 {
		t.Fatalf("CountLogs = %d (%v)", n, err)
	}

	got, err := st.QueryLogs(ctx, LogFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("仅失败 = %d 条, 期望 2", len(got))
	}
	if got[0].RequestID != "r3" {
		t.Errorf("应按时间倒序，首条 = %s", got[0].RequestID)
	}
	for _, e := range got {
		if strings.Contains(e.ErrorMsg, "sk-leaked-secret-value") {
			t.Fatalf("落库的日志必须脱敏，实际: %s", e.ErrorMsg)
		}
	}

	// 模型筛选：忽略大小写的模糊匹配
	for _, kw := range []string{"gpt-4o", "GPT", "-4O", "claude"} {
		hits, err := st.QueryLogs(ctx, LogFilter{Model: kw})
		if err != nil {
			t.Fatalf("QueryLogs(%q): %v", kw, err)
		}
		if kw == "claude" {
			if len(hits) != 1 || hits[0].Model != "claude-3-5" {
				t.Fatalf("模型筛选 %q 命中 %d 条, 期望 1", kw, len(hits))
			}
			continue
		}
		if len(hits) != 2 {
			t.Fatalf("模型筛选 %q 命中 %d 条, 期望 2（GPT-4O 与 gpt-4o-mini）", kw, len(hits))
		}
	}
	if hits, _ := st.QueryLogs(ctx, LogFilter{Model: "不存在"}); len(hits) != 0 {
		t.Fatalf("无关关键词不应命中，实际 %d 条", len(hits))
	}
	// LIKE 通配符按字面匹配
	if hits, _ := st.QueryLogs(ctx, LogFilter{Model: "%"}); len(hits) != 0 {
		t.Fatalf("%% 应按字面匹配，实际 %d 条", len(hits))
	}
	if hits, _ := st.QueryLogs(ctx, LogFilter{Model: "gpt_4o"}); len(hits) != 0 {
		t.Fatalf("_ 应按字面匹配，实际 %d 条", len(hits))
	}

	if _, err := st.PruneLogs(ctx, 2); err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if n, _ := st.CountLogs(ctx); n != 2 {
		t.Fatalf("清理后条数 = %d, 期望 2", n)
	}
}

func TestLogsSchemaColumns(t *testing.T) {
	// 保证 logs 表包含 first_byte_ms / stream 等字段（防止 DDL 漂移）
	st, _ := newTestStore(t)
	cols := tableColumns(t, st, "logs")
	for _, want := range []string{"first_byte_ms", "stream", "error_msg", "client_ip", "provider_id"} {
		if !cols[want] {
			t.Errorf("logs 表缺少字段 %s", want)
		}
	}
}

func TestCandidateModelsReplaceLoadAndFetchError(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	if _, err := st.SaveProvider(ctx, master, sampleProvider("p1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveProvider(ctx, master, sampleProvider("p2")); err != nil {
		t.Fatal(err)
	}

	fetchedAt := time.Now().UTC().Truncate(time.Second)
	if err := st.ReplaceCandidateModels(ctx, "p1", []string{"m1", "m2"}, fetchedAt); err != nil {
		t.Fatalf("ReplaceCandidateModels: %v", err)
	}

	models, at, err := st.LoadCandidateModels(ctx, "p1")
	if err != nil {
		t.Fatalf("LoadCandidateModels: %v", err)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "m2" {
		t.Fatalf("候选缓存 = %v", models)
	}
	if !at.Equal(fetchedAt) {
		t.Errorf("fetched_at = %v, 期望 %v", at, fetchedAt)
	}
	if got, _, _ := st.LoadCandidateModels(ctx, "p2"); len(got) != 0 {
		t.Errorf("未拉取的供应商不应有候选缓存: %v", got)
	}

	// 覆盖式替换：旧模型应被清掉
	if err := st.ReplaceCandidateModels(ctx, "p1", []string{"m3"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	models, _, _ = st.LoadCandidateModels(ctx, "p1")
	if len(models) != 1 || models[0] != "m3" {
		t.Fatalf("替换后缓存 = %v", models)
	}

	// 记录失败原因、成功后清空
	if err := st.SetProviderFetchError(ctx, "p1", "上游返回 401: invalid key"); err != nil {
		t.Fatalf("SetProviderFetchError: %v", err)
	}
	rec, err := st.GetProvider(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.LastFetchError, "401") {
		t.Errorf("last_fetch_error = %q", rec.LastFetchError)
	}

	if err := st.ReplaceCandidateModels(ctx, "p1", []string{"m4"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetProvider(ctx, "p1")
	if rec.LastFetchError != "" {
		t.Errorf("成功拉取后应清空 last_fetch_error，得到 %q", rec.LastFetchError)
	}
	if rec.LastFetchAt.IsZero() {
		t.Error("成功拉取后应记录 last_fetch_at")
	}
}

func TestSaveProviderPreservesFetchStatus(t *testing.T) {
	st, master := newTestStore(t)
	ctx := context.Background()

	p := sampleProvider("p1")
	if _, err := st.SaveProvider(ctx, master, p); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceCandidateModels(ctx, "p1", []string{"cached-model"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 再次保存（例如 Web UI 改了名称）不应清空拉取状态
	p.Name = "改名后的供应商"
	p.Models = []string{"m3"}
	if _, err := st.SaveProvider(ctx, master, p); err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetProvider(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastFetchAt.IsZero() {
		t.Error("更新供应商不应清空 last_fetch_at")
	}
	if rec.Name != "改名后的供应商" {
		t.Errorf("Name = %q", rec.Name)
	}
	if len(rec.ModelsSelected) != 1 || rec.ModelsSelected[0] != "m3" {
		t.Errorf("勾选模型应被更新: %v", rec.ModelsSelected)
	}
	if got, _, _ := st.LoadCandidateModels(ctx, "p1"); len(got) != 1 || got[0] != "cached-model" {
		t.Errorf("候选缓存不应被 SaveProvider 影响: %v", got)
	}
}
