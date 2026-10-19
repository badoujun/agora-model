package api

import (
	"context"
	"net/http"
	"testing"

	"agora-model/internal/config"
	"agora-model/internal/store"
)

// TestComputeKPI 验证今日 / 7 天 / 30 天 KPI 从 daily 数组派生的逻辑。
func TestComputeKPI(t *testing.T) {
	// daily 故意按日期升序传入，包含 14 天。
	daily := []usageDailyRow{
		{Date: "2026-10-01", Total: 100},
		{Date: "2026-10-02", Total: 200},
		{Date: "2026-10-03", Total: 300},
		// 04–07：空数据（total=0）
		{Date: "2026-10-04", Total: 0},
		{Date: "2026-10-05", Total: 0},
		{Date: "2026-10-06", Total: 0},
		{Date: "2026-10-07", Total: 0},
		{Date: "2026-10-08", Total: 800},
		{Date: "2026-10-09", Total: 900},
		{Date: "2026-10-10", Total: 1000},
		{Date: "2026-10-11", Total: 1100},
		{Date: "2026-10-12", Total: 1200},
		{Date: "2026-10-13", Total: 1300},
		{Date: "2026-10-14", Total: 1400}, // today
	}

	kpi := computeKPI(daily, "2026-10-14")
	// today: 1400（精确匹配 daily 末尾）
	if kpi.TodayTotal != 1400 {
		t.Errorf("today = %d, want 1400", kpi.TodayTotal)
	}
	// last 7（含 today）：1300+1400 + 前 5 天 800+900+1000+1100+1200 = 2700+5000 = 7700
	if kpi.Week7Total != 7700 {
		t.Errorf("week7 = %d, want 7700", kpi.Week7Total)
	}
	// last 14（14 行全在 len-30=−16 内，全部累加）：
	// 100+200+300+0×4+800+900+1000+1100+1200+1300+1400 = 8300
	if kpi.Month30Total != 8300 {
		t.Errorf("month30 = %d, want 8300", kpi.Month30Total)
	}

	// 空 daily 应全 0
	empty := computeKPI(nil, "2026-10-14")
	if empty.TodayTotal != 0 || empty.Week7Total != 0 || empty.Month30Total != 0 {
		t.Errorf("空 daily 应全 0：%+v", empty)
	}

	// today 不在 daily 列表 → today=0
	noToday := computeKPI(daily, "2026-09-30")
	if noToday.TodayTotal != 0 {
		t.Errorf("today 不在 daily：today=%d 应 0", noToday.TodayTotal)
	}
}

// TestBuildHeatmap 验证返回 84 格且日期升序、未知日期 total=0。
func TestBuildHeatmap(t *testing.T) {
	daily := []usageDailyRow{
		{Date: "2026-10-14", Total: 1500}, // today - 0
		{Date: "2026-10-10", Total: 1000}, // today - 4
		{Date: "2026-09-15", Total: 50},   // 范围内
	}
	cells := buildHeatmap(daily, "2026-10-14")
	if len(cells) != 84 {
		t.Fatalf("len = %d, want 84", len(cells))
	}
	// 最后一个 cell 应是 today
	if cells[83].Date != "2026-10-14" || cells[83].Total != 1500 {
		t.Errorf("末尾 cell: %+v", cells[83])
	}
	// 第一个 cell 应是 today - 83 天 = 2026-07-23
	if cells[0].Date != "2026-07-23" {
		t.Errorf("首 cell 日期：%s, want 2026-07-23", cells[0].Date)
	}
	// 2026-09-15 在 cells 中的位置：2026-10-14 - 2026-09-15 = 29 天 → index 83-29 = 54
	if cells[54].Date != "2026-09-15" || cells[54].Total != 50 {
		t.Errorf("cells[54]: %+v", cells[54])
	}
	// 不在 daily 列表的格子 total=0
	if cells[10].Total != 0 {
		t.Errorf("cells[10] (未知日期) total=%d 应 0", cells[10].Total)
	}
}

// TestBuildTrend 验证从 daily 派生每日的 total。
func TestBuildTrend(t *testing.T) {
	daily := []usageDailyRow{
		{Date: "2026-10-12", Total: 100},
		{Date: "2026-10-13", Total: 200},
		{Date: "2026-10-14", Total: 300},
	}
	points := buildTrend(daily)
	if len(points) != 3 {
		t.Fatalf("len = %d", len(points))
	}
	for i, p := range points {
		if p.Date != daily[i].Date || p.Total != daily[i].Total {
			t.Errorf("points[%d] = %+v, want {%s, %d}", i, p, daily[i].Date, daily[i].Total)
		}
	}
}

// TestShiftDays 验证日期平移；空日期返回原串。
func TestShiftDays(t *testing.T) {
	cases := []struct {
		in   string
		days int
		want string
	}{
		{"2026-10-19", -30, "2026-09-19"},
		{"2026-10-19", 0, "2026-10-19"},
		{"2026-10-19", 1, "2026-10-20"},
		{"2026-03-01", -1, "2026-02-28"},
		{"invalid", -30, "invalid"},
	}
	for _, c := range cases {
		got := shiftDays(c.in, c.days)
		if got != c.want {
			t.Errorf("shiftDays(%q, %d) = %q, want %q", c.in, c.days, got, c.want)
		}
	}
}

