package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agora-model/internal/config"
	"agora-model/internal/crypto"
	"agora-model/internal/logging"
)

func newTestStore(t *testing.T) (*Store, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "agora.db")
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
	autoFetch := true
	return config.Provider{
		ID:               id,
		Name:             "供应商 " + id,
		OpenAIBaseURL:    "https://api.example.com/v1",
		AnthropicBaseURL: "https://api.example.com",
		APIKey:           "sk-secret-" + id,
		Models:           []string{"m1", "m2"},
		Priority:         10,
		TimeoutSeconds:   60,
		AllowInternal:    true, // 测试跳过 DNS/IP 校验，SSRF 单测另行覆盖
		AutoFetchModels:  &autoFetch,
		Enabled:          &enabled,
	}
}

func TestOpenIsIdempotentAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agora.db")
	ctx := context.Background()

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	var version int
	if err := first.DB().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("读取迁移版本: %v", err)
	}
	if version != 1 {
		t.Fatalf("迁移版本 = %d, 期望 1", version)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("二次 Open 应幂等: %v", err)
	}
	defer second.Close()
	var count int
	if err := second.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("迁移记录数 = %d, 期望 1（不应重复执行）", count)
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
	if saved.AutoFetchModels != true || !saved.Enabled {
		t.Errorf("布尔字段落库异常: auto=%v enabled=%v", saved.AutoFetchModels, saved.Enabled)
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
	if got.Name != "供应商 p1" || len(got.Models) != 2 || got.Priority != 10 {
		t.Errorf("字段往返异常: %+v", got)
	}
	if got.Endpoint(config.ProtocolOpenAI) != "https://api.example.com/v1/chat/completions" {
		t.Errorf("endpoint = %q", got.Endpoint(config.ProtocolOpenAI))
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
		{TS: time.Now().UTC(), RequestID: "r1", InboundProtocol: "openai", Model: "m1", StatusCode: 200, LatencyMS: 12},
		{TS: time.Now().UTC(), RequestID: "r2", InboundProtocol: "anthropic", Model: "m2", StatusCode: 404, LatencyMS: 3,
			ErrorMsg: "上游拒绝：authorization=Bearer sk-leaked-secret-value"},
		{TS: time.Now().UTC(), RequestID: "r3", InboundProtocol: "openai", Model: "m1", StatusCode: 504, LatencyMS: 1000, Stream: true},
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

	if _, err := st.PruneLogs(ctx, 2); err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if n, _ := st.CountLogs(ctx); n != 2 {
		t.Fatalf("清理后条数 = %d, 期望 2", n)
	}
}

func TestLogsSchemaDropAndRecreateUnsuitableColumns(t *testing.T) {
	// 保证 logs 表包含 first_byte_ms / stream 等字段（防止 DDL 漂移）
	st, _ := newTestStore(t)
	rows, err := st.DB().QueryContext(context.Background(), `SELECT name FROM pragma_table_info('logs')`)
	if err != nil {
		t.Fatalf("读取表结构失败: %v", err)
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
	for _, want := range []string{"first_byte_ms", "stream", "error_msg", "client_ip", "provider_id"} {
		if !cols[want] {
			t.Errorf("logs 表缺少字段 %s", want)
		}
	}
}
