package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agora-model/internal/logging"
	"agora-model/internal/store"
)

// TestGatewayKeysCRUD 覆盖多网关 Key 的完整生命周期：
// 新建 → 列表 → 查看明文 → 更新别名/备注/范围 → 停用 → 软删除 → 409 保护。
func TestGatewayKeysCRUD(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	// 先建一个默认 Key（模拟运行期 EnsureGatewayKey）
	if code, _, raw := h.do(client, http.MethodPost, "/api/gateway-key/reset", nil); code != http.StatusOK {
		t.Fatalf("初始化网关 Key 失败: %d（%s）", code, raw)
	}

	// 新建：返回一次性明文
	code, created, raw := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{
		"name": "Claude Code",
		"note": "张三的笔记本",
		"scopes": []map[string]any{
			{"provider_id": "seed", "model": "seed-model"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("新建状态码 = %d（%s）", code, raw)
	}
	keyObj, _ := created["key"].(map[string]any)
	keyID, _ := keyObj["id"].(string)
	plain, _ := created["gateway_key"].(string)
	if keyID == "" || plain == "" {
		t.Fatalf("新建响应缺少 id 或明文: %s", raw)
	}
	if plain == "" || keyObj["key_hint"] == plain {
		t.Error("响应中只应有掩码，不应重复回传明文")
	}
	if keyObj["note"] != "张三的笔记本" || keyObj["enabled"] != true {
		t.Errorf("新建结果 = %v", keyObj)
	}
	scopes, _ := keyObj["scopes"].([]any)
	if len(scopes) != 1 {
		t.Errorf("模型范围应被保存: %v", keyObj["scopes"])
	}

	// 列表：掩码、不含明文
	code, list, raw := h.do(client, http.MethodGet, "/api/gateway-keys", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", code)
	}
	if len(list["items"].([]any)) < 2 {
		t.Errorf("列表应包含默认 Key 与新建 Key: %v", list["items"])
	}
	if strings.Contains(raw, plain) {
		t.Fatal("列表不得包含明文")
	}

	// 查看明文
	code, revealed, raw := h.do(client, http.MethodGet, "/api/gateway-keys/"+keyID+"/reveal", nil)
	if code != http.StatusOK {
		t.Fatalf("查看明文状态码 = %d（%s）", code, raw)
	}
	if revealed["gateway_key"] != plain {
		t.Errorf("明文不一致: %v", revealed["gateway_key"])
	}

	// 更新：只改备注（不传 name 表示沿用）
	code, updated, raw := h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{
		"note": "改用李四的机器",
	})
	if code != http.StatusOK {
		t.Fatalf("更新状态码 = %d（%s）", code, raw)
	}
	got, _ := updated["key"].(map[string]any)
	if got["name"] != "Claude Code" || got["note"] != "改用李四的机器" {
		t.Errorf("更新后 = %v", got)
	}

	// 清空范围 → 变成不限制
	code, updated, raw = h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{
		"scopes": []map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("清空范围状态码 = %d（%s）", code, raw)
	}
	if got, _ := updated["key"].(map[string]any); len(got["scopes"].([]any)) != 0 {
		t.Errorf("清空后范围应为空: %v", got["scopes"])
	}

	// 非法范围：指向不存在的模型 → 400
	code, _, raw = h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{
		"scopes": []map[string]any{{"provider_id": "seed", "model": "not-selected"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("失效范围应返回 400，得到 %d（%s）", code, raw)
	}

	// 空别名 → 400
	code, _, _ = h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{"name": "  "})
	if code != http.StatusBadRequest {
		t.Errorf("空别名应返回 400，得到 %d", code)
	}

	// 软删除
	code, _, raw = h.do(client, http.MethodDelete, "/api/gateway-keys/"+keyID, nil)
	if code != http.StatusOK {
		t.Fatalf("删除状态码 = %d（%s）", code, raw)
	}
	// 默认列表不再包含它，include_deleted 时包含
	_, list, _ = h.do(client, http.MethodGet, "/api/gateway-keys", nil)
	for _, item := range list["items"].([]any) {
		if item.(map[string]any)["id"] == keyID {
			t.Fatal("已删除的 Key 不应出现在默认列表里")
		}
	}
	_, list, _ = h.do(client, http.MethodGet, "/api/gateway-keys?include_deleted=1", nil)
	found := false
	for _, item := range list["items"].([]any) {
		obj := item.(map[string]any)
		if obj["id"] == keyID {
			found = true
			if obj["deleted"] != true || obj["deleted_at"] == "" {
				t.Errorf("已删除项应标记 deleted/deleted_at: %v", obj)
			}
			if obj["name"] != "Claude Code" {
				t.Errorf("已删除项仍应保留名称（历史可读）: %v", obj)
			}
		}
	}
	if !found {
		t.Fatal("include_deleted=1 应返回已删除的 Key")
	}

	// 已删除的 Key 不能重置 / 查看的仍是明文（历史排障），重置应 404
	if code, _, _ := h.do(client, http.MethodPost, "/api/gateway-keys/"+keyID+"/reset", nil); code != http.StatusNotFound {
		t.Errorf("已删除 Key 的重置状态码 = %d，期望 404", code)
	}
}

// TestGatewayKeyLastEnabledProtection 覆盖「不能停用/删除最后一个启用 Key」→ 409。
func TestGatewayKeyLastEnabledProtection(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, body, raw := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "唯一"})
	if code != http.StatusCreated {
		t.Fatalf("新建状态码 = %d（%s）", code, raw)
	}
	keyID := body["key"].(map[string]any)["id"].(string)

	// 此时只有一个 Key（harness 没自动生成），停用应 409
	if code, _, raw := h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{"enabled": false}); code != http.StatusConflict {
		t.Fatalf("停用唯一 Key 状态码 = %d（%s），期望 409", code, raw)
	}
	if code, _, raw := h.do(client, http.MethodDelete, "/api/gateway-keys/"+keyID, nil); code != http.StatusConflict {
		t.Fatalf("删除唯一 Key 状态码 = %d（%s），期望 409", code, raw)
	}

	// 建第二个后即可停用第一个
	_, second, _ := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "第二个"})
	secondID := second["key"].(map[string]any)["id"].(string)
	if code, _, raw := h.do(client, http.MethodPut, "/api/gateway-keys/"+keyID, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("非最后一个 Key 应允许停用: %d（%s）", code, raw)
	}
	// 此时 second 是最后一个启用者
	if code, _, _ := h.do(client, http.MethodDelete, "/api/gateway-keys/"+secondID, nil); code != http.StatusConflict {
		t.Errorf("删除最后一个启用 Key 应 409，得到 %d", code)
	}
}

