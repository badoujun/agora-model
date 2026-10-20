package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// createMultiKeyProvider 通过 API 创建一个带多个 Key 的供应商，返回 id 与响应体。
func createMultiKeyProvider(t *testing.T, h *harness, client *http.Client, name string, keys []map[string]any) (string, map[string]any) {
	t.Helper()
	code, body, raw := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name":            name,
		"openai_base_url": h.upstreamURL + "/v1",
		"keys":            keys,
		"models_selected": []string{"seed-model"},
		"allow_internal":  true,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建多 Key 供应商状态码 = %d（%s）", code, raw)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("响应缺少 id: %s", raw)
	}
	return id, body
}

func TestProviderMultiKeyCreateAndList(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
		{"label": "备用", "api_key": "sk-backup-2222bbbb", "enabled": true},
	})
	if strings.Contains(string(mustJSON(t, body)), "sk-main-1111aaaa") {
		t.Fatal("响应中不得出现明文凭证")
	}

	keys, _ := body["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
	first := keys[0].(map[string]any)
	if first["id"] == "" || first["label"] != "主账号" || first["api_key_hint"] != "sk-****aaaa" {
		t.Errorf("首个 Key = %v", first)
	}
	if first["auto_disabled"] != false {
		t.Errorf("新 Key 不应是自动停用状态: %v", first)
	}
	// 兼容字段取首个启用 Key 的掩码
	if body["api_key_hint"] != "sk-****aaaa" {
		t.Errorf("api_key_hint = %v", body["api_key_hint"])
	}
	if _, ok := body["key_stats"]; !ok {
		t.Error("响应应包含 key_stats")
	}

	// GET 详情同样只回掩码
	code, detail, raw := h.do(client, http.MethodGet, "/api/providers/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("详情状态码 = %d（%s）", code, raw)
	}
	if strings.Contains(raw, "sk-main-1111aaaa") || strings.Contains(raw, "sk-backup-2222bbbb") {
		t.Fatal("GET 详情不得出现明文")
	}
	if len(detail["keys"].([]any)) != 2 {
		t.Errorf("详情 keys = %v", detail["keys"])
	}

	// 落库校验：两个 Key 都应可解密
	providers, err := h.store.LoadProviders(context.Background(), h.master)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("DB 中 Key 数 = %d", len(got.Keys))
	}
	for _, p := range providers {
		if p.ID == id {
			if len(p.APIKeys) != 2 || p.APIKeys[0].Key != "sk-main-1111aaaa" {
				t.Errorf("解密后的 Key = %+v", p.APIKeys)
			}
			if p.APIKeys[0].ID != got.Keys[0].ID {
				t.Errorf("Key id 未透传到运行时配置: %q vs %q", p.APIKeys[0].ID, got.Keys[0].ID)
			}
		}
	}
}

func TestProviderMultiKeyUpdateKeepsAndRemoves(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
		{"label": "备用", "api_key": "sk-backup-2222bbbb", "enabled": true},
	})
	keys := body["keys"].([]any)
	mainID := keys[0].(map[string]any)["id"].(string)
	backupID := keys[1].(map[string]any)["id"].(string)

	// 保留主账号（不传明文）+ 改备注、新增第三个 Key、删除备用
	code, updated, raw := h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{
		"keys": []map[string]any{
			{"id": mainID, "label": "主账号-改名", "enabled": true},
			{"label": "新Key", "api_key": "sk-new-3333cccc", "enabled": true},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("更新状态码 = %d（%s）", code, raw)
	}
	gotKeys, _ := updated["keys"].([]any)
	if len(gotKeys) != 2 {
		t.Fatalf("更新后 Key 数 = %d（%v）", len(gotKeys), gotKeys)
	}
	if gotKeys[0].(map[string]any)["id"] != mainID || gotKeys[0].(map[string]any)["label"] != "主账号-改名" {
		t.Errorf("既有 Key 应保留 id 并更新备注: %v", gotKeys[0])
	}
	if gotKeys[0].(map[string]any)["api_key_hint"] != "sk-****aaaa" {
		t.Errorf("未传明文时应沿用原密文与掩码: %v", gotKeys[0])
	}
	for _, k := range gotKeys {
		if k.(map[string]any)["id"] == backupID {
			t.Fatal("未出现在列表中的 Key 应被删除")
		}
	}

	// 明文仍然可用（换成新 Key 之后解密值应更新）
	providers, err := h.store.LoadProviders(context.Background(), h.master)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.ID != id {
			continue
		}
		if len(p.APIKeys) != 2 {
			t.Fatalf("运行时 Key 数 = %d", len(p.APIKeys))
		}
		if p.APIKeys[0].Key != "sk-main-1111aaaa" {
			t.Errorf("保留的 Key 明文 = %q", p.APIKeys[0].Key)
		}
		if p.APIKeys[1].Key != "sk-new-3333cccc" {
			t.Errorf("新增的 Key 明文 = %q", p.APIKeys[1].Key)
		}
	}

	// 仅改名称、不传 keys：凭证集合保持不变
	code, _, raw = h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{"name": "多Key-改名"})
	if code != http.StatusOK {
		t.Fatalf("只改名称状态码 = %d（%s）", code, raw)
	}
	rec, err := h.store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Keys) != 2 {
		t.Fatalf("只改名称不应影响凭证，得到 %d 个", len(rec.Keys))
	}
}

