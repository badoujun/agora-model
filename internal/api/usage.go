package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
)

// usagePayload 与前端 UsagePayload 形状一一对应。
//
// 所有数字字段都是窗口内的实际 token 数；percent 字段是窗口内的相对占比
// （基线 = 全部 token）。前端 mock 阶段没有用 percent 计算，但保留以便
// 真实接入后切换。
type usagePayload struct {
	FirstUsedAt string               `json:"first_used_at"`
	ActiveDays  int                  `json:"active_days"`
	Granularity string               `json:"granularity"`
	KPI         usageKPI             `json:"kpi"`
	Models      []usageModelShare    `json:"models"`
	Providers   []usageProviderShare `json:"providers"`
	Daily       []usageDailyRow      `json:"daily"`
	Heatmap     []usageHeatmapCell   `json:"heatmap"`
	Trend       []usageTrendPoint    `json:"trend"`
}

type usageKPI struct {
	TodayTotal   int64 `json:"today_total"`
	Week7Total   int64 `json:"week7_total"`
	Month30Total int64 `json:"month30_total"`
}

type usageModelShare struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Total         int64  `json:"total"`
	Percent       int64  `json:"percent"`
	InputCached   int64  `json:"input_cached"`
	InputUncached int64  `json:"input_uncached"`
	Output        int64  `json:"output"`
	Requests      int64  `json:"requests"`
}

type usageProviderShare struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Total      int64  `json:"total"`
	Percent    int64  `json:"percent"`
	ModelCount int    `json:"model_count"`
}

type usageDailyRow struct {
	Date          string               `json:"date"`
	Total         int64                `json:"total"`
	InputCached   int64                `json:"input_cached"`
	InputUncached int64                `json:"input_uncached"`
	Output        int64                `json:"output"`
	Requests      int64                `json:"requests"`
	Breakdown     []usageBreakdownItem `json:"breakdown,omitempty"`
}

// usageBreakdownItem 是某一天内一个 (provider, model) 的用量明细。
//
// 前端热力图 / 使用趋势的悬浮弹窗按"供应商/模型 + 输入(命中缓存) +
// 输入(未命中缓存) + 输出"四列展示，数据来自这里。没有明细的日子
// （补齐出来的空行）该字段缺省，前端按"无数据"渲染。
type usageBreakdownItem struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	InputCached   int64  `json:"input_cached"`
	InputUncached int64  `json:"input_uncached"`
	Output        int64  `json:"output"`
}

type usageHeatmapCell struct {
	Date  string `json:"date"`
	Total int64  `json:"total"`
}

type usageTrendPoint struct {
	Date  string `json:"date"`
	Total int64  `json:"total"`
}

// handleUsage 返回 GET /api/usage 的响应。
//
// 支持 query：
//   - from / to：YYYY-MM-DD（闭区间）；from 缺省时取最早有数据的一天
//     （保证前端「累计」窗口看到完整历史），to 缺省时为今天
//   - granularity：daily / weekly / monthly（前端目前只用 daily，但接受参数）
//
// daily 会补齐 [from, to] 内**缺失的日期**（total=0 的行），这样前端按窗口
// 渲染趋势时，没有请求的日子依然有点位。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}

	q := r.URL.Query()
	today := time.Now().UTC().Format("2006-01-02")
	from := q.Get("from")
	to := q.Get("to")
	if to == "" {
		to = today
	}
	granularity := q.Get("granularity")
	if granularity == "" {
		granularity = "daily"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if from == "" {
		from = s.earliestUsageDate(ctx)
		if from == "" || from > to {
			from = shiftDays(to, -30)
		}
	}

	daily, models, providers, err := s.aggregateUsageRange(ctx, from, to, s.providerDisplayNames(ctx))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "读取 usage 数据失败", err)
		return
	}

	// first_used_at / active_days 基于真实有数据的行计算，随后再补零，
	// 否则补出来的空行会把 first_used_at 拉早到窗口起点。
	firstUsed, activeDays := s.firstUsedAndActiveDays(ctx, daily, today)

	kpi := computeKPI(daily, today)
	daily = fillDailyRange(daily, from, to)
	heatmap := buildHeatmap(daily, today)
	trend := buildTrend(daily)

	writeJSON(w, http.StatusOK, usagePayload{
		FirstUsedAt: firstUsed,
		ActiveDays:  activeDays,
		Granularity: granularity,
		KPI:         kpi,
		Models:      models,
		Providers:   providers,
		Daily:       daily,
		Heatmap:     heatmap,
		Trend:       trend,
	})
}