// TestLogsFilterByGatewayKeyAPI 覆盖日志按网关 Key 筛选且 total 与列表一致。
func TestLogsFilterByGatewayKeyAPI(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	_, a, raw := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "A"})
	if raw == "" {
		t.Fatal("创建 Key A 失败")
	}
	_, b, _ := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "B"})
	idA := a["key"].(map[string]any)["id"].(string)
	idB := b["key"].(map[string]any)["id"].(string)

	if err := h.store.InsertLogs(context.Background(), []logging.Entry{
		apiTestLog("r1", idA), apiTestLog("r2", idA), apiTestLog("r3", idB),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path      string
		wantItems int
		wantTotal float64
	}{
		{"/api/logs", 3, 3},
		{"/api/logs?gateway_key_id=" + idA, 2, 2},
		{"/api/logs?gateway_key_id=" + idB, 1, 1},
	} {
		code, body, raw := h.do(client, http.MethodGet, tc.path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d（%s）", tc.path, code, raw)
		}
		items, _ := body["items"].([]any)
		if len(items) != tc.wantItems {
			t.Errorf("%s 返回 %d 条，期望 %d", tc.path, len(items), tc.wantItems)
		}
		if body["total"].(float64) != tc.wantTotal {
			t.Errorf("%s total = %v，期望 %v", tc.path, body["total"], tc.wantTotal)
		}
	}

	// 日志条目应带上网关 Key 的展示名
	_, body, _ := h.do(client, http.MethodGet, "/api/logs?gateway_key_id="+idA, nil)
	first := body["items"].([]any)[0].(map[string]any)
	if first["gateway_key_name"] != "A" {
		t.Errorf("gateway_key_name = %v，期望 A", first["gateway_key_name"])
	}
}