// TestFirstUsedAndActiveDaysFromEmpty 验证 daily 为空时回退到 DB；这里只验证
// "daily 非空时" 的核心逻辑，避免依赖 DB（DB 全链路测试在 store 包里）。
func TestFirstUsedAndActiveDaysWithDaily(t *testing.T) {
	s := &Server{} // 不需要完整初始化——函数只读 daily 字段
	daily := []usageDailyRow{
		{Date: "2026-10-01", Total: 100},
		{Date: "2026-10-02", Total: 0}, // 不计 active
		{Date: "2026-10-03", Total: 50},
	}
	firstUsed, active := s.firstUsedAndActiveDays(context.Background(), daily, "2026-10-03")
	if firstUsed != "2026-10-01" {
		t.Errorf("firstUsed = %q, want 2026-10-01", firstUsed)
	}
	if active != 2 {
		t.Errorf("active = %d, want 2", active)
	}
}

// TestFillDailyRange 验证缺失日期补 0、已有日期原样保留、非法区间原样返回。
func TestFillDailyRange(t *testing.T) {
	daily := []usageDailyRow{
		{Date: "2026-10-01", Total: 100, Requests: 2},
		{Date: "2026-10-03", Total: 300, Requests: 3},
	}
	out := fillDailyRange(daily, "2026-10-01", "2026-10-04")
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(out), out)
	}
	want := []struct {
		date  string
		total int64
	}{
		{"2026-10-01", 100},
		{"2026-10-02", 0},
		{"2026-10-03", 300},
		{"2026-10-04", 0},
	}
	for i, w := range want {
		if out[i].Date != w.date || out[i].Total != w.total {
			t.Errorf("out[%d] = {%s, %d}, want {%s, %d}", i, out[i].Date, out[i].Total, w.date, w.total)
		}
	}
	if out[0].Requests != 2 {
		t.Errorf("已有行的其他字段应保留：requests = %d", out[0].Requests)
	}

	// 非法日期 → 原样返回
	if got := fillDailyRange(daily, "bad", "2026-10-04"); len(got) != len(daily) {
		t.Errorf("非法 from 应原样返回：%+v", got)
	}
	if got := fillDailyRange(daily, "2026-10-04", "2026-10-01"); len(got) != len(daily) {
		t.Errorf("倒序区间应原样返回：%+v", got)
	}
}

// TestUsageProviderName 验证 id → 名称回退逻辑。
func TestUsageProviderName(t *testing.T) {
	names := map[string]string{"p1": "DeepSeek H", "p2": "  "}
	if got := usageProviderName(names, "p1"); got != "DeepSeek H" {
		t.Errorf("got %q", got)
	}
	// 未登记 / 空白名称 → 原样返回 id
	if got := usageProviderName(names, "p9"); got != "p9" {
		t.Errorf("未知 id: got %q", got)
	}
	if got := usageProviderName(names, "p2"); got != "p2" {
		t.Errorf("空白名称: got %q", got)
	}
}

// TestUsageEndpoint 覆盖 GET /api/usage 的两处修复：
//  1. models[].provider / providers[].name 返回供应商名称而不是 id；
//  2. daily 补齐区间内缺失的日期（total=0）。
func TestUsageEndpoint(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	// seed 是 newHarness 内置供应商；再建一个 id 与名称不同的供应商。
	if _, err := h.store.SaveProvider(ctx, h.master, config.Provider{
		ID:            "p_named",
		Name:          "DeepSeek H",
		OpenAIBaseURL: h.upstreamURL + "/v1",
		APIKey:        "sk-named",
		Models:        []string{"named-model"},
		AllowInternal: true,
	}); err != nil {
		t.Fatalf("seed named provider: %v", err)
	}

	if err := h.store.UpsertUsageDaily(ctx, []store.DailyBucket{
		{Date: "2026-10-01", ProviderID: "seed", Model: "seed-model", Requests: 1, InputTokens: 100, OutputTokens: 50},
		{Date: "2026-10-04", ProviderID: "p_named", Model: "named-model", Requests: 2, InputTokens: 200, OutputTokens: 80},
	}); err != nil {
		t.Fatalf("写入 usage: %v", err)
	}

	client := newClient(t)
	code, body, raw := h.do(client, http.MethodGet, "/api/usage?from=2026-10-01&to=2026-10-04", nil)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", code, raw)
	}

	// 1) 供应商名称
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	modelProviders := map[string]string{}
	for _, item := range models {
		m := item.(map[string]any)
		modelProviders[m["name"].(string)] = m["provider"].(string)
	}
	if got := modelProviders["seed-model"]; got != "seed" {
		t.Errorf("seed-model 的 provider = %q, want %q", got, "seed")
	}
	if got := modelProviders["named-model"]; got != "DeepSeek H" {
		t.Errorf("named-model 的 provider = %q, want %q", got, "DeepSeek H")
	}

	providers, _ := body["providers"].([]any)
	provNames := map[string]string{}
	for _, item := range providers {
		p := item.(map[string]any)
		provNames[p["id"].(string)] = p["name"].(string)
	}
	if got := provNames["p_named"]; got != "DeepSeek H" {
		t.Errorf("providers[p_named].name = %q, want %q", got, "DeepSeek H")
	}

	// 2) daily 补齐：4 天连续，10-02 / 10-03 为 0 值行
	daily, _ := body["daily"].([]any)
	if len(daily) != 4 {
		t.Fatalf("daily 长度 = %d, want 4: %v", len(daily), daily)
	}
	wantTotals := []float64{150, 0, 0, 280}
	for i, want := range wantTotals {
		row := daily[i].(map[string]any)
		if row["date"] != []string{"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"}[i] {
			t.Errorf("daily[%d].date = %v", i, row["date"])
		}
		if row["total"].(float64) != want {
			t.Errorf("daily[%d].total = %v, want %v", i, row["total"], want)
		}
	}

	// trend 与 daily 同长，0 值日期也在
	trend, _ := body["trend"].([]any)
	if len(trend) != 4 {
		t.Fatalf("trend 长度 = %d, want 4", len(trend))
	}
}