// aggregateUsageRange 一次 SQL 把 [from, to] 区间内所有 model_usage_daily 行
// 拉出来，分别按 (date) 和 (provider, model) 聚合；同步按 (provider) 聚合供
// 供应商占比。
//
// 返回的 daily 已按日期升序；models / providers 按 total 降序。窗口内
// total=0 的 (provider, model) 不出现在 models/providers（前端按总数排序，
// 0 占比无意义）。
//
// providerNames 把 provider_id 映射为展示名；缺失时回退为 id 本身。
// 统计表里存的是 provider_id，而前端要显示供应商名称。
func (s *Server) aggregateUsageRange(ctx context.Context, from, to string, providerNames map[string]string) (
	dailyOut []usageDailyRow,
	models []usageModelShare,
	providers []usageProviderShare,
	err error,
) {
	rows, qerr := s.store.DB().QueryContext(ctx,
		`SELECT date, provider_id, model, requests, input_tokens, cached_input_tokens, output_tokens
		 FROM model_usage_daily
		 WHERE date >= ? AND date <= ?`, from, to)
	if qerr != nil {
		return nil, nil, nil, qerr
	}
	defer rows.Close()

	// 按 (provider, model) 聚合
	type modelKey struct {
		providerID, model string
	}
	// 按 date 聚合 daily 行（输入 + 输出 = total）；同时按 (provider, model)
	// 收集当天明细，供前端悬浮弹窗展开。
	type dayAccum struct {
		requests, input, cached, output int64
		breakdown                       map[modelKey]*usageBreakdownItem
	}
	byDay := map[string]*dayAccum{}
	modelAcc := map[modelKey]*usageModelShare{}
	// 按 provider 聚合
	type provAccum struct {
		total                                        int64
		inputCached, inputUncached, output, requests int64
		models                                       map[string]struct{}
	}
	provAccMap := map[string]*provAccum{}

	var totalAll int64

	for rows.Next() {
		var (
			date, providerID, model         string
			requests, input, cached, output int64
		)
		if err := rows.Scan(&date, &providerID, &model, &requests, &input, &cached, &output); err != nil {
			return nil, nil, nil, err
		}
		rowTotal := input + output

		// daily 聚合（并按 (provider, model) 收集当天明细）
		da, ok := byDay[date]
		if !ok {
			da = &dayAccum{breakdown: map[modelKey]*usageBreakdownItem{}}
			byDay[date] = da
		}
		da.requests += requests
		da.input += input
		da.cached += cached
		da.output += output

		bk := modelKey{providerID, model}
		item, ok := da.breakdown[bk]
		if !ok {
			item = &usageBreakdownItem{
				Provider: usageProviderName(providerNames, providerID),
				Model:    model,
			}
			da.breakdown[bk] = item
		}
		item.InputCached += cached
		item.InputUncached += input - cached
		item.Output += output

		// model 聚合
		mk := modelKey{providerID, model}
		ma, ok := modelAcc[mk]
		if !ok {
			ma = &usageModelShare{Name: model, Provider: usageProviderName(providerNames, providerID)}
			modelAcc[mk] = ma
		}
		ma.Total += rowTotal
		ma.InputCached += cached
		ma.InputUncached += input - cached
		ma.Output += output
		ma.Requests += requests

		// provider 聚合
		pa, ok := provAccMap[providerID]
		if !ok {
			pa = &provAccum{models: map[string]struct{}{}}
			provAccMap[providerID] = pa
		}
		pa.total += rowTotal
		pa.inputCached += cached
		pa.inputUncached += input - cached
		pa.output += output
		pa.requests += requests
		pa.models[model] = struct{}{}

		totalAll += rowTotal
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	// daily 行（按日期升序；每天附上按 (provider, model) 的明细）
	dailyOut = make([]usageDailyRow, 0, len(byDay))
	for date, acc := range byDay {
		breakdown := make([]usageBreakdownItem, 0, len(acc.breakdown))
		for _, item := range acc.breakdown {
			breakdown = append(breakdown, *item)
		}
		// 无用量 / 全 0 的行不返回明细，前端按"当天无数据"渲染
		if len(breakdown) > 0 {
			sort.Slice(breakdown, func(i, j int) bool {
				left := breakdown[i].InputCached + breakdown[i].InputUncached + breakdown[i].Output
				right := breakdown[j].InputCached + breakdown[j].InputUncached + breakdown[j].Output
				if left != right {
					return left > right
				}
				if breakdown[i].Provider != breakdown[j].Provider {
					return breakdown[i].Provider < breakdown[j].Provider
				}
				return breakdown[i].Model < breakdown[j].Model
			})
		}
		dailyOut = append(dailyOut, usageDailyRow{
			Date:          date,
			Total:         acc.input + acc.output,
			InputCached:   acc.cached,
			InputUncached: acc.input - acc.cached,
			Output:        acc.output,
			Requests:      acc.requests,
			Breakdown:     breakdown,
		})
	}
	sort.Slice(dailyOut, func(i, j int) bool { return dailyOut[i].Date < dailyOut[j].Date })

	// model 列表（按 total 降序；total=0 过滤）
	models = make([]usageModelShare, 0, len(modelAcc))
	for _, m := range modelAcc {
		if m.Total == 0 {
			continue
		}
		if totalAll > 0 {
			m.Percent = int64(float64(m.Total) / float64(totalAll) * 100)
		}
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Total > models[j].Total })

	// provider 列表（按 total 降序）
	providers = make([]usageProviderShare, 0, len(provAccMap))
	for id, p := range provAccMap {
		if p.total == 0 {
			continue
		}
		percent := int64(0)
		if totalAll > 0 {
			percent = int64(float64(p.total) / float64(totalAll) * 100)
		}
		providers = append(providers, usageProviderShare{
			ID:         id,
			Name:       usageProviderName(providerNames, id),
			Total:      p.total,
			Percent:    percent,
			ModelCount: len(p.models),
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Total > providers[j].Total })

	return dailyOut, models, providers, nil
}

// computeKPI 从每日明细派生今日 / 7 天 / 30 天三个 KPI。
//
// 输入 daily 已按日期升序；today 用于定位"今日"行（防止 daily 末尾
// 不是 today 时误把昨天当今天）。
func computeKPI(daily []usageDailyRow, today string) usageKPI {
	if len(daily) == 0 {
		return usageKPI{}
	}
	var (
		todayTotal  int64
		last7Total  int64
		last30Total int64
		n7, n30     int
	)
	for i, r := range daily {
		if r.Date == today {
			todayTotal = r.Total
		}
		if i >= len(daily)-7 {
			last7Total += r.Total
			n7++
		}
		if i >= len(daily)-30 {
			last30Total += r.Total
			n30++
		}
	}
	_ = n7
	_ = n30
	return usageKPI{
		TodayTotal:   todayTotal,
		Week7Total:   last7Total,
		Month30Total: last30Total,
	}
}

// buildHeatmap 把 daily 切成最近 84 天（12 周 × 7 天）的格子，按日期升序填充。
//
// 超出 daily 范围的格子 total=0（前端热力图渲染为浅灰占位）。
func buildHeatmap(daily []usageDailyRow, today string) []usageHeatmapCell {
	byDate := map[string]int64{}
	for _, r := range daily {
		byDate[r.Date] = r.Total
	}
	out := make([]usageHeatmapCell, 0, 84)
	end, err := time.Parse("2006-01-02", today)
	if err != nil {
		return out
	}
	for i := 83; i >= 0; i-- {
		d := end.AddDate(0, 0, -i)
		key := d.Format("2006-01-02")
		out = append(out, usageHeatmapCell{Date: key, Total: byDate[key]})
	}
	return out
}

// buildTrend 直接从 daily 派生每日 total，作为柱状图数据。
func buildTrend(daily []usageDailyRow) []usageTrendPoint {
	out := make([]usageTrendPoint, len(daily))
	for i, r := range daily {
		out[i] = usageTrendPoint{Date: r.Date, Total: r.Total}
	}
	return out
}

// fillDailyRange 补齐 [from, to] 内缺失的日期（total 全 0 的行）。
//
// model_usage_daily 只在有请求的日子写行；前端趋势图要按所选窗口逐日渲染，
// 因此这里把空白的日子显式补成 0 值，保证点位数量与日历天数一致。
//
// 入参 daily 必须已按日期升序；返回值同样升序。日期解析失败时原样返回。
func fillDailyRange(daily []usageDailyRow, from, to string) []usageDailyRow {
	start, err1 := time.Parse("2006-01-02", from)
	end, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || start.After(end) {
		return daily
	}

	byDate := make(map[string]usageDailyRow, len(daily))
	for _, r := range daily {
		byDate[r.Date] = r
	}

	total := int(end.Sub(start).Hours()/24) + 1
	out := make([]usageDailyRow, 0, total)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if row, ok := byDate[key]; ok {
			out = append(out, row)
			continue
		}
		out = append(out, usageDailyRow{Date: key})
	}
	return out
}

