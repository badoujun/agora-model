package store

import (
	"context"
	"testing"
	"time"
)

// TestUpsertUsageMinute 验证同主键 (minute, provider, model) 合并，多次写累加。
func TestUpsertUsageMinute(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	minute := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)
	bucket := MinuteBucket{
		Minute: minute, ProviderID: "p1", Model: "m1",
		Requests: 1, InputTokens: 100, CachedInputTokens: 30, OutputTokens: 50,
	}
	if err := st.UpsertUsageMinute(ctx, []MinuteBucket{bucket}); err != nil {
		t.Fatalf("UpsertUsageMinute: %v", err)
	}

	// 第二次写：增加不同字段值 → 验证累加
	bucket2 := bucket
	bucket2.Requests = 2
	bucket2.InputTokens = 50
	bucket2.CachedInputTokens = 10
	bucket2.OutputTokens = 20
	if err := st.UpsertUsageMinute(ctx, []MinuteBucket{bucket2}); err != nil {
		t.Fatalf("UpsertUsageMinute: %v", err)
	}

	// 直接读 SQLite 验证：累计 = requests 3 / input 150 / cached 40 / output 70
	var reqs, input, cached, output int64
	row := st.DB().QueryRowContext(ctx,
		`SELECT requests, input_tokens, cached_input_tokens, output_tokens
		 FROM model_usage_minute WHERE minute=? AND provider_id=? AND model=?`,
		minute.UTC().Format("2006-01-02T15:04"), "p1", "m1")
	if err := row.Scan(&reqs, &input, &cached, &output); err != nil {
		t.Fatal(err)
	}
	if reqs != 3 || input != 150 || cached != 40 || output != 70 {
		t.Errorf("requests/input/cached/output = %d/%d/%d/%d, want 3/150/40/70", reqs, input, cached, output)
	}
}

// TestUpsertUsageDaily 验证同主键 (date, provider, model) 合并。
func TestUpsertUsageDaily(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	bucket := DailyBucket{
		Date: "2026-10-19", ProviderID: "p1", Model: "m1",
		Requests: 3, InputTokens: 300, CachedInputTokens: 100, OutputTokens: 150,
	}
	if err := st.UpsertUsageDaily(ctx, []DailyBucket{bucket}); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}
	// 累加
	if err := st.UpsertUsageDaily(ctx, []DailyBucket{bucket}); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}

	var reqs, input, cached, output int64
	row := st.DB().QueryRowContext(ctx,
		`SELECT requests, input_tokens, cached_input_tokens, output_tokens
		 FROM model_usage_daily WHERE date=? AND provider_id=? AND model=?`,
		"2026-10-19", "p1", "m1")
	if err := row.Scan(&reqs, &input, &cached, &output); err != nil {
		t.Fatal(err)
	}
	if reqs != 6 || input != 600 || cached != 200 || output != 300 {
		t.Errorf("requests/input/cached/output = %d/%d/%d/%d, want 6/600/200/300", reqs, input, cached, output)
	}
}

// TestPruneUsageMinute 验证 before 时间之前的分钟桶被删除，之后的保留。
func TestPruneUsageMinute(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	old := MinuteBucket{
		Minute: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		ProviderID: "p1", Model: "m1", Requests: 1, InputTokens: 10, OutputTokens: 5,
	}
	newer := MinuteBucket{
		Minute: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		ProviderID: "p1", Model: "m1", Requests: 1, InputTokens: 10, OutputTokens: 5,
	}
	if err := st.UpsertUsageMinute(ctx, []MinuteBucket{old, newer}); err != nil {
		t.Fatal(err)
	}

	// 删早于 2026-10-10 的（保留 10-19）
	n, err := st.PruneUsageMinute(ctx, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("prune rows = %d, want 1", n)
	}

	// 旧的不在了
	var count int
	row := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_usage_minute WHERE minute=?`,
		old.Minute.UTC().Format("2006-01-02T15:04"))
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("旧 minute 应已被删，count=%d", count)
	}
	// 新的还在
	row = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_usage_minute WHERE minute=?`,
		newer.Minute.UTC().Format("2006-01-02T15:04"))
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("新 minute 应保留，count=%d", count)
	}
}