func TestProviderMultiKeyValidation(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
	})
	keyID := body["keys"].([]any)[0].(map[string]any)["id"].(string)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"全部停用", map[string]any{"keys": []map[string]any{
			{"id": keyID, "label": "主账号", "enabled": false},
		}}, http.StatusBadRequest},
		{"空 Key 且无 id", map[string]any{"keys": []map[string]any{
			{"label": "x"},
		}}, http.StatusBadRequest},
		{"未知 Key id", map[string]any{"keys": []map[string]any{
			{"id": "pk_not-exist", "label": "x"},
		}}, http.StatusBadRequest},
		{"重复明文", map[string]any{"keys": []map[string]any{
			{"api_key": "sk-same-0000aaaa", "enabled": true},
			{"api_key": "sk-same-0000aaaa", "enabled": true},
		}}, http.StatusBadRequest},
		{"空数组", map[string]any{"keys": []map[string]any{}}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := h.do(client, http.MethodPut, "/api/providers/"+id, tc.body)
			if code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d（%s）", code, tc.want, raw)
			}
		})
	}

	// 创建时没有任何 Key 应被拒绝
	code, _, _ := h.do(client, http.MethodPost, "/api/providers", map[string]any{
		"name": "无Key", "openai_base_url": h.upstreamURL + "/v1",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("无凭证创建状态码 = %d，期望 400", code)
	}
}

func TestProviderKeyAutoDisabledVisibleAndReEnable(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
		{"label": "备用", "api_key": "sk-backup-2222bbbb", "enabled": true},
	})
	keys := body["keys"].([]any)
	mainID := keys[0].(map[string]any)["id"].(string)

	// 模拟网关自动停用（数据面写库的路径）
	if err := h.store.DisableProviderKey(context.Background(), mainID, "HTTP 401: invalid key"); err != nil {
		t.Fatal(err)
	}
	code, detail, raw := h.do(client, http.MethodGet, "/api/providers/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("详情状态码 = %d（%s）", code, raw)
	}
	first := detail["keys"].([]any)[0].(map[string]any)
	if first["enabled"] != false || first["auto_disabled"] != true {
		t.Fatalf("应显示为自动停用: %v", first)
	}
	if !strings.Contains(first["disabled_reason"].(string), "401") {
		t.Errorf("停用原因 = %v", first["disabled_reason"])
	}
	stats := detail["key_stats"].(map[string]any)[mainID].(map[string]any)
	if stats["failure_count"].(float64) < 1 {
		t.Errorf("失败计数 = %v", stats["failure_count"])
	}
	// 兼容字段应指向仍然启用的那个 Key
	if detail["api_key_hint"] != "sk-****bbbb" {
		t.Errorf("api_key_hint 应取首个启用 Key，得到 %v", detail["api_key_hint"])
	}

	// 用户在界面上重新启用（保存 keys 并把 enabled 置真）
	code, updated, raw := h.do(client, http.MethodPut, "/api/providers/"+id, map[string]any{
		"keys": []map[string]any{
			{"id": mainID, "label": "主账号", "enabled": true},
			{"id": keys[1].(map[string]any)["id"], "label": "备用", "enabled": true},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("重新启用状态码 = %d（%s）", code, raw)
	}
	reEnabled := updated["keys"].([]any)[0].(map[string]any)
	if reEnabled["enabled"] != true || reEnabled["auto_disabled"] != false || reEnabled["disabled_reason"] != nil {
		t.Errorf("重新启用后应清空自动停用标记: %v", reEnabled)
	}
}

func TestTestSingleKeyEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
	})
	keyID := body["keys"].([]any)[0].(map[string]any)["id"].(string)

	code, result, raw := h.do(client, http.MethodPost, "/api/providers/"+id+"/keys/"+keyID+"/test", nil)
	if code != http.StatusOK {
		t.Fatalf("单 Key 测试状态码 = %d（%s）", code, raw)
	}
	if result["ok"] != true {
		t.Fatalf("单 Key 测试应通过: %v", result)
	}
	keys, _ := result["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["key_id"] != keyID {
		t.Errorf("结果应只含该 Key: %v", keys)
	}

	if code, _, _ := h.do(client, http.MethodPost, "/api/providers/"+id+"/keys/pk_nope/test", nil); code != http.StatusNotFound {
		t.Errorf("未知 Key 状态码 = %d，期望 404", code)
	}
	// 停用的 Key 仍可测试（用于判断能否重新启用）
	if err := h.store.DisableProviderKey(context.Background(), keyID, "手动测试"); err != nil {
		t.Fatal(err)
	}
	code, _, raw = h.do(client, http.MethodPost, "/api/providers/"+id+"/keys/"+keyID+"/test", nil)
	if code != http.StatusOK {
		t.Fatalf("已停用 Key 的测试状态码 = %d（%s）", code, raw)
	}
}