// providerDisplayNames 返回 provider_id → 供应商名称的映射。
//
// 统计表只记录 provider_id，页面需要展示可读名称；供应商被删除后 id 仍会
// 留在历史统计里，缺失的名字由 usageProviderName 回退成 id 本身。
func (s *Server) providerDisplayNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	records, err := s.store.ListProviders(ctx)
	if err != nil {
		s.logger.Warn("读取供应商名称失败，使用 id 展示", "error", err)
		return out
	}
	for _, rec := range records {
		out[rec.ID] = rec.Name
	}
	return out
}

// usageProviderName 把 provider_id 解析为展示名；未知 id 原样返回。
func usageProviderName(names map[string]string, providerID string) string {
	if name, ok := names[providerID]; ok && strings.TrimSpace(name) != "" {
		return name
	}
	return providerID
}

// earliestUsageDate 返回 model_usage_daily 中最早的一天；无数据时返回空串。
func (s *Server) earliestUsageDate(ctx context.Context) string {
	var first sql.NullString
	if err := s.store.DB().QueryRowContext(ctx,
		`SELECT MIN(date) FROM model_usage_daily`).Scan(&first); err != nil {
		return ""
	}
	if !first.Valid {
		return ""
	}
	return first.String
}

// firstUsedAndActiveDays 返回窗口内的 first_used_at 与 active_days。
//
// first_used_at 优先取窗口内最早一天；若窗口内为空（罕见），fallback 到
// 整个 model_usage_daily 表的 MIN(date)。active_days = total > 0 的天数。
func (s *Server) firstUsedAndActiveDays(ctx context.Context, daily []usageDailyRow, today string) (string, int) {
	firstUsed := ""
	if len(daily) > 0 {
		firstUsed = daily[0].Date
	}
	active := 0
	for _, r := range daily {
		if r.Total > 0 {
			active++
		}
	}
	if firstUsed == "" {
		var dbFirst sql.NullString
		if err := s.store.DB().QueryRowContext(ctx,
			`SELECT MIN(date) FROM model_usage_daily`).Scan(&dbFirst); err == nil && dbFirst.Valid {
			firstUsed = dbFirst.String
		}
	}
	return firstUsed, active
}

// shiftDays 把 YYYY-MM-DD 日期字符串平移 days 天（负数=往前）。
func shiftDays(date string, days int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}