// TestUsageFilterByGatewayKeyAPI 覆盖使用统计按 Key 过滤：
// 单个 Key 的用量之和应等于全量，且单 Key 的各项都不超过全量。
func TestUsageFilterByGatewayKeyAPI(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	_, a, _ := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "A"})
	_, b, _ := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{"name": "B"})
	idA := a["key"].(map[string]any)["id"].(string)
	idB := b["key"].(map[string]any)["id"].(string)

	ctx := context.Background()
	if err := h.store.UpsertUsageDaily(ctx, []store.DailyBucket{
		{Date: "2026-10-01", GatewayKeyID: idA, ProviderID: "seed", Model: "seed-model", Requests: 1, InputTokens: 100, OutputTokens: 50},
		{Date: "2026-10-01", GatewayKeyID: idB, ProviderID: "seed", Model: "seed-model", Requests: 1, InputTokens: 20, OutputTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}

	read := func(path string) map[string]any {
		code, body, raw := h.do(client, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d（%s）", path, code, raw)
		}
		return body
	}

	all := read("/api/usage?from=2026-10-01&to=2026-10-02")
	onlyA := read("/api/usage?from=2026-10-01&to=2026-10-02&gateway_key_id=" + idA)
	onlyB := read("/api/usage?from=2026-10-01&to=2026-10-02&gateway_key_id=" + idB)

	if onlyA["gateway_key_id"] != idA || all["gateway_key_id"] != "" {
		t.Errorf("gateway_key_id 回显异常: all=%v A=%v", all["gateway_key_id"], onlyA["gateway_key_id"])
	}

	dailyTotal := func(body map[string]any) float64 {
		var sum float64
		for _, row := range body["daily"].([]any) {
			sum += row.(map[string]any)["total"].(float64)
		}
		return sum
	}
	totalAll, totalA, totalB := dailyTotal(all), dailyTotal(onlyA), dailyTotal(onlyB)
	if totalAll != 180 {
		t.Errorf("全量 token = %v，期望 180", totalAll)
	}
	if totalA != 150 || totalB != 30 {
		t.Errorf("按 Key 统计 = A:%v B:%v，期望 A:150 B:30", totalA, totalB)
	}
	if totalA > totalAll || totalB > totalAll {
		t.Error("单 Key 的统计不应超过全量")
	}

	// 模型占比、热力图、趋势都必须跟随筛选
	if models := onlyA["models"].([]any); len(models) != 1 || models[0].(map[string]any)["total"].(float64) != 150 {
		t.Errorf("单 Key 模型占比 = %v", onlyA["models"])
	}
	if len(onlyA["heatmap"].([]any)) != 84 {
		t.Errorf("热力图格数 = %d，期望 84", len(onlyA["heatmap"].([]any)))
	}
	var heatSum float64
	for _, cell := range onlyA["heatmap"].([]any) {
		heatSum += cell.(map[string]any)["total"].(float64)
	}
	if heatSum != 150 {
		t.Errorf("单 Key 热力图合计 = %v，期望 150", heatSum)
	}
	if kpi := onlyA["kpi"].(map[string]any); kpi["month30_total"].(float64) != 150 {
		t.Errorf("单 Key KPI = %v", kpi)
	}

	// 指定不存在的 Key：全部为 0，而不是报错
	empty := read("/api/usage?from=2026-10-01&to=2026-10-02&gateway_key_id=gk_missing")
	if dailyTotal(empty) != 0 {
		t.Errorf("未知 Key 应统计为 0，得到 %v", dailyTotal(empty))
	}
}

// apiTestLog 构造一条归属指定网关 Key 的请求日志。
func apiTestLog(requestID, gatewayKeyID string) logging.Entry {
	return logging.Entry{
		TS:              time.Now().UTC(),
		RequestID:       requestID,
		InboundProtocol: "openai",
		Model:           "seed-model",
		ProviderID:      "seed",
		StatusCode:      200,
		GatewayKeyID:    gatewayKeyID,
	}
}

// TestGatewayKeysUsageTotals 验证列表里每个 Key 带累计 token 用量，
// 且「未命中缓存 = 输入总量 - 命中缓存」的口径与使用统计页一致。
func TestGatewayKeysUsageTotals(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	code, created, raw := h.do(client, http.MethodPost, "/api/gateway-keys", map[string]any{
		"name": "统计测试",
	})
	if code != http.StatusCreated {
		t.Fatalf("新建 Key 失败: %d（%s）", code, raw)
	}
	keyObj, _ := created["key"].(map[string]any)
	keyID, _ := keyObj["id"].(string)
	if keyID == "" {
		t.Fatalf("新建响应缺少 id: %s", raw)
	}

	// 两个日期、不同模型：验证跨日/跨模型汇总
	if err := h.store.UpsertUsageDaily(context.Background(), []store.DailyBucket{
		{Date: "2026-10-19", GatewayKeyID: keyID, ProviderID: "seed", Model: "seed-model",
			Requests: 1, InputTokens: 100, CachedInputTokens: 30, OutputTokens: 50},
		{Date: "2026-10-20", GatewayKeyID: keyID, ProviderID: "seed", Model: "extra-model",
			Requests: 1, InputTokens: 200, CachedInputTokens: 80, OutputTokens: 20},
	}); err != nil {
		t.Fatalf("写入用量: %v", err)
	}

	code, list, raw := h.do(client, http.MethodGet, "/api/gateway-keys", nil)
	if code != http.StatusOK {
		t.Fatalf("列表状态码 = %d（%s）", code, raw)
	}

	var found map[string]any
	for _, item := range list["items"].([]any) {
		obj := item.(map[string]any)
		if obj["id"] == keyID {
			found = obj
			break
		}
	}
	if found == nil {
		t.Fatalf("列表里找不到新建的 Key: %s", raw)
	}

	usage, _ := found["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("列表项缺少 usage: %v", found)
	}
	// 输入 300（命中 110 + 未命中 190）+ 输出 70 = 370
	for field, want := range map[string]float64{
		"total_tokens":   370,
		"input_cached":   110,
		"input_uncached": 190,
		"output":         70,
	} {
		if got := usage[field].(float64); got != want {
			t.Errorf("usage.%s = %v, want %v", field, got, want)
		}
	}
}