func TestTestDraftKeyEndpoint(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	draft := map[string]any{
		"openai_base_url": h.upstreamURL + "/v1",
		"keys": []map[string]any{
			{"label": "第一个", "api_key": "sk-aaaa-0000zzzz"},
			{"label": "第二个", "api_key": "sk-bbbb-1111yyyy"},
		},
		"allow_internal":  true,
		"models_selected": []string{"seed-model"},
	}

	// 上游全部失败：应逐个尝试并返回两个 Key 的结果
	h.upstream.setFail(true)
	code, result, raw := h.do(client, http.MethodPost, "/api/providers/test-key", draft)
	if code != http.StatusOK {
		t.Fatalf("草稿测试状态码 = %d（%s）", code, raw)
	}
	if result["ok"] != false {
		t.Fatalf("上游全部失败时 ok 应为 false: %v", result)
	}
	keys, _ := result["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("应返回两个 Key 的探测结果: %v", keys)
	}
	if keys[0].(map[string]any)["hint"] != "sk-****zzzz" || keys[1].(map[string]any)["hint"] != "sk-****yyyy" {
		t.Errorf("Key 掩码 = %v", keys)
	}

	// 上游恢复：首个 Key 即成功
	h.upstream.setFail(false)
	code, result, raw = h.do(client, http.MethodPost, "/api/providers/test-key", draft)
	if code != http.StatusOK || result["ok"] != true {
		t.Fatalf("草稿测试应通过: code=%d %s", code, raw)
	}

	// 缺少 Key 应 400
	code, _, _ = h.do(client, http.MethodPost, "/api/providers/test-key", map[string]any{
		"openai_base_url": h.upstreamURL + "/v1",
	})
	if code != http.StatusBadRequest {
		t.Errorf("缺少 Key 的状态码 = %d，期望 400", code)
	}
}

func TestProviderHintAndStatsAfterFailedRequests(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	id, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
	})
	keyID := body["keys"].([]any)[0].(map[string]any)["id"].(string)

	if err := h.store.RecordProviderKeyFailure(context.Background(), keyID, "HTTP 429: slow down"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.TouchProviderKey(context.Background(), keyID); err != nil {
		t.Fatal(err)
	}
	code, detail, raw := h.do(client, http.MethodGet, "/api/providers/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("详情状态码 = %d（%s）", code, raw)
	}
	stats := detail["key_stats"].(map[string]any)[keyID].(map[string]any)
	if stats["failure_count"].(float64) != 1 {
		t.Errorf("failure_count = %v", stats["failure_count"])
	}
	if stats["last_error"] != "HTTP 429: slow down" {
		t.Errorf("last_error = %v", stats["last_error"])
	}
	if stats["last_used_at"] == "" {
		t.Errorf("last_used_at 应被填充: %v", stats)
	}
}

func TestExportIncludesAllKeysAndLegacyField(t *testing.T) {
	h := newHarness(t, "")
	client := newClient(t)

	_, body := createMultiKeyProvider(t, h, client, "多Key", []map[string]any{
		{"label": "主账号", "api_key": "sk-main-1111aaaa", "enabled": true},
		{"label": "备用", "api_key": "sk-backup-2222bbbb", "enabled": false},
	})
	discoveredID := ""
	if keys, ok := body["keys"].([]any); ok && len(keys) == 2 {
		discoveredID = keys[0].(map[string]any)["id"].(string)
	}
	if discoveredID == "" {
		t.Fatalf("创建响应异常: %v", body)
	}

	code, exported, raw := h.do(client, http.MethodGet, "/api/export", nil)
	if code != http.StatusOK {
		t.Fatalf("导出状态码 = %d（%s）", code, raw)
	}
	providers := exported["providers"].([]any)
	var target map[string]any
	for _, raw2 := range providers {
		p := raw2.(map[string]any)
		if p["id"] == body["id"] {
			target = p
		}
	}
	if target == nil {
		t.Fatalf("导出缺少目标供应商: %v", providers)
	}
	exportedKeys, _ := target["keys"].([]any)
	if len(exportedKeys) != 2 {
		t.Fatalf("导出的 keys = %v", exportedKeys)
	}
	k0 := exportedKeys[0].(map[string]any)
	if k0["label"] != "主账号" || k0["api_key"] != "sk-main-1111aaaa" || k0["enabled"] != true {
		t.Errorf("导出的首个 Key = %v", k0)
	}
	k1 := exportedKeys[1].(map[string]any)
	if k1["api_key"] != "sk-backup-2222bbbb" || k1["enabled"] != false {
		t.Errorf("停用的 Key 也应导出并保留 enabled=false: %v", k1)
	}
	if target["api_key"] != "sk-main-1111aaaa" {
		t.Errorf("兼容字段应为首个启用 Key 的明文: %v", target["api_key"])
	}
	// Key id 属于易变/内部信息，不应出现在导出文件里
	for _, k := range exportedKeys {
		if _, has := k.(map[string]any)["id"]; has {
			t.Error("导出不应包含 Key id")
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
